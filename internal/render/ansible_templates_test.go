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

func TestHAContainerLaunchersBlockWithoutBusyPolling(t *testing.T) {
	for _, name := range []string{"kube-do-haproxy.sh.j2", "kube-do-keepalived.sh.j2"} {
		path := filepath.Join("..", "..", "ansible", "roles", "ha", "templates", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		body := string(data)
		if !strings.Contains(body, "run --rm --null-io") {
			t.Fatalf("%s must run the container in the foreground", name)
		}
		if strings.Contains(body, "tasks ps") || strings.Contains(body, "--detach") {
			t.Fatalf("%s must not poll container state or detach", name)
		}
		if !strings.Contains(body, "rm -rf \"$runtime_dir\"") {
			t.Fatalf("%s must clear a stale containerd runtime state directory", name)
		}
		if !strings.Contains(body, "umount -l \"$runtime_dir/rootfs\"") {
			t.Fatalf("%s must detach a stale runtime rootfs before cleanup", name)
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

func TestKubeadmInitWaitsForCRI(t *testing.T) {
	path := filepath.Join("..", "..", "ansible", "roles", "kubeadm_init", "tasks", "main.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, want := range []string{
		"Wait for the container runtime CRI endpoint",
		"/usr/bin/timeout",
		"kube_do_kubeadm_cri_ready_retries",
		"until: kube_do_kubeadm_cri_ready.rc == 0",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("kubeadm init is missing CRI readiness guard %q", want)
		}
	}
}

func TestKubeadmConfigSetsRuntimeRequestTimeout(t *testing.T) {
	path := filepath.Join("..", "..", "ansible", "roles", "kubeadm_init", "templates", "kubeadm-config.yaml.j2")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "runtimeRequestTimeout: {{ kube_do_kubelet_runtime_request_timeout }}") {
		t.Fatal("kubelet runtime request timeout must be configurable")
	}
}

func TestKubeadmConfigSetsKubeletPollingFrequencies(t *testing.T) {
	defaultsPath := filepath.Join("..", "..", "ansible", "roles", "kubeadm_init", "defaults", "main.yml")
	defaults, err := os.ReadFile(defaultsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"kube_do_kubelet_sync_frequency: 1m",
		"kube_do_kubelet_file_check_frequency: 20s",
		"kube_do_kubelet_node_status_update_frequency: 30s",
		"kube_do_kubelet_node_status_report_frequency: 5m",
	} {
		if !strings.Contains(string(defaults), want) {
			t.Fatalf("kubelet polling defaults are missing %q", want)
		}
	}

	templatePath := filepath.Join("..", "..", "ansible", "roles", "kubeadm_init", "templates", "kubeadm-config.yaml.j2")
	template, err := os.ReadFile(templatePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"syncFrequency: {{ kube_do_kubelet_sync_frequency }}",
		"fileCheckFrequency: {{ kube_do_kubelet_file_check_frequency }}",
		"nodeStatusUpdateFrequency: {{ kube_do_kubelet_node_status_update_frequency }}",
		"nodeStatusReportFrequency: {{ kube_do_kubelet_node_status_report_frequency }}",
	} {
		if !strings.Contains(string(template), want) {
			t.Fatalf("kubeadm config is missing kubelet polling field %q", want)
		}
	}
}

func TestKubeadmConfigSupportsSlowStorageOverrides(t *testing.T) {
	path := filepath.Join("..", "..", "ansible", "roles", "kubeadm_init", "templates", "kubeadm-config.yaml.j2")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, want := range []string{
		"storage-initialization-timeout",
		"etcd-healthcheck-timeout",
		"etcd-readycheck-timeout",
		"heartbeat-interval",
		"election-timeout",
		"unsafe-no-fsync",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("kubeadm config is missing slow-storage override %q", want)
		}
	}
}

func TestContainerRuntimeSupportsCgroupDriverOverrides(t *testing.T) {
	containerdDefaults, err := os.ReadFile(filepath.Join("..", "..", "ansible", "roles", "containerd", "defaults", "main.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(containerdDefaults), "kube_do_containerd_systemd_cgroup: true") {
		t.Fatal("containerd must keep systemd cgroups enabled by default")
	}
	containerdTemplate, err := os.ReadFile(filepath.Join("..", "..", "ansible", "roles", "containerd", "templates", "config.toml.j2"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(containerdTemplate), "kube_do_containerd_systemd_cgroup | bool") {
		t.Fatal("containerd cgroup driver must be configurable")
	}

	kubeadmDefaults, err := os.ReadFile(filepath.Join("..", "..", "ansible", "roles", "kubeadm_init", "defaults", "main.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(kubeadmDefaults), "kube_do_kubelet_cgroup_driver: systemd") {
		t.Fatal("kubelet must use the systemd cgroup driver by default")
	}
	kubeadmTemplate, err := os.ReadFile(filepath.Join("..", "..", "ansible", "roles", "kubeadm_init", "templates", "kubeadm-config.yaml.j2"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(kubeadmTemplate), "cgroupDriver: {{ kube_do_kubelet_cgroup_driver }}") {
		t.Fatal("kubelet cgroup driver must be configurable")
	}
}
