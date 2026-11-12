package manifest

import (
	"strings"
	"testing"
)

const testSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func validManifestYAML() string {
	return `apiVersion: kube-do.io/v1alpha1
kind: ResourceManifest
metadata:
  name: test-build
  kubernetesVersion: v1.36.5
spec:
  platform:
    os: linux
    arch: amd64
  registry:
    namespace: kube-do
  installer:
    binaries: []
  binaries:
    - name: kubeadm
      version: v1.36.5
      os: linux
      arch: amd64
      url: https://example.com/kubeadm
      sha256: ` + testSHA + `
  images:
    - name: pause
      source: registry.k8s.io/pause:3.10.2
      target: pause:3.10.2
`
}

func TestParseValid(t *testing.T) {
	m, err := Parse([]byte(validManifestYAML()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Metadata.Name != "test-build" {
		t.Fatalf("name = %q", m.Metadata.Name)
	}
	if len(m.Spec.Binaries) != 1 || m.Spec.Binaries[0].Name != "kubeadm" {
		t.Fatalf("unexpected binaries: %#v", m.Spec.Binaries)
	}
}

func TestParseRejectsMultipleYAMLDocuments(t *testing.T) {
	_, err := Parse([]byte(validManifestYAML() + "---\nmetadata:\n  name: smuggled\n"))
	if err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("expected multiple document error, got %v", err)
	}
}

func TestValidateRejectsBadSHA(t *testing.T) {
	_, err := Parse([]byte(strings.Replace(validManifestYAML(), testSHA, "not-a-sha", 1)))
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("expected sha256 error, got %v", err)
	}
}

func TestValidateRejectsPlatformMismatch(t *testing.T) {
	bad := strings.Replace(validManifestYAML(), "      arch: amd64\n      url: https://example.com/kubeadm", "      arch: arm64\n      url: https://example.com/kubeadm", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "platform linux/arm64 is not declared") {
		t.Fatalf("expected platform error, got %v", err)
	}
}

func multiPlatformManifestYAML() string {
	yaml := strings.Replace(validManifestYAML(), "  platform:\n    os: linux\n    arch: amd64\n", "  platforms:\n    - os: linux\n      arch: amd64\n    - os: linux\n      arch: arm64\n", 1)
	arm64Binary := "    - name: kubeadm\n      version: v1.36.5\n      os: linux\n      arch: arm64\n      url: https://example.com/kubeadm-arm64\n      sha256: " + testSHA + "\n"
	return strings.Replace(yaml, "  images:\n", arm64Binary+"  images:\n", 1)
}

func TestParseMultiPlatform(t *testing.T) {
	m, err := Parse([]byte(multiPlatformManifestYAML()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !m.SupportsPlatform("linux", "amd64") || !m.SupportsPlatform("linux", "arm64") {
		t.Fatalf("supported platforms = %#v", m.SupportedPlatforms())
	}
	arm := m.BinariesFor("linux", "arm64")
	if len(arm) != 1 || arm[0].Arch != "arm64" {
		t.Fatalf("arm64 binaries = %#v", arm)
	}
}

func multiPlatformInstallerImageManifestYAML() string {
	binaries := `  installer:
    binaries:
      - name: registry
        version: 3.1.2
        os: linux
        arch: amd64
        url: https://example.com/registry-amd64
        sha256: ` + testSHA + `
      - name: registry
        version: 3.1.2
        os: linux
        arch: arm64
        url: https://example.com/registry-arm64
        sha256: ` + testSHA + `
`
	images := `
    images:
      - name: registry-runtime
        version: 3.1.2
        os: linux
        arch: amd64
        target: registry-runtime:3.1.2-amd64
        binary: registry
      - name: registry-runtime
        version: 3.1.2
        os: linux
        arch: arm64
        target: registry-runtime:3.1.2-arm64
        binary: registry
`
	base := strings.Replace(multiPlatformManifestYAML(), "  installer:\n    binaries: []\n", binaries, 1)
	return strings.Replace(base, "\n  binaries:\n", images+"  binaries:\n", 1)
}

func TestParseInstallerImages(t *testing.T) {
	m, err := Parse([]byte(multiPlatformInstallerImageManifestYAML()))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	images := m.InstallerImagesFor("linux", "arm64")
	if len(images) != 1 || images[0].Name != "registry-runtime" || images[0].Binary != "registry" {
		t.Fatalf("Installer images = %#v", images)
	}
	binary, ok := m.InstallerBinaryFor("registry", "linux", "arm64")
	if !ok || binary.Version != "3.1.2" {
		t.Fatalf("Installer binary = %#v, %v", binary, ok)
	}
}

func TestValidateRejectsInstallerImageWithoutBinary(t *testing.T) {
	bad := strings.Replace(multiPlatformInstallerImageManifestYAML(), "        binary: registry\n      - name: registry-runtime\n        version: 3.1.2\n        os: linux\n        arch: arm64", "        binary: missing\n      - name: registry-runtime\n        version: 3.1.2\n        os: linux\n        arch: arm64", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "is not an Installer binary") {
		t.Fatalf("expected missing binary error, got %v", err)
	}
}

func TestValidateRejectsInstallerImageVersionDrift(t *testing.T) {
	bad := strings.Replace(multiPlatformInstallerImageManifestYAML(), "        version: 3.1.2\n        os: linux\n        arch: arm64", "        version: 3.1.1\n        os: linux\n        arch: arm64", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "differs from Installer binary") {
		t.Fatalf("expected binary version error, got %v", err)
	}
}

func TestValidateRejectsDuplicateInstallerImageTarget(t *testing.T) {
	bad := strings.Replace(multiPlatformInstallerImageManifestYAML(), "target: registry-runtime:3.1.2-arm64", "target: registry-runtime:3.1.2-amd64", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "target") || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("expected duplicate target error, got %v", err)
	}
}

func TestValidateRejectsUnversionedInstallerImageTarget(t *testing.T) {
	bad := strings.Replace(multiPlatformInstallerImageManifestYAML(), "target: registry-runtime:3.1.2-arm64", "target: registry-runtime:latest", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "target tag") {
		t.Fatalf("expected target tag error, got %v", err)
	}
}

func TestValidateRejectsMissingMultiPlatformBinary(t *testing.T) {
	yaml := multiPlatformManifestYAML()
	arm64Binary := "    - name: kubeadm\n      version: v1.36.5\n      os: linux\n      arch: arm64\n      url: https://example.com/kubeadm-arm64\n      sha256: " + testSHA + "\n"
	yaml = strings.Replace(yaml, arm64Binary, "", 1)
	_, err := Parse([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "does not provide the same binary set") {
		t.Fatalf("expected binary set error, got %v", err)
	}
}

func TestValidateRejectsMultiPlatformVersionDrift(t *testing.T) {
	yaml := strings.Replace(multiPlatformManifestYAML(), "version: v1.36.5\n      os: linux\n      arch: arm64", "version: v1.36.4\n      os: linux\n      arch: arm64", 1)
	_, err := Parse([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "differ") || !strings.Contains(err.Error(), "one version per Artifact") {
		t.Fatalf("expected version drift error, got %v", err)
	}
}

func TestValidateRejectsDuplicatePlatform(t *testing.T) {
	yaml := strings.Replace(multiPlatformManifestYAML(), "    - os: linux\n      arch: arm64", "    - os: linux\n      arch: amd64", 1)
	_, err := Parse([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "duplicates platform linux/amd64") {
		t.Fatalf("expected duplicate platform error, got %v", err)
	}
}

func TestValidateRejectsPlatformFieldsTogether(t *testing.T) {
	yaml := strings.Replace(multiPlatformManifestYAML(), "  platforms:\n", "  platform:\n    os: linux\n    arch: amd64\n  platforms:\n", 1)
	_, err := Parse([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutual exclusion error, got %v", err)
	}
}

func TestValidateRejectsUnpinnedImage(t *testing.T) {
	bad := strings.Replace(validManifestYAML(), "source: registry.k8s.io/pause:3.10.2", "source: registry.k8s.io/pause", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "pinned with a tag") {
		t.Fatalf("expected tag error, got %v", err)
	}
}

func TestValidateRejectsDuplicateImage(t *testing.T) {
	bad := strings.Replace(validManifestYAML(), `    - name: pause
      source: registry.k8s.io/pause:3.10.2
      target: pause:3.10.2`,
		`    - name: pause
      source: registry.k8s.io/pause:3.10.2
      target: pause:3.10.2
    - name: pause
      source: registry.k8s.io/pause:3.10.2
      target: pause:3.10.2`, 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "duplicate image name") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
}

func TestValidateRejectsDuplicateImageTarget(t *testing.T) {
	bad := strings.Replace(validManifestYAML(), `    - name: pause
      source: registry.k8s.io/pause:3.10.2
      target: pause:3.10.2`,
		`    - name: pause
      source: registry.k8s.io/pause:3.10.2
      target: pause:3.10.2
    - name: coredns
      source: registry.k8s.io/coredns/coredns:v1.14.2
      target: pause:3.10.2`, 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("expected duplicate target error, got %v", err)
	}
}

func TestValidateRejectsImageTargetCollidingWithInstallerImage(t *testing.T) {
	bad := strings.Replace(validManifestYAML(), `  installer:
    binaries: []`, `  installer:
    binaries:
      - name: registry
        version: 3.1.2
        os: linux
        arch: amd64
        url: https://example.com/registry
        sha256: `+testSHA+`
    images:
      - name: registry-runtime
        version: 3.1.2
        os: linux
        arch: amd64
        target: pause:3.10.2
        binary: registry`, 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("expected cross-kind target collision error, got %v", err)
	}
}

func TestValidateRejectsUnsafeBuildName(t *testing.T) {
	bad := strings.Replace(validManifestYAML(), "name: test-build", "name: ../escape", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "safe single path segment") {
		t.Fatalf("expected build name error, got %v", err)
	}
}

func TestValidateRejectsUnsafeNamespace(t *testing.T) {
	bad := strings.Replace(validManifestYAML(), "namespace: kube-do", "namespace: ../escape", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "repository namespace") {
		t.Fatalf("expected namespace error, got %v", err)
	}
}

func TestValidateRejectsTraversingImagePath(t *testing.T) {
	bad := strings.Replace(validManifestYAML(), "target: pause:3.10.2", "target: ../pause:3.10.2", 1)
	_, err := Parse([]byte(bad))
	if err == nil || !strings.Contains(err.Error(), "unsafe repository path") {
		t.Fatalf("expected unsafe image path error, got %v", err)
	}
}

func TestValidateRejectsUnsafeBinaryNameAndVersion(t *testing.T) {
	for _, tc := range []struct {
		name string
		from string
		to   string
		want string
	}{
		{"name", "name: kubeadm", "name: ../kubeadm", "safe file name"},
		{"version", "version: v1.36.5", "version: ../1.36.5", "version must contain"},
		{"os", "  platform:\n    os: linux", "  platform:\n    os: ../linux", "platforms[0].os"},
	} {
		bad := strings.Replace(validManifestYAML(), tc.from, tc.to, 1)
		_, err := Parse([]byte(bad))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: expected %q error, got %v", tc.name, tc.want, err)
		}
	}
}

func TestTargetRef(t *testing.T) {
	img := Image{Name: "pause", Source: "registry.k8s.io/pause:3.10.2", Target: "pause:3.10.2"}
	got, err := img.TargetRef("reg.internal:5000", "kube-do")
	if err != nil {
		t.Fatalf("TargetRef: %v", err)
	}
	if want := "reg.internal:5000/kube-do/pause:3.10.2"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
