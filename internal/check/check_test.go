package check

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kube-dosh/kube-do/internal/cluster"
	"github.com/kube-dosh/kube-do/internal/layout"
	"github.com/kube-dosh/kube-do/internal/manifest"
)

func testInputs(t *testing.T) (*cluster.Config, *manifest.Manifest) {
	t.Helper()
	cfg, err := cluster.Parse([]byte(`apiVersion: kube-do.io/v1alpha1
kind: ClusterConfig
metadata:
  name: test
build: test-build
installer:
  address: 10.0.0.10
registry:
  address: 10.0.0.10:5000
controlPlane:
  vip: 10.0.0.100
  nodes:
    - name: cp-1
      address: 10.0.0.11
cni:
  name: cilium
  kubeProxyReplacement: true
`))
	if err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{
		Metadata: manifest.Metadata{Name: "test-build", KubernetesVersion: "v1.36.5"},
		Spec: manifest.Spec{
			Platforms: []manifest.Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}},
			Registry:  manifest.Registry{Namespace: "kube-do"},
			Installer: manifest.Installer{},
			Binaries: []manifest.Binary{
				{Name: "kubeadm", Version: "v1.36.5", OS: "linux", Arch: "amd64", URL: "https://example.com/kubeadm", SHA256: strings.Repeat("a", 64)},
				{Name: "kubelet", Version: "v1.36.5", OS: "linux", Arch: "amd64", URL: "https://example.com/kubelet", SHA256: strings.Repeat("b", 64)},
				{Name: "kubeadm", Version: "v1.36.5", OS: "linux", Arch: "arm64", URL: "https://example.com/kubeadm-arm64", SHA256: strings.Repeat("c", 64)},
				{Name: "kubelet", Version: "v1.36.5", OS: "linux", Arch: "arm64", URL: "https://example.com/kubelet-arm64", SHA256: strings.Repeat("d", 64)},
			},
			Images: []manifest.Image{{Name: "pause", Source: "registry.k8s.io/pause:3.10.2", Target: "pause:3.10.2"}},
		},
	}
	return cfg, m
}

func TestRunMissingArtifactsIsWarning(t *testing.T) {
	cfg, m := testInputs(t)
	cache := t.TempDir()
	results := Run(Options{Config: cfg, Manifest: m, CacheDir: cache})

	artifacts := find(results, "artifacts")
	if artifacts == nil || artifacts.Status != StatusWarn {
		t.Fatalf("artifacts result = %#v", artifacts)
	}
	if !strings.Contains(artifacts.Detail, "2 missing") {
		t.Fatalf("detail = %q", artifacts.Detail)
	}
	platform := find(results, "platform")
	if platform == nil || platform.Status != StatusOK || !strings.Contains(platform.Detail, "linux/amd64") {
		t.Fatalf("platform result = %#v", platform)
	}
	platforms := find(results, "platforms")
	if platforms == nil || platforms.Status != StatusOK || !strings.Contains(platforms.Detail, "linux/arm64") {
		t.Fatalf("platforms result = %#v", platforms)
	}
	if HasFailure(results) {
		t.Fatalf("missing artifacts must not fail by default: %#v", results)
	}
}

func TestRunVerifiesChecksums(t *testing.T) {
	cfg, m := testInputs(t)
	cache := t.TempDir()

	writeArtifact := func(b manifest.Binary, content string) {
		p := layout.BinaryPath(cache, m.Metadata.Name, b)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// "a"*64 is a valid-looking but wrong checksum for "kubeadm".
	writeArtifact(m.Spec.Binaries[0], "kubeadm")
	results := Run(Options{Config: cfg, Manifest: m, CacheDir: cache})
	artifacts := find(results, "artifacts")
	if artifacts == nil || artifacts.Status != StatusFail {
		t.Fatalf("artifacts result = %#v", artifacts)
	}
	if !HasFailure(results) {
		t.Fatalf("checksum mismatch must fail")
	}
}

func TestRunBuildMismatchFails(t *testing.T) {
	cfg, m := testInputs(t)
	cfg.Build = "other-build"
	results := Run(Options{Config: cfg, Manifest: m, CacheDir: t.TempDir()})
	if !HasFailure(results) {
		t.Fatalf("build mismatch must fail: %#v", results)
	}
}

func TestRunRejectsInstallerHostArchitectureMismatch(t *testing.T) {
	cfg, m := testInputs(t)
	if runtime.GOARCH == "arm64" {
		t.Skip("fixture assumes an amd64 Installer Host")
	}
	cfg.Platform.Arch = "arm64"
	results := Run(Options{Config: cfg, Manifest: m, CacheDir: t.TempDir()})
	platform := find(results, "platform")
	if platform == nil || platform.Status != StatusFail || !strings.Contains(platform.Detail, "Installer Host") {
		t.Fatalf("platform result = %#v", platform)
	}
}

func TestRunFailsWhenProvidedTLSFilesAreMissing(t *testing.T) {
	cfg, m := testInputs(t)
	cfg.Registry.TLS = cluster.TLS{
		Mode:     cluster.TLSModeProvided,
		CAFile:   "/missing/ca.crt",
		CertFile: "/missing/server.crt",
		KeyFile:  "/missing/server.key",
	}
	results := Run(Options{Config: cfg, Manifest: m, CacheDir: t.TempDir()})
	inputs := find(results, "inputs")
	if inputs == nil || inputs.Status != StatusFail || !strings.Contains(inputs.Detail, "registry.tls.caFile") {
		t.Fatalf("inputs result = %#v", inputs)
	}
}

func TestRunValidatesBuiltArtifactMetadata(t *testing.T) {
	cfg, m := testInputs(t)
	b := manifest.Binary{
		Name: "haproxy", Version: "3.2.26", OS: "linux", Arch: "amd64",
		URL: "https://example.com/haproxy.tar.gz", Archive: manifest.ArchiveTarGz,
		Build: manifest.BuildHAProxy, SourceSHA256: strings.Repeat("e", 64),
	}
	m.Spec.Binaries = nil
	m.Spec.Installer.Binaries = []manifest.Binary{b}
	cache := t.TempDir()
	path := layout.BinaryPath(cache, m.Metadata.Name, b)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("built-haproxy")
	if err := os.WriteFile(path, payload, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	outputSHA := hex.EncodeToString(sum[:])
	if err := os.WriteFile(layout.BinaryChecksumPath(cache, m.Metadata.Name, b), []byte(outputSHA+"  haproxy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(filepath.Dir(path), ".haproxy.build.json")
	writeMarker := func(version string) {
		t.Helper()
		data, err := json.Marshal(buildMarker{
			Recipe:       b.Build,
			Version:      version,
			SourceSHA256: b.SourceSHA256,
			OutputSHA256: outputSHA,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(markerPath, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeMarker(b.Version)
	results := Run(Options{Config: cfg, Manifest: m, CacheDir: cache})
	artifacts := find(results, "artifacts")
	if artifacts == nil || artifacts.Status != StatusOK {
		t.Fatalf("artifacts result = %#v", artifacts)
	}

	writeMarker("stale")
	results = Run(Options{Config: cfg, Manifest: m, CacheDir: cache})
	artifacts = find(results, "artifacts")
	if artifacts == nil || artifacts.Status != StatusFail || !strings.Contains(artifacts.Detail, "invalid build metadata") {
		t.Fatalf("artifacts result = %#v", artifacts)
	}
}

func find(results []Result, name string) *Result {
	for i := range results {
		if results[i].Name == name {
			return &results[i]
		}
	}
	return nil
}
