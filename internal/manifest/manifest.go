// Package manifest models a Build's Resource Manifest: the versioned set of
// binaries and container images needed to install one Kubernetes cluster.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

const (
	APIVersion = "kube-do.io/v1alpha1"
	Kind       = "ResourceManifest"

	ArchiveNone  = ""
	ArchiveTarGz = "tar.gz"

	// Build recipes are intentionally a closed set. They run on the Installer
	// Host and produce a single executable from an upstream source archive.
	BuildHAProxy    = "haproxy"
	BuildKeepalived = "keepalived"
)

// Manifest is the Resource Manifest for one Build.
type Manifest struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
}

type Metadata struct {
	// Name is the Build identifier, e.g. v1.36.5-kube-do.1.
	Name              string `yaml:"name"`
	KubernetesVersion string `yaml:"kubernetesVersion"`
}

type Spec struct {
	// Platforms lists every OS/architecture pair provided by this Build.
	// Platform is retained for v1alpha1 manifests that predate multi-platform
	// Builds; new manifests should use Platforms.
	Platforms []Platform `yaml:"platforms,omitempty"`
	Platform  *Platform  `yaml:"platform,omitempty"`
	Registry  Registry   `yaml:"registry"`
	Installer Installer  `yaml:"installer"`
	// Binaries are installed on cluster nodes.
	Binaries []Binary `yaml:"binaries"`
	// Images are pulled from Registry by cluster nodes.
	Images []Image `yaml:"images"`
}

type Platform struct {
	OS   string `yaml:"os"`
	Arch string `yaml:"arch"`
}

// SupportedPlatforms returns the normalized platform list.
func (m *Manifest) SupportedPlatforms() []Platform {
	if len(m.Spec.Platforms) > 0 {
		out := make([]Platform, len(m.Spec.Platforms))
		copy(out, m.Spec.Platforms)
		return out
	}
	if m.Spec.Platform != nil {
		return []Platform{*m.Spec.Platform}
	}
	return nil
}

// SupportsPlatform reports whether this Build declares os/arch.
func (m *Manifest) SupportsPlatform(os, arch string) bool {
	for _, p := range m.SupportedPlatforms() {
		if p.OS == os && p.Arch == arch {
			return true
		}
	}
	return false
}

// BinariesFor returns node binaries for one platform.
func (m *Manifest) BinariesFor(os, arch string) []Binary {
	return filterBinaries(m.Spec.Binaries, os, arch)
}

// InstallerBinariesFor returns Installer Host binaries for one platform.
func (m *Manifest) InstallerBinariesFor(os, arch string) []Binary {
	return filterBinaries(m.Spec.Installer.Binaries, os, arch)
}

// BinaryFor returns the named binary for one platform.
func (m *Manifest) BinaryFor(name, os, arch string) (Binary, bool) {
	binaries := make([]Binary, 0, len(m.Spec.Binaries)+len(m.Spec.Installer.Binaries))
	binaries = append(binaries, m.Spec.Binaries...)
	binaries = append(binaries, m.Spec.Installer.Binaries...)
	for _, b := range binaries {
		if b.Name == name && b.OS == os && b.Arch == arch {
			return b, true
		}
	}
	return Binary{}, false
}

func filterBinaries(binaries []Binary, os, arch string) []Binary {
	out := make([]Binary, 0, len(binaries))
	for _, b := range binaries {
		if b.OS == os && b.Arch == arch {
			out = append(out, b)
		}
	}
	return out
}

type Registry struct {
	// Namespace is the image namespace kube-do uses inside the private Registry.
	Namespace string `yaml:"namespace"`
}

// Installer holds Artifacts needed only on the Installer Host.
type Installer struct {
	Binaries []Binary `yaml:"binaries"`
	// Images are runtime images assembled from Installer binaries. They are
	// pushed to the private Registry and pulled by cluster nodes.
	Images []InstallerImage `yaml:"images,omitempty"`
}

// InstallerBinaryFor returns the named Installer Host binary for one platform.
func (m *Manifest) InstallerBinaryFor(name, os, arch string) (Binary, bool) {
	for _, b := range m.Spec.Installer.Binaries {
		if b.Name == name && b.OS == os && b.Arch == arch {
			return b, true
		}
	}
	return Binary{}, false
}

// InstallerImagesFor returns generated runtime images for one platform.
func (m *Manifest) InstallerImagesFor(os, arch string) []InstallerImage {
	out := make([]InstallerImage, 0, len(m.Spec.Installer.Images))
	for _, img := range m.Spec.Installer.Images {
		if img.OS == os && img.Arch == arch {
			out = append(out, img)
		}
	}
	return out
}

// Binary is a downloadable executable or archive.
type Binary struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
	OS      string `yaml:"os"`
	Arch    string `yaml:"arch"`
	URL     string `yaml:"url"`
	SHA256  string `yaml:"sha256,omitempty"`
	// Archive is empty for a raw file, or tar.gz for an archive.
	Archive string `yaml:"archive,omitempty"`
	// Build turns URL into an upstream source archive and names the recipe used
	// to produce this binary. SHA256 is empty for built binaries; SourceSHA256
	// pins the downloaded source archive instead.
	Build        string `yaml:"build,omitempty"`
	SourceSHA256 string `yaml:"sourceSha256,omitempty"`
}

// IsBuilt reports whether this Binary is produced from a pinned source archive.
func (b Binary) IsBuilt() bool { return b.Build != "" }

// Image is a container image copied from Source to the private Registry.
type Image struct {
	Name   string `yaml:"name"`
	Source string `yaml:"source"`
	// Target is a repository:tag path relative to <registry>/<namespace>.
	Target string `yaml:"target"`
}

// InstallerImage is a container image assembled on the Installer Host from one
// of its pinned binaries and that binary's runtime libraries.
type InstallerImage struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
	OS      string `yaml:"os"`
	Arch    string `yaml:"arch"`
	// Target is a repository:tag path relative to <registry>/<namespace>.
	Target string `yaml:"target"`
	// Binary names the Installer Host binary copied into this image.
	Binary string `yaml:"binary"`
}

// Parse decodes and validates a Resource Manifest.
func Parse(data []byte) (*Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("resource manifest is empty")
		}
		return nil, fmt.Errorf("decode resource manifest: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("decode trailing resource manifest document: %w", err)
		}
		return nil, errors.New("resource manifest must contain exactly one YAML document")
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// LoadFile reads and validates a Resource Manifest from disk.
func LoadFile(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read resource manifest: %w", err)
	}
	return Parse(data)
}

// TargetRef returns the full image reference inside the private Registry.
func (i Image) TargetRef(registry, namespace string) (string, error) {
	return imageTargetRef(i.Name, i.Target, registry, namespace)
}

// TargetRef returns the full image reference inside the private Registry.
func (i InstallerImage) TargetRef(registry, namespace string) (string, error) {
	return imageTargetRef(i.Name, i.Target, registry, namespace)
}

func imageTargetRef(name, targetRef, registry, namespace string) (string, error) {
	if registry == "" {
		return "", errors.New("registry address is empty")
	}
	if namespace == "" {
		return "", errors.New("registry namespace is empty")
	}
	target := targetRef
	for len(target) > 0 && target[0] == '/' {
		target = target[1:]
	}
	if target == "" {
		return "", fmt.Errorf("image %q has an empty target", name)
	}
	return registry + "/" + namespace + "/" + target, nil
}
