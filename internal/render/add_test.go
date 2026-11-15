package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kube-dosh/kube-do/internal/cluster"
	"github.com/kube-dosh/kube-do/internal/manifest"
)

func TestRenderAddWorkers(t *testing.T) {
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
workers:
  - name: worker-1
    address: 10.0.0.21
cni:
  name: cilium
  kubeProxyReplacement: true
`))
	if err != nil {
		t.Fatalf("cluster.Parse: %v", err)
	}
	m := &manifest.Manifest{
		Metadata: manifest.Metadata{Name: "test-build", KubernetesVersion: "v1.36.5"},
		Spec: manifest.Spec{
			Platforms: []manifest.Platform{{OS: "linux", Arch: "amd64"}},
			Registry:  manifest.Registry{Namespace: "kube-do"},
			Binaries: []manifest.Binary{{
				Name: "kubeadm", Version: "v1.36.5", OS: "linux", Arch: "amd64",
				URL: "https://example.com/kubeadm", SHA256: strings.Repeat("a", 64),
			}},
			Installer: manifest.Installer{Binaries: []manifest.Binary{{
				Name: "registry", Version: "3.1.2", OS: "linux", Arch: "amd64",
				URL: "https://example.com/registry", SHA256: strings.Repeat("b", 64),
			}}},
		},
	}

	out := t.TempDir()
	files, err := RenderAddWorkers(cfg, []cluster.Node{
		{Name: "worker-2", Address: "10.0.0.22"},
		{Name: "worker-3", Address: "10.0.0.23"},
	}, m, out)
	if err != nil {
		t.Fatalf("RenderAddWorkers: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %v", files)
	}
	inventory, err := os.ReadFile(filepath.Join(out, "inventory.ini"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[control_plane]\ncp-1 ",
		"[workers]\nworker-1 ",
		"[new_workers]\nworker-2 ",
		"worker-3 ",
		"[k8s_cluster:children]\ncontrol_plane\nworkers\nnew_workers",
	} {
		if !strings.Contains(string(inventory), want) {
			t.Fatalf("inventory missing %q:\n%s", want, inventory)
		}
	}
}
