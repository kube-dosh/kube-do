package ops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kube-dosh/kube-do/internal/cluster"
	"github.com/kube-dosh/kube-do/internal/manifest"
)

func TestBackupEtcdAndCheckCerts(t *testing.T) {
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

	root := t.TempDir()
	ansibleDir := filepath.Join(root, "ansible")
	if err := os.MkdirAll(ansibleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ops-etcd-backup.yml", "ops-certs-check.yml"} {
		if err := os.WriteFile(filepath.Join(ansibleDir, name), []byte("---\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
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
fi
printf '%s\n' "$@"
`
	if err := os.WriteFile(filepath.Join(binDir, "ansible-playbook"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	var log bytes.Buffer
	backupDir := filepath.Join(root, "backups")
	result, err := BackupEtcd(context.Background(), cfg, m, ansibleDir, filepath.Join(root, "rendered"), backupDir, &log)
	if err != nil {
		t.Fatalf("BackupEtcd: %v\n%s", err, log.String())
	}
	if result.Cluster != "test" || result.Size != int64(len("snapshot-data")) {
		t.Fatalf("result = %#v", result)
	}
	data, err := os.ReadFile(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "snapshot-data" {
		t.Fatalf("snapshot = %q", data)
	}

	log.Reset()
	if err := CheckCerts(context.Background(), cfg, m, ansibleDir, filepath.Join(root, "rendered-certs"), &log); err != nil {
		t.Fatalf("CheckCerts: %v\n%s", err, log.String())
	}
	if !strings.Contains(log.String(), "ops-certs-check.yml") {
		t.Fatalf("certificate check did not invoke its playbook:\n%s", log.String())
	}
}
