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
cert_dir=""
for arg in "$@"; do
  case "$arg" in
    kube_do_backup_dir=*) backup_dir="${arg#*=}" ;;
    kube_do_backup_filename=*) filename="${arg#*=}" ;;
    kube_do_cert_output_dir=*) cert_dir="${arg#*=}" ;;
  esac
done
if [ -n "$backup_dir" ] && [ -n "$filename" ]; then
  printf 'snapshot-data' > "$backup_dir/$filename"
  sum=$(sha256sum "$backup_dir/$filename" | awk '{print $1}')
  size=$(wc -c < "$backup_dir/$filename")
  printf '{"hash":1,"revision":12,"totalKey":3,"totalSize":%s}\n' "$size" > "$backup_dir/$filename.status.json"
  printf '{"cluster":"test","filename":"%s","sha256":"%s","size":%s,"snapshotStatus":{"hash":1,"revision":12,"totalKey":3,"totalSize":%s}}\n' "$filename" "$sum" "$size" "$size" > "$backup_dir/$filename.json"
fi
if [ -n "$cert_dir" ]; then
  cat > "$cert_dir/cp-1.txt" <<'EOF'
CERTIFICATE                EXPIRES                  RESIDUAL TIME   CERTIFICATE AUTHORITY   EXTERNALLY MANAGED
admin.conf                 Nov 02, 2027 02:02 UTC   351d            ca                      no
!MISSING! super-admin.conf

CERTIFICATE AUTHORITY   EXPIRES                  RESIDUAL TIME   EXTERNALLY MANAGED
ca                      Oct 30, 2036 02:02 UTC   9y              no
EOF
fi
printf '%s\n' "$@"
`
	if err := os.WriteFile(filepath.Join(binDir, "ansible-playbook"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var log bytes.Buffer
	backupDir := filepath.Join(root, "backups")
	result, err := BackupEtcd(context.Background(), cfg, m, ansibleDir, filepath.Join(root, "rendered"), backupDir, BackupOptions{Timeout: time.Minute}, &log)
	if err != nil {
		t.Fatalf("BackupEtcd: %v\n%s", err, log.String())
	}
	if result.Cluster != "test" || result.Size != int64(len("snapshot-data")) || !result.Validated || result.Snapshot.Revision != 12 {
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
	certResult, err := CheckCerts(context.Background(), cfg, m, ansibleDir, filepath.Join(root, "rendered-certs"), CertCheckOptions{Timeout: time.Minute}, &log)
	if err != nil {
		t.Fatalf("CheckCerts: %v\n%s", err, log.String())
	}
	if certResult.CertificateCount != 2 || certResult.MissingCount != 1 || certResult.Healthy {
		t.Fatalf("certResult = %#v", certResult)
	}
	if !strings.Contains(log.String(), "ops-certs-check.yml") {
		t.Fatalf("certificate check did not invoke its playbook:\n%s", log.String())
	}
}

func TestParseCertOutputClassifiesExpiry(t *testing.T) {
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	output := `CERTIFICATE                EXPIRES                  RESIDUAL TIME   CERTIFICATE AUTHORITY   EXTERNALLY MANAGED
expired                    Nov 01, 2026 00:00 UTC   0s              ca                      no
critical                   Dec 01, 2026 00:00 UTC   15d             ca                      no
warning                    Jan 15, 2027 00:00 UTC   60d             ca                      no
healthy                    Nov 15, 2027 00:00 UTC   365d            ca                      no
!MISSING! super-admin.conf

CERTIFICATE AUTHORITY   EXPIRES                  RESIDUAL TIME   EXTERNALLY MANAGED
ca                      Oct 30, 2036 00:00 UTC   9y              no
`
	result, err := parseCertOutput(output, now, CertCheckOptions{WarnDays: 90, CriticalDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Certificates) != 5 || len(result.Missing) != 1 {
		t.Fatalf("result = %#v", result)
	}
	severity := map[string]string{}
	for _, cert := range result.Certificates {
		severity[cert.Name] = cert.Severity
	}
	if severity["expired"] != "expired" || severity["critical"] != "critical" || severity["warning"] != "warning" || severity["healthy"] != "ok" || severity["ca"] != "ok" {
		t.Fatalf("severity = %#v", severity)
	}
}

func TestPruneBackupsKeepsCurrentAndNewest(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		"demo-etcd-20261115T000000Z.db",
		"demo-etcd-20261115T010000Z.db",
		"demo-etcd-20261115T020000Z.db",
	}
	base := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	for index, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("snapshot"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".json", []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		modified := base.Add(time.Duration(index) * time.Hour)
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := pruneBackups(dir, "demo", 2, names[2])
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != names[0] {
		t.Fatalf("deleted = %#v", deleted)
	}
	for _, name := range names[1:] {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("expected %s to be kept: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, names[0])); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be deleted", names[0])
	}
}
