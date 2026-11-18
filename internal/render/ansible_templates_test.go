package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCiliumValuesUseMirroredOperatorImageOverride(t *testing.T) {
	path := filepath.Join("..", "..", "ansible", "roles", "cilium", "templates", "values.yaml.j2")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `override: "{{ kube_do_operator_image.repository }}:{{ kube_do_operator_image.tag }}"`) {
		t.Fatal("Cilium operator image must use the complete mirrored operator image reference")
	}
	if strings.Contains(string(data), "/cilium/operator\n") {
		t.Fatal("Cilium operator repository must not bypass the manifest target")
	}
	if strings.Contains(string(data), "repository: {{ kube_do_operator_image.repository }}") {
		t.Fatal("Cilium operator repository must not rely on chart-specific image suffixing")
	}
}

func TestHARoleSupportsPrebuiltImagesWithoutFallbackBinaries(t *testing.T) {
	path := filepath.Join("..", "..", "ansible", "roles", "ha", "tasks", "main.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, want := range []string{
		"kube_do_haproxy_image.source is defined or installer_binaries",
		"kube_do_keepalived_image.source is defined or installer_binaries",
		`retries: "{{ kube_do_ha_container_ready_retries }}"`,
		`retries: "{{ kube_do_ha_vip_ready_retries }}"`,
		`retries: "{{ kube_do_ha_systemd_retries }}"`,
		`daemon_reload: "{{ kube_do_systemd_daemon_reload }}"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("HA role is missing %q", want)
		}
	}
}

func TestKubeletUnitWaitsForKubeadmConfiguration(t *testing.T) {
	path := filepath.Join("..", "..", "ansible", "roles", "kubernetes", "templates", "kubelet.service.j2")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.Contains(body, "ConditionPathExists=/var/lib/kubelet/config.yaml") {
		t.Fatal("kubelet must not start before kubeadm writes its configuration")
	}
	if !strings.Contains(body, "--config=/var/lib/kubelet/config.yaml") {
		t.Fatal("kubelet unit must use the kubeadm-generated configuration")
	}
}
