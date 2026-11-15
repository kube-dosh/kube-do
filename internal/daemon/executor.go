package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kube-dosh/kube-do/internal/check"
	"github.com/kube-dosh/kube-do/internal/cluster"
	"github.com/kube-dosh/kube-do/internal/download"
	"github.com/kube-dosh/kube-do/internal/install"
	"github.com/kube-dosh/kube-do/internal/layout"
	"github.com/kube-dosh/kube-do/internal/manifest"
	"github.com/kube-dosh/kube-do/internal/registry"
	"github.com/kube-dosh/kube-do/internal/render"
	"github.com/kube-dosh/kube-do/manifests"
)

type Executor interface {
	Install(ctx context.Context, cfg *cluster.Config, req InstallRequest, workDir string, log io.Writer) error
	AddWorkers(ctx context.Context, cfg *cluster.Config, newWorkers []cluster.Node, workDir string, log io.Writer) error
}

// HostExecutor runs the existing kube-do resource and Ansible pipeline.
type HostExecutor struct {
	CacheDir     string
	AnsibleDir   string
	ManifestPath string
}

func (e *HostExecutor) Install(ctx context.Context, cfg *cluster.Config, req InstallRequest, workDir string, log io.Writer) error {
	m, err := e.manifestFor(cfg)
	if err != nil {
		return err
	}
	if err := ensureHostPlatform(cfg); err != nil {
		return err
	}

	prepare := true
	if req.Prepare != nil {
		prepare = *req.Prepare
	}
	preflight := true
	if req.Check != nil {
		preflight = *req.Check
	}

	if prepare {
		logf(log, "==> downloading node binaries and building installer images")
		sum, err := download.Run(ctx, download.Options{
			CacheDir:        e.CacheDir,
			RegistryAddress: cfg.Registry.Address,
			Manifest:        m,
			OS:              cfg.Platform.ManifestOS(),
			Arch:            cfg.Platform.Arch,
			BinariesOnly:    true,
			CAFile:          cfg.RegistryCAPath(),
			CredentialsFile: cfg.Registry.CredentialsFile,
			Logf:            logFunc(log),
		})
		if err != nil {
			return fmt.Errorf("download binaries: %w", err)
		}
		logf(log, "downloaded=%d skipped=%d", sum.Downloaded, sum.Skipped)

		logf(log, "==> ensuring private registry")
		registryBinary, ok := m.BinaryFor("registry", cfg.Platform.ManifestOS(), cfg.Platform.Arch)
		if !ok {
			return fmt.Errorf("build %q has no registry installer binary for %s/%s", m.Metadata.Name, cfg.Platform.ManifestOS(), cfg.Platform.Arch)
		}
		if _, err := registry.Ensure(ctx, registry.Options{
			Address:       cfg.Registry.Address,
			DataDir:       cfg.Registry.DataDir,
			TLSMode:       cfg.Registry.TLS.Mode,
			CAFile:        cfg.Registry.TLS.CAFile,
			CertFile:      cfg.Registry.TLS.CertFile,
			KeyFile:       cfg.Registry.TLS.KeyFile,
			SourceArchive: layout.BinaryPath(e.CacheDir, m.Metadata.Name, registryBinary),
			Logf:          logFunc(log),
		}); err != nil {
			return fmt.Errorf("ensure registry: %w", err)
		}

		logf(log, "==> mirroring Kubernetes images")
		sum, err = download.Run(ctx, download.Options{
			CacheDir:        e.CacheDir,
			RegistryAddress: cfg.Registry.Address,
			Manifest:        m,
			OS:              cfg.Platform.ManifestOS(),
			Arch:            cfg.Platform.Arch,
			ImagesOnly:      true,
			CAFile:          cfg.RegistryCAPath(),
			CredentialsFile: cfg.Registry.CredentialsFile,
			Logf:            logFunc(log),
		})
		if err != nil {
			return fmt.Errorf("mirror images: %w", err)
		}
		logf(log, "built_images=%d mirrored=%d", sum.Built, sum.Mirrored)
	}

	if preflight {
		logf(log, "==> running preflight checks")
		results := check.Run(check.Options{
			Config:        cfg,
			Manifest:      m,
			CacheDir:      e.CacheDir,
			CheckRegistry: true,
		})
		for _, result := range results {
			logf(log, "[%-4s] %-10s %s", result.Status, result.Name, result.Detail)
		}
		if check.HasFailure(results) {
			return errors.New("preflight checks failed")
		}
	}

	logf(log, "==> rendering Ansible inventory")
	files, err := render.Render(cfg, m, workDir)
	if err != nil {
		return err
	}
	for _, file := range files {
		logf(log, "rendered %s", file)
	}

	logf(log, "==> installing cluster %s", cfg.Metadata.Name)
	return install.Run(ctx, install.Options{
		AnsibleDir: e.AnsibleDir,
		Playbook:   "site.yml",
		Inventory:  filepath.Join(workDir, "inventory.ini"),
		WorkingDir: e.AnsibleDir,
		ExtraVars:  req.ExtraVars,
		Tags:       req.Tags,
		Stdout:     log,
		Stderr:     log,
	})
}

func (e *HostExecutor) AddWorkers(ctx context.Context, cfg *cluster.Config, newWorkers []cluster.Node, workDir string, log io.Writer) error {
	if len(newWorkers) == 0 {
		return errors.New("at least one worker node is required")
	}
	m, err := e.manifestFor(cfg)
	if err != nil {
		return err
	}
	logf(log, "==> rendering Ansible inventory for new workers")
	files, err := render.RenderAddWorkers(cfg, newWorkers, m, workDir)
	if err != nil {
		return err
	}
	for _, file := range files {
		logf(log, "rendered %s", file)
	}
	logf(log, "==> joining %d worker(s) to cluster %s", len(newWorkers), cfg.Metadata.Name)
	return install.Run(ctx, install.Options{
		AnsibleDir: e.AnsibleDir,
		Playbook:   "add-worker.yml",
		Inventory:  filepath.Join(workDir, "inventory.ini"),
		WorkingDir: e.AnsibleDir,
		Stdout:     log,
		Stderr:     log,
	})
}

func (e *HostExecutor) manifestFor(cfg *cluster.Config) (*manifest.Manifest, error) {
	if cfg == nil {
		return nil, errors.New("cluster config is nil")
	}
	var (
		m   *manifest.Manifest
		err error
	)
	if e.ManifestPath != "" {
		m, err = manifest.LoadFile(e.ManifestPath)
	} else {
		m, err = manifests.Get(cfg.Build)
	}
	if err != nil {
		return nil, err
	}
	if m.Metadata.Name != cfg.Build {
		return nil, fmt.Errorf("build mismatch: cluster config pins %q but manifest is %q", cfg.Build, m.Metadata.Name)
	}
	return m, nil
}

func ensureHostPlatform(cfg *cluster.Config) error {
	if runtime.GOOS != cfg.Platform.ManifestOS() || runtime.GOARCH != cfg.Platform.Arch {
		return fmt.Errorf("installer host is %s/%s but target is %s/%s", runtime.GOOS, runtime.GOARCH, cfg.Platform.ManifestOS(), cfg.Platform.Arch)
	}
	return nil
}

func logFunc(w io.Writer) func(string, ...any) {
	return func(format string, args ...any) {
		fmt.Fprintf(w, format+"\n", args...)
	}
}

func logf(w io.Writer, format string, args ...any) {
	if !strings.HasSuffix(format, "\n") {
		format += "\n"
	}
	fmt.Fprintf(w, format, args...)
}

var _ Executor = (*HostExecutor)(nil)

func jobWorkDir(dataDir, jobID string) string {
	return filepath.Join(dataDir, "jobs", jobID)
}

func ensureWorkDir(dataDir, jobID string) (string, error) {
	dir := jobWorkDir(dataDir, jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create job work directory: %w", err)
	}
	return dir, nil
}
