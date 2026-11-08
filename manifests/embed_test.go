package manifests

import (
	"reflect"
	"strings"
	"testing"
)

func TestEmbeddedBuilds(t *testing.T) {
	names, err := Names()
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("Names returned no embedded builds")
	}
	const build = "v1.36.5-kube-do.1"
	found := false
	for _, name := range names {
		if name == build {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("embedded Build %q not found in %v", build, names)
	}

	m, err := Get(build)
	if err != nil {
		t.Fatalf("Get(%q): %v", build, err)
	}
	if m.Metadata.Name != build {
		t.Fatalf("manifest name = %q, want %q", m.Metadata.Name, build)
	}
	if m.Metadata.KubernetesVersion != "v1.36.5" {
		t.Fatalf("kubernetesVersion = %q", m.Metadata.KubernetesVersion)
	}
	for _, target := range []struct{ os, arch string }{{"linux", "amd64"}, {"linux", "arm64"}} {
		if !m.SupportsPlatform(target.os, target.arch) {
			t.Fatalf("embedded build does not support %s/%s", target.os, target.arch)
		}
		if len(m.BinariesFor(target.os, target.arch)) == 0 {
			t.Fatalf("embedded build has no node binaries for %s/%s", target.os, target.arch)
		}
		if len(m.InstallerBinariesFor(target.os, target.arch)) == 0 {
			t.Fatalf("embedded build has no installer binaries for %s/%s", target.os, target.arch)
		}
		images := m.InstallerImagesFor(target.os, target.arch)
		if len(images) != 2 {
			t.Fatalf("embedded build installer images for %s/%s = %#v", target.os, target.arch, images)
		}
		seenImages := map[string]bool{}
		for _, image := range images {
			seenImages[image.Name] = true
			if image.Binary != image.Name {
				t.Fatalf("installer image %q uses binary %q for %s/%s", image.Name, image.Binary, target.os, target.arch)
			}
		}
		for _, name := range []string{"haproxy", "keepalived"} {
			if !seenImages[name] {
				t.Fatalf("embedded build has no %s installer image for %s/%s", name, target.os, target.arch)
			}
			if _, ok := m.InstallerBinaryFor(name, target.os, target.arch); !ok {
				t.Fatalf("embedded build has no %s installer binary for %s/%s", name, target.os, target.arch)
			}
			for _, binary := range m.BinariesFor(target.os, target.arch) {
				if binary.Name == name {
					t.Fatalf("embedded build distributes %s as a node binary for %s/%s", name, target.os, target.arch)
				}
			}
		}
	}
}

func TestUnknownBuild(t *testing.T) {
	_, err := Get("v0.0.0-kube-do.999")
	if err == nil || !strings.Contains(err.Error(), "unknown build") {
		t.Fatalf("expected unknown build error, got %v", err)
	}
}

func TestSortBuildNames(t *testing.T) {
	names := []string{
		"v1.36.5-kube-do.10",
		"custom-build",
		"v1.36.5-kube-do.2",
		"v1.37.0-kube-do.1",
		"v1.36.4-kube-do.9",
	}
	sortBuildNames(names)
	want := []string{
		"v1.36.4-kube-do.9",
		"v1.36.5-kube-do.2",
		"v1.36.5-kube-do.10",
		"v1.37.0-kube-do.1",
		"custom-build",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}
