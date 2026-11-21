package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kube-dosh/kube-do/internal/cluster"
	"github.com/kube-dosh/kube-do/internal/manifest"
)

func lifecycleTestConfig(t *testing.T) (*cluster.Config, *manifest.Manifest) {
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
    - name: cp-2
      address: 10.0.0.12
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
	return cfg, m
}

func TestRenderAddControlPlanes(t *testing.T) {
	cfg, m := lifecycleTestConfig(t)
	cfg.ControlPlane.Nodes = append(cfg.ControlPlane.Nodes, cluster.Node{Name: "cp-3", Address: "10.0.0.13"})
	out := t.TempDir()
	if _, err := RenderAddControlPlanes(cfg, []cluster.Node{{Name: "cp-3", Address: "10.0.0.13"}}, m, out); err != nil {
		t.Fatalf("RenderAddControlPlanes: %v", err)
	}
	inventory, err := os.ReadFile(filepath.Join(out, "inventory.ini"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[control_plane]\ncp-1 ",
		"cp-2 ",
		"cp-3 ",
		"[new_control_plane]\ncp-3 ",
		"[k8s_cluster:children]\ncontrol_plane\nworkers",
	} {
		if !strings.Contains(string(inventory), want) {
			t.Fatalf("inventory missing %q:\n%s", want, inventory)
		}
	}
}

func TestRenderRemoveControlPlane(t *testing.T) {
	cfg, m := lifecycleTestConfig(t)
	out := t.TempDir()
	if _, err := RenderRemoveNode(cfg, cfg.ControlPlane.Nodes[1], "control-plane", m, out); err != nil {
		t.Fatalf("RenderRemoveNode: %v", err)
	}
	inventory, err := os.ReadFile(filepath.Join(out, "inventory.ini"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[remove_nodes]\ncp-2 ",
		"[remaining_control_plane]\ncp-1 ",
	} {
		if !strings.Contains(string(inventory), want) {
			t.Fatalf("inventory missing %q:\n%s", want, inventory)
		}
	}
	if strings.Contains(string(inventory), "[remaining_control_plane]\ncp-1 ansible_host=10.0.0.11 ansible_user=root ansible_port=22 kube_do_node_ip=10.0.0.11\ncp-2") {
		t.Fatalf("removed node remained in remaining_control_plane:\n%s", inventory)
	}
}
