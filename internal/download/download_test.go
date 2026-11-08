package download

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/kube-dosh/kube-do/internal/layout"
	"github.com/kube-dosh/kube-do/internal/manifest"
)

func sourceArchive(t *testing.T) []byte {
	t.Helper()
	var body bytes.Buffer
	gz := gzip.NewWriter(&body)
	tw := tar.NewWriter(gz)
	dir := &tar.Header{Name: "demo-1.0/", Typeflag: tar.TypeDir, Mode: 0o755}
	if err := tw.WriteHeader(dir); err != nil {
		t.Fatal(err)
	}
	content := []byte("upstream source")
	file := &tar.Header{Name: "demo-1.0/Makefile", Mode: 0o644, Size: int64(len(content))}
	if err := tw.WriteHeader(file); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func testManifest(binaries []manifest.Binary, images []manifest.Image) *manifest.Manifest {
	return &manifest.Manifest{
		Metadata: manifest.Metadata{Name: "test-build", KubernetesVersion: "v1.36.5"},
		Spec: manifest.Spec{
			Platforms: []manifest.Platform{{OS: "linux", Arch: "amd64"}},
			Registry:  manifest.Registry{Namespace: "kube-do"},
			Binaries:  binaries,
			Images:    images,
		},
	}
}

func TestDownloadBinary(t *testing.T) {
	payload := []byte("fake-kubeadm-binary")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	b := manifest.Binary{
		Name: "kubeadm", Version: "v1.36.5", OS: "linux", Arch: "amd64",
		URL: srv.URL + "/kubeadm", SHA256: sha256Hex(payload),
	}
	m := testManifest([]manifest.Binary{b}, nil)
	cache := t.TempDir()

	sum, err := Run(context.Background(), Options{CacheDir: cache, RegistryAddress: "reg:5000", Manifest: m, BinariesOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Downloaded != 1 || sum.Skipped != 0 {
		t.Fatalf("first run summary = %+v", sum)
	}
	got, err := os.ReadFile(layout.BinaryPath(cache, m.Metadata.Name, b))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("content = %q", got)
	}

	sum, err = Run(context.Background(), Options{CacheDir: cache, RegistryAddress: "reg:5000", Manifest: m, BinariesOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Downloaded != 0 || sum.Skipped != 1 {
		t.Fatalf("second run summary = %+v", sum)
	}
}

func TestDownloadBinaryChecksumMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("tampered"))
	}))
	defer srv.Close()

	b := manifest.Binary{
		Name: "kubeadm", Version: "v1.36.5", OS: "linux", Arch: "amd64",
		URL: srv.URL, SHA256: strings.Repeat("0", 64),
	}
	m := testManifest([]manifest.Binary{b}, nil)
	cache := t.TempDir()

	if _, err := Run(context.Background(), Options{CacheDir: cache, RegistryAddress: "reg:5000", Manifest: m, BinariesOnly: true}); err == nil {
		t.Fatal("expected a checksum error")
	}
	if _, err := os.Stat(layout.BinaryPath(cache, m.Metadata.Name, b)); !os.IsNotExist(err) {
		t.Fatalf("partial file left behind: %v", err)
	}
}

func TestRunDownloadsOnlyTargetPlatform(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("fixture targets linux/amd64")
	}
	payload := []byte("target-only")
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	amd64 := manifest.Binary{Name: "kubeadm", Version: "v1.36.5", OS: "linux", Arch: "amd64", URL: srv.URL + "/amd64/kubeadm", SHA256: sha256Hex(payload)}
	arm64 := manifest.Binary{Name: "kubeadm", Version: "v1.36.5", OS: "linux", Arch: "arm64", URL: srv.URL + "/arm64/kubeadm", SHA256: sha256Hex(payload)}
	m := testManifest([]manifest.Binary{amd64, arm64}, nil)
	m.Spec.Platforms = []manifest.Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}}

	sum, err := Run(context.Background(), Options{CacheDir: t.TempDir(), RegistryAddress: "reg:5000", Manifest: m, BinariesOnly: true, Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Downloaded != 1 || sum.Platform != "linux/amd64" {
		t.Fatalf("summary = %+v", sum)
	}
	if len(paths) != 1 || paths[0] != "/amd64/kubeadm" {
		t.Fatalf("fetched paths = %v", paths)
	}
}

func TestRunRejectsInstallerHostArchitectureMismatch(t *testing.T) {
	if runtime.GOARCH == "arm64" {
		t.Skip("fixture assumes an amd64 Installer Host")
	}
	m := testManifest(nil, nil)
	m.Spec.Platforms = append(m.Spec.Platforms, manifest.Platform{OS: "linux", Arch: "arm64"})
	_, err := Run(context.Background(), Options{CacheDir: t.TempDir(), RegistryAddress: "reg:5000", Manifest: m, BinariesOnly: true, Arch: "arm64"})
	if err == nil || !strings.Contains(err.Error(), "Installer Host") {
		t.Fatalf("expected Installer Host mismatch, got %v", err)
	}
}

func TestBuildBinary(t *testing.T) {
	src := sourceArchive(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(src)
	}))
	defer srv.Close()

	b := manifest.Binary{
		Name: "haproxy", Version: "3.2.26", OS: "linux", Arch: "amd64",
		URL: srv.URL + "/haproxy.tar.gz", Archive: manifest.ArchiveTarGz,
		Build: manifest.BuildHAProxy, SourceSHA256: sha256Hex(src),
	}
	m := testManifest([]manifest.Binary{b}, nil)
	cache := t.TempDir()
	buildCalls := 0
	runner := func(_ context.Context, dir, name string, _ []string, _, _ io.Writer) error {
		buildCalls++
		if name != "make" {
			t.Fatalf("unexpected build command %q", name)
		}
		return os.WriteFile(filepath.Join(dir, "haproxy"), []byte("built-haproxy"), 0o755)
	}
	opts := Options{CacheDir: cache, RegistryAddress: "reg:5000", Manifest: m, BinariesOnly: true, Runner: runner}

	sum, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Downloaded != 1 || buildCalls != 1 {
		t.Fatalf("summary = %+v, buildCalls = %d", sum, buildCalls)
	}
	got, err := os.ReadFile(layout.BinaryPath(cache, m.Metadata.Name, b))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "built-haproxy" {
		t.Fatalf("built content = %q", got)
	}
	checksum, err := os.ReadFile(layout.BinaryChecksumPath(cache, m.Metadata.Name, b))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(checksum), sha256Hex(got)) {
		t.Fatalf("checksum sidecar = %q", checksum)
	}

	sum, err = Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Skipped != 1 || buildCalls != 1 {
		t.Fatalf("second summary = %+v, buildCalls = %d", sum, buildCalls)
	}

	if err := os.Remove(layout.BinaryChecksumPath(cache, m.Metadata.Name, b)); err != nil {
		t.Fatal(err)
	}
	sum, err = Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Downloaded != 1 || buildCalls != 2 {
		t.Fatalf("missing sidecar should rebuild: summary = %+v, buildCalls = %d", sum, buildCalls)
	}
	if _, err := os.Stat(layout.BinaryChecksumPath(cache, m.Metadata.Name, b)); err != nil {
		t.Fatalf("checksum sidecar was not recreated: %v", err)
	}
}

func TestMirrorImage(t *testing.T) {
	src := httptest.NewServer(registry.New())
	defer src.Close()
	dst := httptest.NewServer(registry.New())
	defer dst.Close()
	srcHost := strings.TrimPrefix(src.URL, "http://")
	dstHost := strings.TrimPrefix(dst.URL, "http://")

	srcRef, err := name.ParseReference(srcHost+"/upstream/pause:v1", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	img, err := random.Image(1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(srcRef, img); err != nil {
		t.Fatalf("seed source registry: %v", err)
	}

	m := testManifest(nil, []manifest.Image{{Name: "pause", Source: srcRef.String(), Target: "pause:v1"}})
	sum, err := Run(context.Background(), Options{
		CacheDir: t.TempDir(), RegistryAddress: dstHost, Manifest: m, ImagesOnly: true, Insecure: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Mirrored != 1 {
		t.Fatalf("summary = %+v", sum)
	}

	dstRef, err := name.ParseReference(dstHost+"/kube-do/pause:v1", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	srcDesc, err := remote.Get(srcRef)
	if err != nil {
		t.Fatal(err)
	}
	dstDesc, err := remote.Get(dstRef)
	if err != nil {
		t.Fatalf("mirrored image not found: %v", err)
	}
	if srcDesc.Digest != dstDesc.Digest {
		t.Fatalf("digest mismatch: %s != %s", srcDesc.Digest, dstDesc.Digest)
	}
}

func TestRunBuildsInstallerImage(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("fixture targets linux/amd64")
	}
	dst := httptest.NewServer(registry.New())
	defer dst.Close()
	dstHost := strings.TrimPrefix(dst.URL, "http://")

	payload := []byte("built-keepalived")
	binary := manifest.Binary{
		Name: "keepalived", Version: "2.4.3", OS: "linux", Arch: "amd64", SHA256: sha256Hex(payload),
	}
	m := testManifest(nil, nil)
	m.Spec.Installer.Binaries = []manifest.Binary{binary}
	m.Spec.Installer.Images = []manifest.InstallerImage{{
		Name: "keepalived", Version: "2.4.3", OS: "linux", Arch: "amd64",
		Target: "keepalived:2.4.3-amd64", Binary: "keepalived",
	}}
	cache := t.TempDir()
	binaryPath := layout.BinaryPath(cache, m.Metadata.Name, binary)
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryPath, payload, 0o755); err != nil {
		t.Fatal(err)
	}

	builderCalled := false
	sum, err := Run(context.Background(), Options{
		CacheDir: cache, RegistryAddress: dstHost, Manifest: m, ImagesOnly: true, Insecure: true,
		ImageBuilder: func(got manifest.Binary, gotPath string) (v1.Image, error) {
			builderCalled = true
			if got.Name != binary.Name || gotPath != binaryPath {
				t.Fatalf("builder input = %#v, %q", got, gotPath)
			}
			return random.Image(1024, 1)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !builderCalled || sum.Built != 1 || sum.Mirrored != 0 {
		t.Fatalf("summary = %+v, builderCalled = %v", sum, builderCalled)
	}
	dstRef, err := name.ParseReference(dstHost+"/kube-do/keepalived:2.4.3-amd64", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Get(dstRef); err != nil {
		t.Fatalf("generated image not found: %v", err)
	}
}

func TestRunRequiresBinaryForInstallerImage(t *testing.T) {
	m := testManifest(nil, nil)
	m.Spec.Installer.Binaries = []manifest.Binary{{
		Name: "keepalived", Version: "2.4.3", OS: "linux", Arch: "amd64", SHA256: strings.Repeat("0", 64),
	}}
	m.Spec.Installer.Images = []manifest.InstallerImage{{
		Name: "keepalived", Version: "2.4.3", OS: "linux", Arch: "amd64",
		Target: "keepalived:2.4.3-amd64", Binary: "keepalived",
	}}
	_, err := Run(context.Background(), Options{
		CacheDir: t.TempDir(), RegistryAddress: "reg:5000", Manifest: m, ImagesOnly: true,
	})
	if err == nil || !strings.Contains(err.Error(), "download --binaries-only") {
		t.Fatalf("expected missing binary error, got %v", err)
	}
}

func TestNormalizeRegistryKey(t *testing.T) {
	cases := map[string]string{
		"https://index.docker.io/v1/": "index.docker.io",
		"http://reg.internal:5000/":   "reg.internal:5000",
		"reg.internal:5000":           "reg.internal:5000",
	}
	for in, want := range cases {
		if got := normalizeRegistryKey(in); got != want {
			t.Errorf("normalizeRegistryKey(%q) = %q, want %q", in, got, want)
		}
	}
}
