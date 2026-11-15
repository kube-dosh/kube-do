// Package ops implements safe operational actions against an installed cluster.
package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/kube-dosh/kube-do/internal/cluster"
	"github.com/kube-dosh/kube-do/internal/install"
	"github.com/kube-dosh/kube-do/internal/manifest"
	"github.com/kube-dosh/kube-do/internal/render"
)

type BackupResult struct {
	Cluster   string    `json:"cluster"`
	Path      string    `json:"path"`
	SHA256    string    `json:"sha256"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"createdAt"`
}

// BackupEtcd saves an etcd snapshot from the first control-plane node and
// fetches it to backupDir on the Installer Host.
func BackupEtcd(ctx context.Context, cfg *cluster.Config, m *manifest.Manifest, ansibleDir, workDir, backupDir string, log io.Writer) (BackupResult, error) {
	var result BackupResult
	if cfg == nil {
		return result, errors.New("cluster config is nil")
	}
	if m == nil {
		return result, errors.New("resource manifest is nil")
	}
	ansibleDir, workDir, err := prepareDirs(ansibleDir, workDir)
	if err != nil {
		return result, err
	}
	backupDir, err = filepath.Abs(backupDir)
	if err != nil {
		return result, fmt.Errorf("resolve backup directory: %w", err)
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return result, fmt.Errorf("create backup directory: %w", err)
	}
	if err := renderForOps(cfg, m, workDir, log); err != nil {
		return result, err
	}

	now := time.Now().UTC()
	filename := fmt.Sprintf("%s-etcd-%s.db", cfg.Metadata.Name, now.Format("20060102T150405Z"))
	path := filepath.Join(backupDir, filename)
	logf(log, "==> saving etcd snapshot to %s", path)
	if err := install.Run(ctx, install.Options{
		AnsibleDir: ansibleDir,
		Playbook:   "ops-etcd-backup.yml",
		Inventory:  filepath.Join(workDir, "inventory.ini"),
		WorkingDir: ansibleDir,
		ExtraVars: []string{
			"kube_do_backup_dir=" + backupDir,
			"kube_do_backup_filename=" + filename,
			"kube_do_backup_timestamp=" + now.Format(time.RFC3339),
		},
		Stdout: log,
		Stderr: log,
	}); err != nil {
		return result, err
	}

	info, err := os.Stat(path)
	if err != nil {
		return result, fmt.Errorf("verify etcd snapshot: %w", err)
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return result, err
	}
	result = BackupResult{
		Cluster:   cfg.Metadata.Name,
		Path:      path,
		SHA256:    sum,
		Size:      info.Size(),
		CreatedAt: now,
	}
	logf(log, "==> etcd snapshot ready: %s (%d bytes, sha256=%s)", result.Path, result.Size, result.SHA256)
	return result, nil
}

// CheckCerts runs kubeadm certificate expiration checks on every control-plane
// node and returns the Ansible output through log.
func CheckCerts(ctx context.Context, cfg *cluster.Config, m *manifest.Manifest, ansibleDir, workDir string, log io.Writer) error {
	if cfg == nil {
		return errors.New("cluster config is nil")
	}
	if m == nil {
		return errors.New("resource manifest is nil")
	}
	ansibleDir, workDir, err := prepareDirs(ansibleDir, workDir)
	if err != nil {
		return err
	}
	if err := renderForOps(cfg, m, workDir, log); err != nil {
		return err
	}
	logf(log, "==> checking Kubernetes certificate expiration")
	return install.Run(ctx, install.Options{
		AnsibleDir: ansibleDir,
		Playbook:   "ops-certs-check.yml",
		Inventory:  filepath.Join(workDir, "inventory.ini"),
		WorkingDir: ansibleDir,
		Stdout:     log,
		Stderr:     log,
	})
}

func renderForOps(cfg *cluster.Config, m *manifest.Manifest, workDir string, log io.Writer) error {
	files, err := render.Render(cfg, m, workDir)
	if err != nil {
		return err
	}
	for _, file := range files {
		logf(log, "rendered %s", file)
	}
	return nil
}

func prepareDirs(ansibleDir, workDir string) (string, string, error) {
	ansibleDir, err := filepath.Abs(ansibleDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve ansible directory: %w", err)
	}
	workDir, err = filepath.Abs(workDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve working directory: %w", err)
	}
	return ansibleDir, workDir, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open snapshot: %w", err)
	}
	defer file.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return "", fmt.Errorf("hash snapshot: %w", err)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func logf(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, format+"\n", args...)
}
