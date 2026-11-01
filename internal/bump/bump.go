// Package bump updates a Resource Manifest to a reviewed set of upstream
// versions. It resolves dependent image and chart versions, downloads source or
// binary Artifacts to compute checksums, and returns a new Build for review.
package bump

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kubedo/kubedo/internal/archive"
	"github.com/kubedo/kubedo/internal/manifest"
)

type Versions struct {
	Kubernetes string
	Cilium     string
	Containerd string
	Runc       string
	CNIPlugins string
	CRITools   string
	Helm       string
	CiliumCLI  string
	Registry   string
	HAProxy    string
	Keepalived string
}

func (v Versions) Empty() bool {
	return v == Versions{}
}

type Options struct {
	Base     *manifest.Manifest
	Build    string
	Versions Versions

	HashURL       func(ctx context.Context, url string) (string, error)
	DownloadFile  func(ctx context.Context, url, dest string) (string, error)
	KubeadmImages func(ctx context.Context, binary, version string) ([]string, error)
	Logf          func(format string, args ...any)
}

type Result struct {
	Manifest *manifest.Manifest
	Build    string
	Changes  []string
}

var buildSuffixRe = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+-kubedo\.([0-9]+)$`)

// Run applies version overrides to Base and returns a validated new Build.
func Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.Base == nil {
		return nil, errors.New("base manifest is required")
	}
	if opts.Versions.Empty() {
		return nil, errors.New("at least one component version must be provided")
	}
	next, err := clone(opts.Base)
	if err != nil {
		return nil, err
	}
	if opts.HashURL == nil {
		opts.HashURL = hashURL
	}
	if opts.DownloadFile == nil {
		opts.DownloadFile = downloadFile
	}
	if opts.KubeadmImages == nil {
		opts.KubeadmImages = kubeadmImages
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}

	var changes []string
	if opts.Versions.Kubernetes != "" {
		if err := applyKubernetes(ctx, &opts, next, opts.Versions.Kubernetes, &changes); err != nil {
			return nil, err
		}
	}
	if opts.Versions.Cilium != "" {
		if err := applyCilium(ctx, &opts, next, opts.Versions.Cilium, &changes); err != nil {
			return nil, err
		}
	}
	if opts.Versions.CiliumCLI != "" {
		if err := applyBinary(ctx, &opts, next, "cilium-cli", opts.Versions.CiliumCLI, changesFor(&changes)); err != nil {
			return nil, err
		}
	}
	for _, item := range []struct {
		name    string
		version string
	}{
		{"containerd", opts.Versions.Containerd},
		{"runc", opts.Versions.Runc},
		{"cni-plugins", opts.Versions.CNIPlugins},
		{"crictl", opts.Versions.CRITools},
		{"helm", opts.Versions.Helm},
		{"registry", opts.Versions.Registry},
		{"haproxy", opts.Versions.HAProxy},
		{"keepalived", opts.Versions.Keepalived},
	} {
		if item.version == "" {
			continue
		}
		if err := applyBinary(ctx, &opts, next, item.name, item.version, changesFor(&changes)); err != nil {
			return nil, err
		}
	}

	build := opts.Build
	if build == "" {
		build = DefaultBuildName(opts.Base.Metadata.Name, next.Metadata.KubernetesVersion)
	}
	if build == opts.Base.Metadata.Name {
		return nil, errors.New("the new Build must differ from the base Build; pass --build")
	}
	next.Metadata.Name = build
	if err := next.Validate(); err != nil {
		return nil, err
	}
	sort.Strings(changes)
	return &Result{Manifest: next, Build: build, Changes: dedupe(changes)}, nil
}

func applyKubernetes(ctx context.Context, opts *Options, m *manifest.Manifest, raw string, changes *[]string) error {
	version := "v" + trimV(raw)
	if version == m.Metadata.KubernetesVersion {
		return nil
	}
	kubeadms := findBinaries(m, "kubeadm")
	if len(kubeadms) == 0 {
		return fmt.Errorf("binary %q is not present in the base manifest", "kubeadm")
	}
	var hostKubeadm *manifest.Binary
	for _, b := range kubeadms {
		if b.OS == runtime.GOOS && b.Arch == runtime.GOARCH {
			hostKubeadm = b
			break
		}
	}
	if hostKubeadm == nil {
		return fmt.Errorf("resolving kubeadm images requires a kubeadm binary for Installer Host %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	kubeadmURL := fmt.Sprintf("https://dl.k8s.io/release/%s/bin/%s/%s/kubeadm", version, hostKubeadm.OS, hostKubeadm.Arch)
	tmp, err := os.CreateTemp("", "kubedo-kubeadm-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)
	opts.Logf("fetch  kubeadm %s", version)
	kubeadmSHA, err := opts.DownloadFile(ctx, kubeadmURL, tmpPath)
	if err != nil {
		return fmt.Errorf("download kubeadm: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	refs, err := opts.KubeadmImages(ctx, tmpPath, version)
	if err != nil {
		return fmt.Errorf("resolve kubeadm default images: %w", err)
	}

	for _, name := range []string{"kubeadm", "kubelet", "kubectl"} {
		binaries := findBinaries(m, name)
		if len(binaries) == 0 {
			return fmt.Errorf("binary %q is not present in the base manifest", name)
		}
		for _, b := range binaries {
			b.Version = version
			b.URL = fmt.Sprintf("https://dl.k8s.io/release/%s/bin/%s/%s/%s", version, b.OS, b.Arch, name)
			if name == "kubeadm" && b == hostKubeadm {
				b.SHA256 = kubeadmSHA
			} else {
				opts.Logf("fetch  %s %s/%s %s", name, b.OS, b.Arch, version)
				var err error
				b.SHA256, err = opts.HashURL(ctx, b.URL)
				if err != nil {
					return fmt.Errorf("checksum %s %s/%s: %w", name, b.OS, b.Arch, err)
				}
			}
			*changes = append(*changes, fmt.Sprintf("binary %s %s/%s -> %s", name, b.OS, b.Arch, version))
		}
	}

	for _, ref := range refs {
		name, source, target, ok := classifyKubernetesImage(ref)
		if !ok {
			continue
		}
		if err := setImage(m, name, source, target, changes); err != nil {
			return err
		}
	}
	old := m.Metadata.KubernetesVersion
	m.Metadata.KubernetesVersion = version
	*changes = append(*changes, fmt.Sprintf("kubernetesVersion %s -> %s", old, version))
	return nil
}

func applyCilium(ctx context.Context, opts *Options, m *manifest.Manifest, raw string, changes *[]string) error {
	version := trimV(raw)
	charts := findBinaries(m, "cilium-chart")
	if len(charts) == 0 {
		return fmt.Errorf("binary %q is not present in the base manifest", "cilium-chart")
	}
	chartURL := fmt.Sprintf("https://helm.cilium.io/cilium-%s.tgz", version)
	tmp, err := os.CreateTemp("", "kubedo-cilium-*.tgz")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)
	opts.Logf("fetch  cilium chart %s", version)
	chartSHA, err := opts.DownloadFile(ctx, chartURL, tmpPath)
	if err != nil {
		return fmt.Errorf("download cilium chart: %w", err)
	}
	envoyTag, certgenTag, err := ciliumChartTags(tmpPath)
	if err != nil {
		return err
	}
	for _, chart := range charts {
		chart.Version = version
		chart.URL = chartURL
		chart.SHA256 = chartSHA
		*changes = append(*changes, fmt.Sprintf("binary cilium-chart %s/%s -> %s", chart.OS, chart.Arch, version))
	}

	imageTag := "v" + version
	updates := []struct {
		name   string
		source string
		target string
	}{
		{"cilium", "quay.io/cilium/cilium:" + imageTag, "cilium/cilium:" + imageTag},
		{"cilium-operator", "quay.io/cilium/operator-generic:" + imageTag, "cilium/operator-generic:" + imageTag},
		{"hubble-relay", "quay.io/cilium/hubble-relay:" + imageTag, "cilium/hubble-relay:" + imageTag},
		{"cilium-envoy", "quay.io/cilium/cilium-envoy:" + envoyTag, "cilium/cilium-envoy:" + envoyTag},
		{"certgen", "quay.io/cilium/certgen:" + certgenTag, "cilium/certgen:" + certgenTag},
	}
	for _, u := range updates {
		if err := setImage(m, u.name, u.source, u.target, changes); err != nil {
			return err
		}
	}
	return nil
}

func applyBinary(ctx context.Context, opts *Options, m *manifest.Manifest, name, raw string, changes *[]string) error {
	binaries := findBinaries(m, name)
	if len(binaries) == 0 {
		return fmt.Errorf("binary %q is not present in the base manifest", name)
	}
	checksums := map[string]string{}
	for _, b := range binaries {
		version, url, built := componentVersionAndURL(*b, raw)
		if version == b.Version && url == b.URL {
			continue
		}
		checksum, ok := checksums[url]
		if !ok {
			opts.Logf("fetch  %s %s/%s %s", name, b.OS, b.Arch, version)
			var err error
			checksum, err = opts.HashURL(ctx, url)
			if err != nil {
				return fmt.Errorf("checksum %s %s/%s: %w", name, b.OS, b.Arch, err)
			}
			checksums[url] = checksum
		}
		old := b.Version
		b.Version = version
		b.URL = url
		if built {
			b.SourceSHA256 = checksum
			b.SHA256 = ""
		} else {
			b.SHA256 = checksum
		}
		syncInstallerImageVersion(m, b.Name, b.OS, b.Arch, old, version, changes)
		*changes = append(*changes, fmt.Sprintf("binary %s %s/%s %s -> %s", name, b.OS, b.Arch, old, version))
	}
	return nil
}

func syncInstallerImageVersion(m *manifest.Manifest, binaryName, binaryOS, binaryArch, oldVersion, newVersion string, changes *[]string) {
	for i := range m.Spec.Installer.Images {
		img := &m.Spec.Installer.Images[i]
		if img.Binary != binaryName || img.OS != binaryOS || img.Arch != binaryArch || img.Version != oldVersion {
			continue
		}
		oldTarget := img.Target
		img.Version = newVersion
		repo, tag := splitImageTarget(img.Target)
		if tag == oldVersion {
			img.Target = repo + ":" + newVersion
		} else if strings.HasPrefix(tag, oldVersion+"-") {
			img.Target = repo + ":" + newVersion + strings.TrimPrefix(tag, oldVersion)
		}
		if img.Target != oldTarget {
			*changes = append(*changes, fmt.Sprintf("Installer image %s %s/%s -> %s", img.Name, img.OS, img.Arch, img.Target))
		}
	}
}

func splitImageTarget(ref string) (repository, tag string) {
	i := strings.LastIndex(ref, ":")
	j := strings.LastIndex(ref, "/")
	if i <= j || i == len(ref)-1 {
		return ref, ""
	}
	return ref[:i], ref[i+1:]
}

func componentVersionAndURL(b manifest.Binary, raw string) (version, url string, built bool) {
	plain := trimV(raw)
	switch b.Name {
	case "containerd":
		version = plain
		url = fmt.Sprintf("https://github.com/containerd/containerd/releases/download/v%s/containerd-%s-linux-%s.tar.gz", plain, plain, b.Arch)
	case "runc":
		version = "v" + plain
		url = fmt.Sprintf("https://github.com/opencontainers/runc/releases/download/v%s/runc.%s", plain, b.Arch)
	case "cni-plugins":
		version = "v" + plain
		url = fmt.Sprintf("https://github.com/containernetworking/plugins/releases/download/v%s/cni-plugins-linux-%s-v%s.tgz", plain, b.Arch, plain)
	case "crictl":
		version = "v" + plain
		url = fmt.Sprintf("https://github.com/kubernetes-sigs/cri-tools/releases/download/v%s/crictl-v%s-linux-%s.tar.gz", plain, plain, b.Arch)
	case "helm":
		version = "v" + plain
		url = fmt.Sprintf("https://get.helm.sh/helm-v%s-linux-%s.tar.gz", plain, b.Arch)
	case "cilium-cli":
		version = "v" + plain
		url = fmt.Sprintf("https://github.com/cilium/cilium-cli/releases/download/v%s/cilium-linux-%s.tar.gz", plain, b.Arch)
	case "registry":
		version = plain
		url = fmt.Sprintf("https://github.com/distribution/distribution/releases/download/v%s/registry_%s_linux_%s.tar.gz", plain, plain, b.Arch)
	case "haproxy":
		parts := strings.Split(plain, ".")
		branch := plain
		if len(parts) >= 2 {
			branch = parts[0] + "." + parts[1]
		}
		version = plain
		url = fmt.Sprintf("https://www.haproxy.org/download/%s/src/haproxy-%s.tar.gz", branch, plain)
		built = true
	case "keepalived":
		version = plain
		url = fmt.Sprintf("https://www.keepalived.org/software/keepalived-%s.tar.gz", plain)
		built = true
	default:
		return b.Version, b.URL, b.IsBuilt()
	}
	return version, url, built
}

func setImage(m *manifest.Manifest, name, source, target string, changes *[]string) error {
	for i := range m.Spec.Images {
		img := &m.Spec.Images[i]
		if img.Name != name {
			continue
		}
		if img.Source == source && img.Target == target {
			return nil
		}
		img.Source = source
		img.Target = target
		*changes = append(*changes, fmt.Sprintf("image %s -> %s", name, source))
		return nil
	}
	return fmt.Errorf("image %q is not present in the base manifest", name)
}

func findBinaries(m *manifest.Manifest, name string) []*manifest.Binary {
	var out []*manifest.Binary
	for i := range m.Spec.Binaries {
		if m.Spec.Binaries[i].Name == name {
			out = append(out, &m.Spec.Binaries[i])
		}
	}
	for i := range m.Spec.Installer.Binaries {
		if m.Spec.Installer.Binaries[i].Name == name {
			out = append(out, &m.Spec.Installer.Binaries[i])
		}
	}
	return out
}

func classifyKubernetesImage(ref string) (name, source, target string, ok bool) {
	tag := imageTag(ref)
	if tag == "" {
		return "", "", "", false
	}
	repo := strings.TrimSuffix(ref, ":"+tag)
	base := filepath.Base(repo)
	switch {
	case base == "coredns":
		return "coredns", ref, "coredns/coredns:" + tag, true
	case base == "etcd":
		return "etcd", ref, "etcd:" + tag, true
	case base == "pause":
		return "pause", ref, "pause:" + tag, true
	case strings.HasPrefix(base, "kube-") && base != "kube-proxy":
		return base, ref, base + ":" + tag, true
	default:
		return "", "", "", false
	}
}

func imageTag(ref string) string {
	i := strings.LastIndex(ref, ":")
	j := strings.LastIndex(ref, "/")
	if i <= j || i == len(ref)-1 {
		return ""
	}
	return ref[i+1:]
}

func ciliumChartTags(chartPath string) (envoy, certgen string, err error) {
	dir, err := os.MkdirTemp("", "kubedo-cilium-chart-*")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(dir)
	files, err := archive.ExtractTarGz(chartPath, dir)
	if err != nil {
		return "", "", fmt.Errorf("extract cilium chart: %w", err)
	}
	valuesPath := ""
	for _, f := range files {
		if filepath.Base(f) == "values.yaml" {
			valuesPath = f
			break
		}
	}
	if valuesPath == "" {
		return "", "", errors.New("cilium chart has no values.yaml")
	}
	data, err := os.ReadFile(valuesPath)
	if err != nil {
		return "", "", err
	}
	var values struct {
		Envoy struct {
			Image struct {
				Tag string `yaml:"tag"`
			} `yaml:"image"`
		} `yaml:"envoy"`
		Certgen struct {
			Image struct {
				Tag string `yaml:"tag"`
			} `yaml:"image"`
		} `yaml:"certgen"`
	}
	if err := yaml.Unmarshal(data, &values); err != nil {
		return "", "", err
	}
	if values.Envoy.Image.Tag == "" || values.Certgen.Image.Tag == "" {
		return "", "", errors.New("cilium chart does not declare envoy/certgen image tags")
	}
	return values.Envoy.Image.Tag, values.Certgen.Image.Tag, nil
}

// DefaultBuildName increments the .N suffix when Kubernetes stays on the same
// minor/patch version, and starts at .1 for a new Kubernetes version.
func DefaultBuildName(baseName, kubernetesVersion string) string {
	prefix := fmt.Sprintf("v%s-kubedo.", trimV(kubernetesVersion))
	if strings.HasPrefix(baseName, prefix) {
		if match := buildSuffixRe.FindStringSubmatch(baseName); match != nil {
			if n, err := strconv.Atoi(match[1]); err == nil {
				return prefix + strconv.Itoa(n+1)
			}
		}
	}
	return prefix + "1"
}

func clone(m *manifest.Manifest) (*manifest.Manifest, error) {
	data, err := yaml.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out manifest.Manifest
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func trimV(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "v") }

func changesFor(changes *[]string) *[]string { return changes }

func dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := in[:0]
	for _, s := range in {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}

var httpClient = &http.Client{Timeout: 30 * time.Minute}

func hashURL(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %s", resp.Status)
	}
	h := sha256.New()
	if _, err := io.Copy(h, resp.Body); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func downloadFile(ctx context.Context, url, dest string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %s", resp.Status)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func kubeadmImages(ctx context.Context, binary, version string) ([]string, error) {
	cmd := exec.CommandContext(ctx, binary, "config", "images", "list", "--kubernetes-version", version)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			refs = append(refs, line)
		}
	}
	return refs, nil
}
