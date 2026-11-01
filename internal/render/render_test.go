package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kubedo/kubedo/internal/cluster"
	"github.com/kubedo/kubedo/internal/manifest"
)

func TestRender(t *testing.T) {
	cfg, err := cluster.Parse([]byte(`apiVersion: kubedo.io/v1alpha1
kind: ClusterConfig
metadata:
  name: test
build: test-build
platform:
  osFamily: debian
  arch: arm64
installer:
  address: 10.0.0.10
registry:
  address: 10.0.0.10:5000
ssh:
  user: installer
  port: 2222
  privateKeyFile: /root/.ssh/kubedo
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
	m, err := manifest.Parse([]byte(`apiVersion: kubedo.io/v1alpha1
kind: ResourceManifest
metadata:
  name: test-build
  kubernetesVersion: v1.36.5
spec:
  platforms:
    - os: linux
      arch: amd64
    - os: linux
      arch: arm64
  registry:
    namespace: kubedo
  installer:
    binaries:
      - name: registry
        version: 3.1.2
        os: linux
        arch: amd64
        url: https://example.com/registry-amd64
        sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
        archive: tar.gz
      - name: registry
        version: 3.1.2
        os: linux
        arch: arm64
        url: https://example.com/registry-arm64
        sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
        archive: tar.gz
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
  binaries:
    - name: kubeadm
      version: v1.36.5
      os: linux
      arch: amd64
      url: https://example.com/kubeadm
      sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    - name: kubeadm
      version: v1.36.5
      os: linux
      arch: arm64
      url: https://example.com/kubeadm-arm64
      sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
  images:
    - name: pause
      source: registry.k8s.io/pause:3.10.2
      target: pause:3.10.2
`))
	if err != nil {
		t.Fatalf("manifest.Parse: %v", err)
	}

	out := t.TempDir()
	files, err := Render(cfg, m, out)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %v", files)
	}

	inventory, err := os.ReadFile(filepath.Join(out, "inventory.ini"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[installer]",
		"cp-1 ansible_host=10.0.0.11 ansible_user=installer ansible_port=2222 kubedo_node_ip=10.0.0.11 ansible_ssh_private_key_file=/root/.ssh/kubedo",
		"worker-1 ansible_host=10.0.0.21 ansible_user=installer ansible_port=2222 kubedo_node_ip=10.0.0.21 ansible_ssh_private_key_file=/root/.ssh/kubedo",
		"[k8s_cluster:children]",
	} {
		if !strings.Contains(string(inventory), want) {
			t.Fatalf("inventory missing %q:\n%s", want, inventory)
		}
	}

	data, err := os.ReadFile(filepath.Join(out, "group_vars", "all.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var vars map[string]any
	if err := yaml.Unmarshal(data, &vars); err != nil {
		t.Fatalf("group vars are not valid YAML: %v", err)
	}
	if vars["kubedo_build"] != "test-build" {
		t.Fatalf("kubedo_build = %v", vars["kubedo_build"])
	}
	if vars["control_plane_endpoint"] != "https://10.0.0.100:16443" {
		t.Fatalf("endpoint = %v", vars["control_plane_endpoint"])
	}
	if vars["download_server"] != "http://10.0.0.10:8080" {
		t.Fatalf("download_server = %v", vars["download_server"])
	}
	if vars["platform_os_family"] != "debian" || vars["platform_arch"] != "arm64" {
		t.Fatalf("platform vars = %v/%v", vars["platform_os_family"], vars["platform_arch"])
	}
	binaries, ok := vars["binaries"].([]any)
	if !ok || len(binaries) != 1 {
		t.Fatalf("binaries = %#v", vars["binaries"])
	}
	binary := binaries[0].(map[string]any)
	if binary["arch"] != "arm64" || binary["url"] != "http://10.0.0.10:8080/artifacts/test-build/linux-arm64/kubeadm" {
		t.Fatalf("rendered binary = %#v", binary)
	}
	installerBinaries, ok := vars["installer_binaries"].([]any)
	if !ok || len(installerBinaries) != 1 || installerBinaries[0].(map[string]any)["arch"] != "arm64" {
		t.Fatalf("installer binaries = %#v", vars["installer_binaries"])
	}
	installerImages, ok := vars["installer_images"].([]any)
	if !ok || len(installerImages) != 1 {
		t.Fatalf("installer images = %#v", vars["installer_images"])
	}
	installerImage := installerImages[0].(map[string]any)
	if installerImage["target"] != "10.0.0.10:5000/kubedo/registry-runtime:3.1.2-arm64" {
		t.Fatalf("installer image target = %v", installerImage["target"])
	}
	if installerImage["repository"] != "10.0.0.10:5000/kubedo/registry-runtime" || installerImage["tag"] != "3.1.2-arm64" {
		t.Fatalf("installer image repository/tag = %v/%v", installerImage["repository"], installerImage["tag"])
	}
	images, ok := vars["images"].([]any)
	if !ok || len(images) != 1 {
		t.Fatalf("images = %#v", vars["images"])
	}
	img := images[0].(map[string]any)
	if img["target"] != "10.0.0.10:5000/kubedo/pause:3.10.2" {
		t.Fatalf("image target = %v", img["target"])
	}
	if img["repository"] != "10.0.0.10:5000/kubedo/pause" || img["tag"] != "3.10.2" {
		t.Fatalf("image repository/tag = %v/%v", img["repository"], img["tag"])
	}
	if vars["ssh_user"] != "installer" || vars["ssh_port"] != 2222 || vars["pod_cidr"] != "10.244.0.0/16" {
		t.Fatalf("rendered defaults = %#v", vars)
	}
}
