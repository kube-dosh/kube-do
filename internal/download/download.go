// Package download fetches binaries, builds Installer runtime images, and
// mirrors node container images into the private Registry.
package download

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/kubedo/kubedo/internal/archive"
	"github.com/kubedo/kubedo/internal/imagebuild"
	"github.com/kubedo/kubedo/internal/layout"
	"github.com/kubedo/kubedo/internal/manifest"
)

type Options struct {
	CacheDir        string
	RegistryAddress string
	Manifest        *manifest.Manifest
	// OS and Arch select the target platform. They must match the Installer
	// Host because built Artifacts (HAProxy and keepalived) are not cross-built.
	OS           string
	Arch         string
	BinariesOnly bool
	ImagesOnly   bool
	// Insecure allows plain HTTP registries (useful for local testing).
	Insecure        bool
	CAFile          string
	CredentialsFile string
	Logf            func(format string, args ...any)
	Runner          func(ctx context.Context, dir, name string, args []string, stdout, stderr io.Writer) error
	// ImageBuilder assembles an Installer-generated runtime image from a
	// verified cached binary. It is injectable for tests.
	ImageBuilder func(binary manifest.Binary, binaryPath string) (v1.Image, error)
}

type Summary struct {
	Downloaded int
	Skipped    int
	Mirrored   int
	Built      int
	Platform   string
}

var binaryClient = &http.Client{Timeout: 30 * time.Minute}

// Run downloads binaries, builds Installer images, and mirrors node images
// according to opts.
func Run(ctx context.Context, opts Options) (Summary, error) {
	var sum Summary
	if opts.Manifest == nil {
		return sum, errors.New("manifest is nil")
	}
	if opts.BinariesOnly && opts.ImagesOnly {
		return sum, errors.New("binaries-only and images-only are mutually exclusive")
	}
	if opts.OS == "" {
		opts.OS = "linux"
	}
	if opts.Arch == "" {
		opts.Arch = runtime.GOARCH
	}
	sum.Platform = opts.OS + "/" + opts.Arch
	if !opts.Manifest.SupportsPlatform(opts.OS, opts.Arch) {
		return sum, fmt.Errorf("build %q does not support platform %s/%s", opts.Manifest.Metadata.Name, opts.OS, opts.Arch)
	}
	if runtime.GOOS != opts.OS || runtime.GOARCH != opts.Arch {
		return sum, fmt.Errorf("Installer Host is %s/%s but target is %s/%s; built HA artifacts cannot be cross-compiled", runtime.GOOS, runtime.GOARCH, opts.OS, opts.Arch)
	}

	if !opts.ImagesOnly {
		binaries := make([]manifest.Binary, 0)
		binaries = append(binaries, opts.Manifest.BinariesFor(opts.OS, opts.Arch)...)
		binaries = append(binaries, opts.Manifest.InstallerBinariesFor(opts.OS, opts.Arch)...)
		if len(binaries) == 0 {
			return sum, fmt.Errorf("build %q has no binaries for %s/%s", opts.Manifest.Metadata.Name, opts.OS, opts.Arch)
		}
		for _, b := range binaries {
			downloaded, err := downloadBinary(ctx, opts, b)
			if err != nil {
				return sum, fmt.Errorf("download %s: %w", b.Name, err)
			}
			if downloaded {
				sum.Downloaded++
			} else {
				sum.Skipped++
			}
		}
	}

	if !opts.BinariesOnly {
		remoteOpts, err := remoteOptions(ctx, opts)
		if err != nil {
			return sum, err
		}
		for _, img := range opts.Manifest.InstallerImagesFor(opts.OS, opts.Arch) {
			if err := pushInstallerImage(img, opts, remoteOpts); err != nil {
				return sum, fmt.Errorf("build Installer image %s: %w", img.Name, err)
			}
			sum.Built++
		}
		for _, img := range opts.Manifest.Spec.Images {
			if err := mirrorImage(img, opts, remoteOpts); err != nil {
				return sum, fmt.Errorf("mirror %s: %w", img.Name, err)
			}
			sum.Mirrored++
		}
	}
	return sum, nil
}

func pushInstallerImage(img manifest.InstallerImage, opts Options, remoteOpts []remote.Option) error {
	binary, ok := opts.Manifest.InstallerBinaryFor(img.Binary, opts.OS, opts.Arch)
	if !ok {
		return fmt.Errorf("Installer binary %q is not present for %s/%s", img.Binary, opts.OS, opts.Arch)
	}
	binaryPath, err := verifiedCachedBinary(opts, binary)
	if err != nil {
		return err
	}
	target, err := img.TargetRef(opts.RegistryAddress, opts.Manifest.Spec.Registry.Namespace)
	if err != nil {
		return err
	}
	dst, err := parseRef(target, opts.Insecure)
	if err != nil {
		return fmt.Errorf("parse target: %w", err)
	}
	builder := opts.ImageBuilder
	if builder == nil {
		builder = func(binary manifest.Binary, binaryPath string) (v1.Image, error) {
			return imagebuild.Build(imagebuild.Options{
				BinaryPath: binaryPath,
				BinaryName: binary.Name,
				Version:    binary.Version,
				OS:         binary.OS,
				Arch:       binary.Arch,
			})
		}
	}
	logf(opts, "build  %s -> %s", img.Name, dst)
	image, err := builder(binary, binaryPath)
	if err != nil {
		return err
	}
	if err := remote.Write(dst, image, remoteOpts...); err != nil {
		return err
	}
	logf(opts, "ok     %s", dst)
	return nil
}

func verifiedCachedBinary(opts Options, binary manifest.Binary) (string, error) {
	binaryPath := layout.BinaryPath(opts.CacheDir, opts.Manifest.Metadata.Name, binary)
	want := binary.SHA256
	if binary.IsBuilt() {
		sidecar, err := os.ReadFile(layout.BinaryChecksumPath(opts.CacheDir, opts.Manifest.Metadata.Name, binary))
		if err != nil {
			return "", fmt.Errorf("Installer image requires the built binary %q; run 'kubedo download --binaries-only' first: %w", binary.Name, err)
		}
		fields := strings.Fields(string(sidecar))
		if len(fields) == 0 {
			return "", fmt.Errorf("checksum sidecar for %q is empty; rerun 'kubedo download --binaries-only'", binary.Name)
		}
		want = fields[0]
	}
	ok, err := verifyFile(binaryPath, want)
	if err != nil {
		return "", fmt.Errorf("verify cached binary %q: %w", binary.Name, err)
	}
	if !ok {
		return "", fmt.Errorf("Installer image requires a verified cached binary %q; run 'kubedo download --binaries-only' first", binary.Name)
	}
	return binaryPath, nil
}

func downloadBinary(ctx context.Context, opts Options, b manifest.Binary) (bool, error) {
	if b.IsBuilt() {
		return buildBinary(ctx, opts, b)
	}
	dest := layout.BinaryPath(opts.CacheDir, opts.Manifest.Metadata.Name, b)
	if ok, err := verifyFile(dest, b.SHA256); err == nil && ok {
		logf(opts, "skip   %s (already present)", b.Name)
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return false, fmt.Errorf("create cache directory: %w", err)
	}
	logf(opts, "fetch  %s <- %s", b.Name, b.URL)
	if err := fetchFile(ctx, b.URL, dest, b.SHA256); err != nil {
		return false, err
	}
	logf(opts, "ok     %s (%s)", b.Name, shortSHA(b.SHA256))
	return true, nil
}

func fetchFile(ctx context.Context, sourceURL, dest, wantSHA string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return err
	}
	resp, err := binaryClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpName)
		}
	}()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, wantSHA) {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, wantSHA)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return err
	}
	success = true
	return nil
}

type buildMarker struct {
	Recipe       string `json:"recipe"`
	Version      string `json:"version"`
	SourceSHA256 string `json:"sourceSha256"`
	OutputSHA256 string `json:"outputSha256"`
}

func buildBinary(ctx context.Context, opts Options, b manifest.Binary) (bool, error) {
	dest := layout.BinaryPath(opts.CacheDir, opts.Manifest.Metadata.Name, b)
	markerPath := filepath.Join(filepath.Dir(dest), "."+b.Name+".build.json")
	checksumPath := layout.BinaryChecksumPath(opts.CacheDir, opts.Manifest.Metadata.Name, b)
	if current, err := builtCurrent(dest, checksumPath, markerPath, b); err == nil && current {
		logf(opts, "skip   %s (build is current)", b.Name)
		return false, nil
	}
	if runtime.GOOS != b.OS || runtime.GOARCH != b.Arch {
		return false, fmt.Errorf("cannot build %s on %s/%s for target %s/%s", b.Name, runtime.GOOS, runtime.GOARCH, b.OS, b.Arch)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return false, fmt.Errorf("create cache directory: %w", err)
	}
	source := filepath.Join(filepath.Dir(dest), "."+b.Name+"-"+b.Version+".source.tar.gz")
	if ok, err := verifyFile(source, b.SourceSHA256); err != nil || !ok {
		logf(opts, "fetch  %s source <- %s", b.Name, b.URL)
		if err := fetchFile(ctx, b.URL, source, b.SourceSHA256); err != nil {
			return false, fmt.Errorf("fetch source: %w", err)
		}
	}

	buildDir, err := os.MkdirTemp("", "kubedo-build-*")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(buildDir)
	if _, err := archive.ExtractTarGz(source, buildDir); err != nil {
		return false, fmt.Errorf("extract source: %w", err)
	}
	sourceRoot, err := extractedRoot(buildDir)
	if err != nil {
		return false, err
	}
	logf(opts, "build  %s %s (%s)", b.Name, b.Version, b.Build)
	output, err := runRecipe(ctx, opts, b, sourceRoot)
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(output)
	if err != nil {
		return false, fmt.Errorf("read build output: %w", err)
	}
	if len(data) == 0 {
		return false, fmt.Errorf("build output %s is empty", output)
	}
	sum := sha256.Sum256(data)
	outputSHA := hex.EncodeToString(sum[:])
	if err := writeAtomic(dest, data, 0o755); err != nil {
		return false, fmt.Errorf("install built binary: %w", err)
	}
	checksum := []byte(fmt.Sprintf("%s  %s\n", outputSHA, b.Name))
	if err := writeAtomic(checksumPath, checksum, 0o644); err != nil {
		return false, fmt.Errorf("write checksum sidecar: %w", err)
	}
	marker, err := json.MarshalIndent(buildMarker{
		Recipe:       b.Build,
		Version:      b.Version,
		SourceSHA256: b.SourceSHA256,
		OutputSHA256: outputSHA,
	}, "", "  ")
	if err != nil {
		return false, err
	}
	if err := writeAtomic(markerPath, append(marker, '\n'), 0o644); err != nil {
		return false, fmt.Errorf("write build marker: %w", err)
	}
	logf(opts, "ok     %s (%s)", b.Name, shortSHA(outputSHA))
	return true, nil
}

func builtCurrent(binaryPath, checksumPath, markerPath string, b manifest.Binary) (bool, error) {
	data, err := os.ReadFile(markerPath)
	if err != nil {
		return false, err
	}
	var marker buildMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return false, err
	}
	if marker.Recipe != b.Build || marker.Version != b.Version || marker.SourceSHA256 != b.SourceSHA256 {
		return false, nil
	}
	checksum, err := os.ReadFile(checksumPath)
	if err != nil {
		return false, err
	}
	fields := strings.Fields(string(checksum))
	if len(fields) == 0 || !strings.EqualFold(fields[0], marker.OutputSHA256) {
		return false, nil
	}
	return verifyFile(binaryPath, marker.OutputSHA256)
}

func extractedRoot(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	if len(entries) == 1 && entries[0].IsDir() {
		return filepath.Join(dir, entries[0].Name()), nil
	}
	return dir, nil
}

func runRecipe(ctx context.Context, opts Options, b manifest.Binary, dir string) (string, error) {
	runner := opts.Runner
	if runner == nil {
		runner = runBuildCommand
	}
	jobs := strconv.Itoa(max(1, min(runtime.NumCPU(), 8)))
	switch b.Build {
	case manifest.BuildHAProxy:
		args := []string{"-j" + jobs, "TARGET=linux-glibc", "USE_OPENSSL=0", "USE_PCRE=0", "USE_ZLIB=0", "USE_THREAD=1", "USE_LIBCRYPT=0"}
		if err := runner(ctx, dir, "make", args, os.Stdout, os.Stderr); err != nil {
			return "", fmt.Errorf("build %s: %w", b.Name, err)
		}
		return filepath.Join(dir, "haproxy"), nil
	case manifest.BuildKeepalived:
		configureArgs := []string{
			"--disable-snmp",
			"--disable-dbus",
			"--disable-json",
			"--disable-libnl",
			"--disable-nftables",
		}
		if err := runner(ctx, dir, "./configure", configureArgs, os.Stdout, os.Stderr); err != nil {
			return "", fmt.Errorf("configure %s: %w", b.Name, err)
		}
		if err := runner(ctx, dir, "make", []string{"-j" + jobs}, os.Stdout, os.Stderr); err != nil {
			return "", fmt.Errorf("build %s: %w", b.Name, err)
		}
		return filepath.Join(dir, "bin", "keepalived"), nil
	default:
		return "", fmt.Errorf("unknown build recipe %q", b.Build)
	}
}

func runBuildCommand(ctx context.Context, dir, name string, args []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = os.Environ()
	return cmd.Run()
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func verifyFile(path, want string) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return strings.EqualFold(hex.EncodeToString(h.Sum(nil)), want), nil
}

func mirrorImage(img manifest.Image, opts Options, remoteOpts []remote.Option) error {
	src, err := parseRef(img.Source, opts.Insecure)
	if err != nil {
		return fmt.Errorf("parse source: %w", err)
	}
	target, err := img.TargetRef(opts.RegistryAddress, opts.Manifest.Spec.Registry.Namespace)
	if err != nil {
		return err
	}
	dst, err := parseRef(target, opts.Insecure)
	if err != nil {
		return fmt.Errorf("parse target: %w", err)
	}
	logf(opts, "mirror %s -> %s", src, dst)
	desc, err := remote.Get(src, remoteOpts...)
	if err != nil {
		return err
	}
	switch desc.MediaType {
	case types.OCIImageIndex, types.DockerManifestList:
		idx, err := desc.ImageIndex()
		if err != nil {
			return err
		}
		if err := remote.WriteIndex(dst, idx, remoteOpts...); err != nil {
			return err
		}
	default:
		image, err := desc.Image()
		if err != nil {
			return err
		}
		if err := remote.Write(dst, image, remoteOpts...); err != nil {
			return err
		}
	}
	logf(opts, "ok     %s", dst)
	return nil
}

func remoteOptions(ctx context.Context, opts Options) ([]remote.Option, error) {
	transport, err := buildTransport(opts.CAFile)
	if err != nil {
		return nil, err
	}
	keychain, err := buildKeychain(opts.CredentialsFile)
	if err != nil {
		return nil, err
	}
	return []remote.Option{
		remote.WithContext(ctx),
		remote.WithTransport(transport),
		remote.WithAuthFromKeychain(keychain),
	}, nil
}

func parseRef(ref string, insecure bool) (name.Reference, error) {
	var opts []name.Option
	if insecure {
		opts = append(opts, name.Insecure)
	}
	return name.ParseReference(ref, opts...)
}

func buildTransport(caFile string) (*http.Transport, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("CA file contains no certificates")
		}
	}
	return &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool},
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   true,
	}, nil
}

func buildKeychain(path string) (authn.Keychain, error) {
	if path == "" {
		return authn.DefaultKeychain, nil
	}
	k, err := newFileKeychain(path)
	if err != nil {
		return nil, err
	}
	return authn.NewMultiKeychain(k, authn.DefaultKeychain), nil
}

type fileKeychain struct {
	auths map[string]authn.Authenticator
}

type dockerConfig struct {
	Auths map[string]struct {
		Auth     string `json:"auth"`
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"auths"`
}

func newFileKeychain(path string) (authn.Keychain, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read credentials file: %w", err)
	}
	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse credentials file: %w", err)
	}
	auths := make(map[string]authn.Authenticator, len(cfg.Auths))
	for host, entry := range cfg.Auths {
		username, password := entry.Username, entry.Password
		if entry.Auth != "" {
			decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
			if err != nil {
				return nil, fmt.Errorf("decode credentials for %s: %w", host, err)
			}
			parts := strings.SplitN(string(decoded), ":", 2)
			if len(parts) == 2 {
				username, password = parts[0], parts[1]
			}
		}
		auths[normalizeRegistryKey(host)] = authn.FromConfig(authn.AuthConfig{Username: username, Password: password})
	}
	return &fileKeychain{auths: auths}, nil
}

func (k *fileKeychain) Resolve(resource authn.Resource) (authn.Authenticator, error) {
	if a, ok := k.auths[normalizeRegistryKey(resource.RegistryStr())]; ok {
		return a, nil
	}
	return authn.Anonymous, nil
}

func normalizeRegistryKey(raw string) string {
	s := strings.TrimPrefix(raw, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimSuffix(s, "/")
	if i := strings.Index(s, "/"); i >= 0 {
		s = s[:i]
	}
	return s
}

func logf(opts Options, format string, args ...any) {
	if opts.Logf != nil {
		opts.Logf(format, args...)
	}
}

func shortSHA(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}
