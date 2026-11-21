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
	"time"

	"github.com/kube-dosh/kube-do/internal/check"
	"github.com/kube-dosh/kube-do/internal/cluster"
	"github.com/kube-dosh/kube-do/internal/download"
	"github.com/kube-dosh/kube-do/internal/install"
	"github.com/kube-dosh/kube-do/internal/layout"
	"github.com/kube-dosh/kube-do/internal/manifest"
	"github.com/kube-dosh/kube-do/internal/ops"
	"github.com/kube-dosh/kube-do/internal/registry"
	"github.com/kube-dosh/kube-do/internal/render"
	"github.com/kube-dosh/kube-do/manifests"
)

type Executor interface {
	Install(ctx context.Context, cfg *cluster.Config, req InstallRequest, workDir string, log io.Writer) error
	AddWorkers(ctx context.Context, cfg *cluster.Config, newWorkers []cluster.Node, workDir string, log io.Writer) error
	AddControlPlanes(ctx context.Context, cfg *cluster.Config, newNodes []cluster.Node, workDir, backupDir string, log io.Writer) (ops.BackupResult, error)
	RemoveNode(ctx context.Context, cfg *cluster.Config, nodeName, role, workDir, backupDir string, log io.Writer) (ops.RemoveNodeResult, error)
	Upgrade(ctx context.Context, cfg *cluster.Config, req UpgradeRequest, workDir, backupDir string, log io.Writer) (ops.UpgradeResult, error)
	Rollback(ctx context.Context, cfg *cluster.Config, req RollbackRequest, workDir, backupDir string, log io.Writer) (ops.RollbackResult, error)
	RestoreEtcd(ctx context.Context, cfg *cluster.Config, req EtcdRestoreRequest, workDir, backupDir string, log io.Writer) (ops.RestoreResult, error)
	Uninstall(ctx context.Context, cfg *cluster.Config, req UninstallRequest, workDir string, log io.Writer) (ops.UninstallResult, error)
	BackupEtcd(ctx context.Context, cfg *cluster.Config, workDir, backupDir string, opts ops.BackupOptions, log io.Writer) (ops.BackupResult, error)
	CheckCerts(ctx context.Context, cfg *cluster.Config, workDir string, opts ops.CertCheckOptions, log io.Writer) (ops.CertCheckResult, error)
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
		if err := e.prepareArtifacts(ctx, cfg, m, log); err != nil {
			return err
		}
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

func (e *HostExecutor) AddControlPlanes(ctx context.Context, cfg *cluster.Config, newNodes []cluster.Node, workDir, backupDir string, log io.Writer) (ops.BackupResult, error) {
	m, err := e.manifestFor(cfg)
	if err != nil {
		return ops.BackupResult{}, err
	}
	return ops.AddControlPlanes(ctx, cfg, m, e.AnsibleDir, workDir, backupDir, newNodes, ops.BackupOptions{}, log)
}

func (e *HostExecutor) RemoveNode(ctx context.Context, cfg *cluster.Config, nodeName, role, workDir, backupDir string, log io.Writer) (ops.RemoveNodeResult, error) {
	m, err := e.manifestFor(cfg)
	if err != nil {
		return ops.RemoveNodeResult{}, err
	}
	return ops.RemoveNode(ctx, cfg, m, e.AnsibleDir, workDir, backupDir, nodeName, role, ops.BackupOptions{}, log)
}

func (e *HostExecutor) Upgrade(ctx context.Context, cfg *cluster.Config, req UpgradeRequest, workDir, backupDir string, log io.Writer) (ops.UpgradeResult, error) {
	current, err := e.manifestFor(cfg)
	if err != nil {
		return ops.UpgradeResult{}, err
	}
	target, err := e.manifestForBuild(req.TargetBuild)
	if err != nil {
		return ops.UpgradeResult{}, err
	}
	if err := ops.ValidateUpgradeTarget(current, target); err != nil {
		return ops.UpgradeResult{}, err
	}
	targetCfg := *cfg
	targetCfg.Build = target.Metadata.Name
	if req.Prepare == nil || *req.Prepare {
		logf(log, "==> preparing target Build %s", target.Metadata.Name)
		if err := e.prepareArtifacts(ctx, &targetCfg, target, log); err != nil {
			return ops.UpgradeResult{}, err
		}
	}
	if req.Check == nil || *req.Check {
		logf(log, "==> running preflight checks for target Build %s", target.Metadata.Name)
		results := check.Run(check.Options{Config: &targetCfg, Manifest: target, CacheDir: e.CacheDir, CheckRegistry: true})
		for _, result := range results {
			logf(log, "[%-4s] %-10s %s", result.Status, result.Name, result.Detail)
		}
		if check.HasFailure(results) {
			return ops.UpgradeResult{}, errors.New("upgrade preflight checks failed")
		}
	}
	return ops.Upgrade(ctx, cfg, current, target, e.AnsibleDir, workDir, backupDir, ops.UpgradeOptions{
		Timeout:   ops.DefaultLifecycleTimeout,
		ExtraVars: req.ExtraVars,
	}, log)
}

func (e *HostExecutor) Rollback(ctx context.Context, cfg *cluster.Config, req RollbackRequest, workDir, backupDir string, log io.Writer) (ops.RollbackResult, error) {
	current, err := e.manifestFor(cfg)
	if err != nil {
		return ops.RollbackResult{}, err
	}
	target, err := e.manifestForBuild(req.TargetBuild)
	if err != nil {
		return ops.RollbackResult{}, err
	}
	targetCfg := *cfg
	targetCfg.Build = target.Metadata.Name
	if req.Prepare == nil || *req.Prepare {
		logf(log, "==> preparing rollback Build %s", target.Metadata.Name)
		if err := e.prepareArtifacts(ctx, &targetCfg, target, log); err != nil {
			return ops.RollbackResult{}, err
		}
	}
	return ops.Rollback(ctx, cfg, current, target, e.AnsibleDir, workDir, backupDir, req.SnapshotPath, req.SnapshotSHA256, req.SnapshotSize, ops.RollbackOptions{
		Timeout:   ops.DefaultLifecycleTimeout,
		ExtraVars: req.ExtraVars,
	}, log)
}

func (e *HostExecutor) RestoreEtcd(ctx context.Context, cfg *cluster.Config, req EtcdRestoreRequest, workDir, backupDir string, log io.Writer) (ops.RestoreResult, error) {
	m, err := e.manifestFor(cfg)
	if err != nil {
		return ops.RestoreResult{}, err
	}
	return ops.RestoreEtcd(ctx, cfg, m, e.AnsibleDir, workDir, backupDir, ops.RestoreOptions{
		Timeout:      ops.DefaultLifecycleTimeout,
		SnapshotPath: req.SnapshotPath,
		TargetNode:   req.TargetNode,
		Confirm:      req.Confirm,
		ExtraVars:    req.ExtraVars,
	}, log)
}

func (e *HostExecutor) Uninstall(ctx context.Context, cfg *cluster.Config, req UninstallRequest, workDir string, log io.Writer) (ops.UninstallResult, error) {
	m, err := e.manifestFor(cfg)
	if err != nil {
		return ops.UninstallResult{}, err
	}
	return ops.Uninstall(ctx, cfg, m, e.AnsibleDir, workDir, ops.UninstallOptions{
		Timeout:   ops.DefaultLifecycleTimeout,
		Confirm:   req.Confirm,
		ExtraVars: req.ExtraVars,
	}, log)
}

func (e *HostExecutor) BackupEtcd(ctx context.Context, cfg *cluster.Config, workDir, backupDir string, opts ops.BackupOptions, log io.Writer) (ops.BackupResult, error) {
	m, err := e.manifestFor(cfg)
	if err != nil {
		return ops.BackupResult{}, err
	}
	return ops.BackupEtcd(ctx, cfg, m, e.AnsibleDir, workDir, backupDir, opts, log)
}

func (e *HostExecutor) CheckCerts(ctx context.Context, cfg *cluster.Config, workDir string, opts ops.CertCheckOptions, log io.Writer) (ops.CertCheckResult, error) {
	m, err := e.manifestFor(cfg)
	if err != nil {
		return ops.CertCheckResult{}, err
	}
	return ops.CheckCerts(ctx, cfg, m, e.AnsibleDir, workDir, opts, log)
}

func (e *HostExecutor) manifestFor(cfg *cluster.Config) (*manifest.Manifest, error) {
	if cfg == nil {
		return nil, errors.New("cluster config is nil")
	}
	var (
		m   *manifest.Manifest
		err error
	)
	m, err = e.manifestForBuild(cfg.Build)
	if err != nil {
		return nil, err
	}
	if m.Metadata.Name != cfg.Build {
		return nil, fmt.Errorf("build mismatch: cluster config pins %q but manifest is %q", cfg.Build, m.Metadata.Name)
	}
	return m, nil
}

func (e *HostExecutor) manifestForBuild(build string) (*manifest.Manifest, error) {
	if e.ManifestPath != "" {
		m, err := manifest.LoadFile(e.ManifestPath)
		if err != nil {
			return nil, err
		}
		if m.Metadata.Name != build {
			return nil, fmt.Errorf("manifest at %s is for build %q, not %q", e.ManifestPath, m.Metadata.Name, build)
		}
		return m, nil
	}
	return manifests.Get(build)
}

func (e *HostExecutor) prepareArtifacts(ctx context.Context, cfg *cluster.Config, m *manifest.Manifest, log io.Writer) error {
	sum, err := retryArtifactPreparation(ctx, log, "download binaries", func() (download.Summary, error) {
		return download.Run(ctx, download.Options{
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
	sum, err = retryArtifactPreparation(ctx, log, "mirror images", func() (download.Summary, error) {
		return download.Run(ctx, download.Options{
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
	})
	if err != nil {
		return fmt.Errorf("mirror images: %w", err)
	}
	logf(log, "built_images=%d mirrored=%d", sum.Built, sum.Mirrored)
	return nil
}

func retryArtifactPreparation(ctx context.Context, log io.Writer, operation string, fn func() (download.Summary, error)) (download.Summary, error) {
	const attempts = 4
	var (
		sum download.Summary
		err error
	)
	for attempt := 1; attempt <= attempts; attempt++ {
		sum, err = fn()
		if err == nil {
			return sum, nil
		}
		if ctx.Err() != nil {
			return sum, ctx.Err()
		}
		if attempt == attempts {
			break
		}
		delay := time.Duration(attempt) * 10 * time.Second
		logf(log, "%s failed (attempt %d/%d): %v; retrying in %s", operation, attempt, attempts, err, delay)
		select {
		case <-ctx.Done():
			return sum, ctx.Err()
		case <-time.After(delay):
		}
	}
	return sum, err
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
