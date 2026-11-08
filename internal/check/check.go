// Package check implements `kube-do check`: local preflight validation.
package check

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/kube-dosh/kube-do/internal/cluster"
	"github.com/kube-dosh/kube-do/internal/layout"
	"github.com/kube-dosh/kube-do/internal/manifest"
	"github.com/kube-dosh/kube-do/internal/registry"
)

type Status string

const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

type Result struct {
	Name   string
	Status Status
	Detail string
}

type Options struct {
	Config        *cluster.Config
	Manifest      *manifest.Manifest
	CacheDir      string
	CheckRegistry bool
	Timeout       time.Duration
}

// Run performs the checks and returns one result per check.
func Run(opts Options) []Result {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	results := []Result{
		{Name: "config", Status: StatusOK, Detail: opts.Config.Metadata.Name},
		{Name: "manifest", Status: StatusOK, Detail: opts.Manifest.Metadata.Name},
	}
	if opts.Config.Build != opts.Manifest.Metadata.Name {
		results = append(results, Result{
			Name:   "build",
			Status: StatusFail,
			Detail: fmt.Sprintf("cluster config pins %q but manifest is %q", opts.Config.Build, opts.Manifest.Metadata.Name),
		})
	} else {
		results = append(results, Result{Name: "build", Status: StatusOK, Detail: opts.Config.Build})
	}
	results = append(results, checkPlatform(opts.Config, opts.Manifest))
	results = append(results, checkPlatforms(opts.Manifest))
	results = append(results, checkArtifacts(opts.CacheDir, opts.Manifest, opts.Config.Platform.ManifestOS(), opts.Config.Platform.Arch))
	if opts.CheckRegistry {
		results = append(results, checkRegistry(opts.Config, timeout))
	}
	return results
}

func checkPlatform(cfg *cluster.Config, m *manifest.Manifest) Result {
	os, arch := cfg.Platform.ManifestOS(), cfg.Platform.Arch
	if !m.SupportsPlatform(os, arch) {
		return Result{Name: "platform", Status: StatusFail, Detail: fmt.Sprintf("build %q does not provide %s/%s", m.Metadata.Name, os, arch)}
	}
	if runtime.GOOS != os || runtime.GOARCH != arch {
		return Result{Name: "platform", Status: StatusFail, Detail: fmt.Sprintf("target is %s/%s but Installer Host is %s/%s; built HA artifacts must match the target", os, arch, runtime.GOOS, runtime.GOARCH)}
	}
	return Result{Name: "platform", Status: StatusOK, Detail: fmt.Sprintf("%s/%s (%s)", os, arch, cfg.Platform.OSFamily)}
}

func checkPlatforms(m *manifest.Manifest) Result {
	platforms := m.SupportedPlatforms()
	names := make([]string, 0, len(platforms))
	for _, p := range platforms {
		names = append(names, p.OS+"/"+p.Arch)
	}
	return Result{Name: "platforms", Status: StatusOK, Detail: fmt.Sprintf("%d: %s", len(names), strings.Join(names, ", "))}
}

func checkArtifacts(cacheDir string, m *manifest.Manifest, platformOS, arch string) Result {
	binaries := make([]manifest.Binary, 0)
	binaries = append(binaries, m.BinariesFor(platformOS, arch)...)
	binaries = append(binaries, m.InstallerBinariesFor(platformOS, arch)...)
	if len(binaries) == 0 {
		return Result{Name: "artifacts", Status: StatusFail, Detail: fmt.Sprintf("build %q has no binaries for %s/%s", m.Metadata.Name, platformOS, arch)}
	}

	var missing, mismatched, unverified int
	for _, b := range binaries {
		path := layout.BinaryPath(cacheDir, m.Metadata.Name, b)
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			missing++
			continue
		} else if err != nil {
			mismatched++
			continue
		}
		want := b.SHA256
		if b.IsBuilt() {
			sidecar, err := os.ReadFile(layout.BinaryChecksumPath(cacheDir, m.Metadata.Name, b))
			if err != nil {
				unverified++
				continue
			}
			fields := strings.Fields(string(sidecar))
			if len(fields) == 0 {
				unverified++
				continue
			}
			want = fields[0]
		}
		sum, err := fileSHA256(path)
		if err != nil || !strings.EqualFold(sum, want) {
			mismatched++
		}
	}
	total := len(binaries)
	verified := total - missing - mismatched - unverified
	switch {
	case mismatched > 0:
		return Result{Name: "artifacts", Status: StatusFail, Detail: fmt.Sprintf("%d/%d verified, %d missing, %d unverified, %d checksum mismatch", verified, total, missing, unverified, mismatched)}
	case missing > 0:
		return Result{Name: "artifacts", Status: StatusWarn, Detail: fmt.Sprintf("%d/%d verified, %d missing, %d unverified (run 'kube-do download')", verified, total, missing, unverified)}
	case unverified > 0:
		return Result{Name: "artifacts", Status: StatusWarn, Detail: fmt.Sprintf("%d/%d verified, %d built without checksum sidecar (rerun 'kube-do download')", verified, total, unverified)}
	default:
		return Result{Name: "artifacts", Status: StatusOK, Detail: fmt.Sprintf("%d/%d verified", verified, total)}
	}
}

func checkRegistry(cfg *cluster.Config, timeout time.Duration) Result {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := registry.Probe(ctx, cfg.Registry.Address, cfg.RegistryCAPath(), timeout); err != nil {
		return Result{Name: "registry", Status: StatusFail, Detail: fmt.Sprintf("%s: %v", cfg.Registry.Address, err)}
	}
	return Result{Name: "registry", Status: StatusOK, Detail: cfg.Registry.Address}
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func HasFailure(results []Result) bool {
	for _, r := range results {
		if r.Status == StatusFail {
			return true
		}
	}
	return false
}

func HasWarning(results []Result) bool {
	for _, r := range results {
		if r.Status == StatusWarn {
			return true
		}
	}
	return false
}
