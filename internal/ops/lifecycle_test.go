package ops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kube-dosh/kube-do/internal/cluster"
	"github.com/kube-dosh/kube-do/internal/manifest"
)

func lifecycleManifest(name, version string) *manifest.Manifest {
	return &manifest.Manifest{
		Metadata: manifest.Metadata{Name: name, KubernetesVersion: version},
		Spec: manifest.Spec{
			Platforms: []manifest.Platform{{OS: "linux", Arch: "amd64"}},
			Binaries: []manifest.Binary{{
				Name: "kubeadm", Version: version, OS: "linux", Arch: "amd64",
				URL: "https://example.com/kubeadm", SHA256: strings.Repeat("a", 64),
			}},
			Installer: manifest.Installer{Binaries: []manifest.Binary{{
				Name: "registry", Version: "3.1.2", OS: "linux", Arch: "amd64",
				URL: "https://example.com/registry", SHA256: strings.Repeat("b", 64),
			}}},
		},
	}
}

func TestValidateUpgradeTarget(t *testing.T) {
	from := lifecycleManifest("v1.36.5-kube-do.1", "v1.36.5")
	if err := ValidateUpgradeTarget(from, lifecycleManifest("v1.36.6-kube-do.1", "v1.36.6")); err != nil {
		t.Fatalf("patch upgrade rejected: %v", err)
	}
	if err := ValidateUpgradeTarget(from, lifecycleManifest("v1.37.0-kube-do.1", "v1.37.0")); err != nil {
		t.Fatalf("minor upgrade rejected: %v", err)
	}
	for _, target := range []*manifest.Manifest{
		lifecycleManifest("v1.36.5-kube-do.1", "v1.36.5"),
		lifecycleManifest("v1.36.4-kube-do.1", "v1.36.4"),
		lifecycleManifest("v1.38.0-kube-do.1", "v1.38.0"),
		lifecycleManifest("v2.0.0-kube-do.1", "v2.0.0"),
	} {
		if err := ValidateUpgradeTarget(from, target); err == nil {
			t.Fatalf("unsupported target %s was accepted", target.Metadata.Name)
		}
	}
}

func TestRestoreAndUninstallRequireConfirmation(t *testing.T) {
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
cni:
  name: cilium
  kubeProxyReplacement: true
`))
	if err != nil {
		t.Fatal(err)
	}
	m := lifecycleManifest("test-build", "v1.36.5")
	if _, err := RestoreEtcd(context.Background(), cfg, m, t.TempDir(), t.TempDir(), t.TempDir(), RestoreOptions{SnapshotPath: "x"}, &bytes.Buffer{}); err == nil {
		t.Fatal("restore without confirmation was accepted")
	}
	if _, err := Uninstall(context.Background(), cfg, m, t.TempDir(), t.TempDir(), UninstallOptions{}, &bytes.Buffer{}); err == nil {
		t.Fatal("uninstall without confirmation was accepted")
	}
}

func TestRemoveLastControlPlaneRejected(t *testing.T) {
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
cni:
  name: cilium
  kubeProxyReplacement: true
`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = RemoveNode(context.Background(), cfg, lifecycleManifest("test-build", "v1.36.5"), t.TempDir(), t.TempDir(), t.TempDir(), "cp-1", "control-plane", BackupOptions{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "last control-plane") {
		t.Fatalf("RemoveNode error = %v", err)
	}
}

func TestRemoveNodeCreatesBackupAndRunsPlaybook(t *testing.T) {
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
		t.Fatal(err)
	}
	root := t.TempDir()
	ansibleDir := filepath.Join(root, "ansible")
	if err := os.MkdirAll(ansibleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ops-etcd-backup.yml", "remove-node.yml"} {
		if err := os.WriteFile(filepath.Join(ansibleDir, name), []byte("---\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	argsLog := filepath.Join(root, "args.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$KUBE_DO_TEST_ARGS"
backup_dir=""
filename=""
for arg in "$@"; do
  case "$arg" in
    kube_do_backup_dir=*) backup_dir="${arg#*=}" ;;
    kube_do_backup_filename=*) filename="${arg#*=}" ;;
  esac
done
if [ -n "$backup_dir" ] && [ -n "$filename" ]; then
  printf 'snapshot-data' > "$backup_dir/$filename"
  sum=$(sha256sum "$backup_dir/$filename" | awk '{print $1}')
  size=$(wc -c < "$backup_dir/$filename")
  printf '{"hash":1,"revision":12,"totalKey":3,"totalSize":%s}\n' "$size" > "$backup_dir/$filename.status.json"
  printf '{"cluster":"test","filename":"%s","sha256":"%s","size":%s,"snapshotStatus":{"hash":1,"revision":12,"totalKey":3,"totalSize":%s}}\n' "$filename" "$sum" "$size" "$size" > "$backup_dir/$filename.json"
fi
`
	if err := os.WriteFile(filepath.Join(binDir, "ansible-playbook"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KUBE_DO_TEST_ARGS", argsLog)

	result, err := RemoveNode(context.Background(), cfg, lifecycleManifest("test-build", "v1.36.5"), ansibleDir, filepath.Join(root, "rendered"), filepath.Join(root, "backups"), "cp-2", "control-plane", BackupOptions{Timeout: time.Minute}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("RemoveNode: %v", err)
	}
	if result.Role != RoleControlPlane || result.Node != "cp-2" || !result.Backup.Validated {
		t.Fatalf("result = %#v", result)
	}
	args, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ops-etcd-backup.yml", "remove-node.yml", "kube_do_remove_node_name=cp-2", "kube_do_remove_node_role=control-plane"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("ansible args missing %q:\n%s", want, args)
		}
	}
}
