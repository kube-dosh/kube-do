package bump

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/kube-dosh/kube-do/internal/manifest"
	"github.com/kube-dosh/kube-do/manifests"
)

const testSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func baseManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	m, err := manifests.Get("v1.36.5-kube-do.1")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDefaultBuildName(t *testing.T) {
	cases := []struct {
		base string
		k8s  string
		want string
	}{
		{"v1.36.5-kube-do.1", "v1.36.5", "v1.36.5-kube-do.2"},
		{"v1.36.5-kube-do.7", "1.36.5", "v1.36.5-kube-do.8"},
		{"v1.36.5-kube-do.1", "v1.37.1", "v1.37.1-kube-do.1"},
	}
	for _, tc := range cases {
		if got := DefaultBuildName(tc.base, tc.k8s); got != tc.want {
			t.Errorf("DefaultBuildName(%q, %q) = %q, want %q", tc.base, tc.k8s, got, tc.want)
		}
	}
}

func TestComponentVersionAndURL(t *testing.T) {
	tests := []struct {
		binary  manifest.Binary
		input   string
		version string
		url     string
		built   bool
	}{
		{manifest.Binary{Name: "containerd", Arch: "amd64"}, "2.3.7", "2.3.7", "https://github.com/containerd/containerd/releases/download/v2.3.7/containerd-2.3.7-linux-amd64.tar.gz", false},
		{manifest.Binary{Name: "helm", Arch: "amd64"}, "v3.23.0", "v3.23.0", "https://get.helm.sh/helm-v3.23.0-linux-amd64.tar.gz", false},
		{manifest.Binary{Name: "haproxy", Arch: "amd64"}, "3.2.27", "3.2.27", "https://www.haproxy.org/download/3.2/src/haproxy-3.2.27.tar.gz", true},
	}
	for _, tc := range tests {
		version, url, built := componentVersionAndURL(tc.binary, tc.input)
		if version != tc.version || url != tc.url || built != tc.built {
			t.Errorf("componentVersionAndURL(%s, %s) = %q, %q, %v", tc.binary.Name, tc.input, version, url, built)
		}
	}
}

func TestRunBumpsContainerd(t *testing.T) {
	m := baseManifest(t)
	result, err := Run(context.Background(), Options{
		Base:     m,
		Versions: Versions{Containerd: "2.3.7"},
		HashURL: func(context.Context, string) (string, error) {
			return testSHA, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Build != "v1.36.5-kube-do.2" {
		t.Fatalf("build = %q", result.Build)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		b, ok := result.Manifest.BinaryFor("containerd", "linux", arch)
		if !ok {
			t.Fatalf("containerd %s not found", arch)
		}
		if b.Version != "2.3.7" || b.SHA256 != testSHA || !strings.Contains(b.URL, "containerd-2.3.7-linux-"+arch) {
			t.Fatalf("containerd %s = %#v", arch, b)
		}
	}
}

func TestRunBumpsKeepalivedInstallerImages(t *testing.T) {
	m := baseManifest(t)
	result, err := Run(context.Background(), Options{
		Base:     m,
		Versions: Versions{Keepalived: "2.4.4"},
		HashURL: func(context.Context, string) (string, error) {
			return testSHA, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		binary, ok := result.Manifest.InstallerBinaryFor("keepalived", "linux", arch)
		if !ok || binary.Version != "2.4.4" {
			t.Fatalf("keepalived %s = %#v, %v", arch, binary, ok)
		}
		images := result.Manifest.InstallerImagesFor("linux", arch)
		image, ok := installerImageFor(images, "keepalived")
		if !ok || image.Version != "2.4.4" || image.Target != "keepalived:2.4.4-"+arch {
			t.Fatalf("keepalived images %s = %#v", arch, images)
		}
	}
}

func TestRunBumpsHAProxyInstallerImages(t *testing.T) {
	m := baseManifest(t)
	result, err := Run(context.Background(), Options{
		Base:     m,
		Versions: Versions{HAProxy: "3.2.27"},
		HashURL: func(context.Context, string) (string, error) {
			return testSHA, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		binary, ok := result.Manifest.InstallerBinaryFor("haproxy", "linux", arch)
		if !ok || binary.Version != "3.2.27" {
			t.Fatalf("haproxy %s = %#v, %v", arch, binary, ok)
		}
		images := result.Manifest.InstallerImagesFor("linux", arch)
		image, ok := installerImageFor(images, "haproxy")
		if !ok || image.Version != "3.2.27" || image.Target != "haproxy:3.2.27-"+arch {
			t.Fatalf("haproxy images %s = %#v", arch, images)
		}
	}
}

func installerImageFor(images []manifest.InstallerImage, name string) (manifest.InstallerImage, bool) {
	for _, image := range images {
		if image.Name == name {
			return image, true
		}
	}
	return manifest.InstallerImage{}, false
}

func TestRunBumpsKubernetesAndImages(t *testing.T) {
	m := baseManifest(t)
	result, err := Run(context.Background(), Options{
		Base:     m,
		Versions: Versions{Kubernetes: "v1.37.1"},
		DownloadFile: func(_ context.Context, _ string, dest string) (string, error) {
			return testSHA, os.WriteFile(dest, []byte("kubeadm"), 0o600)
		},
		KubeadmImages: func(context.Context, string, string) ([]string, error) {
			return []string{
				"registry.k8s.io/kube-apiserver:v1.37.1",
				"registry.k8s.io/kube-controller-manager:v1.37.1",
				"registry.k8s.io/kube-scheduler:v1.37.1",
				"registry.k8s.io/kube-proxy:v1.37.1",
				"registry.k8s.io/coredns/coredns:v1.15.0",
				"registry.k8s.io/pause:3.11.0",
				"registry.k8s.io/etcd:3.6.9-0",
			}, nil
		},
		HashURL: func(context.Context, string) (string, error) {
			return testSHA, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Build != "v1.37.1-kube-do.1" || result.Manifest.Metadata.KubernetesVersion != "v1.37.1" {
		t.Fatalf("result metadata = %#v", result.Manifest.Metadata)
	}
	for _, name := range []string{"kubeadm", "kubelet", "kubectl"} {
		for _, arch := range []string{"amd64", "arm64"} {
			b, ok := result.Manifest.BinaryFor(name, "linux", arch)
			if !ok {
				t.Fatalf("%s %s not found", name, arch)
			}
			if b.Version != "v1.37.1" || b.SHA256 != testSHA || !strings.Contains(b.URL, "/linux/"+arch+"/") {
				t.Fatalf("%s %s = %#v", name, arch, b)
			}
		}
	}
	for _, img := range result.Manifest.Spec.Images {
		if img.Name == "coredns" && img.Source != "registry.k8s.io/coredns/coredns:v1.15.0" {
			t.Fatalf("coredns = %#v", img)
		}
		if img.Name == "etcd" && img.Target != "etcd:3.6.9-0" {
			t.Fatalf("etcd = %#v", img)
		}
	}
}
