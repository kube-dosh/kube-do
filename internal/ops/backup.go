package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kube-dosh/kube-do/internal/cluster"
	"github.com/kube-dosh/kube-do/internal/install"
	"github.com/kube-dosh/kube-do/internal/manifest"
)

const (
	DefaultBackupTimeout      = 30 * time.Minute
	DefaultBackupMinFreeBytes = int64(1 << 30)
	remoteEtcdMinFreeBytes    = int64(256 << 20)
)

// BackupOptions controls etcd snapshot creation and local retention.
type BackupOptions struct {
	Timeout      time.Duration `json:"timeout,omitempty"`
	Retain       int           `json:"retain,omitempty"`
	MinFreeBytes int64         `json:"minFreeBytes,omitempty"`
}

// BackupSnapshotStatus is the output of etcdutl snapshot status.
type BackupSnapshotStatus struct {
	Hash      uint32 `json:"hash"`
	Revision  int64  `json:"revision"`
	TotalKey  int64  `json:"totalKey"`
	TotalSize int64  `json:"totalSize"`
}

// BackupResult describes a downloaded and validated etcd snapshot.
type BackupResult struct {
	Cluster          string               `json:"cluster"`
	Filename         string               `json:"filename"`
	Path             string               `json:"path"`
	MetadataPath     string               `json:"metadataPath"`
	SHA256           string               `json:"sha256"`
	Size             int64                `json:"size"`
	CreatedAt        time.Time            `json:"createdAt"`
	Validated        bool                 `json:"validated"`
	Snapshot         BackupSnapshotStatus `json:"snapshot"`
	RetentionDeleted []string             `json:"retentionDeleted,omitempty"`
}

type backupMetadata struct {
	Cluster        string                `json:"cluster"`
	Filename       string                `json:"filename"`
	CreatedAt      string                `json:"createdAt"`
	SHA256         string                `json:"sha256"`
	Size           int64                 `json:"size"`
	EtcdPod        string                `json:"etcdPod"`
	SnapshotStatus *BackupSnapshotStatus `json:"snapshotStatus"`
}

func (o BackupOptions) withDefaults() BackupOptions {
	if o.Timeout <= 0 {
		o.Timeout = DefaultBackupTimeout
	}
	if o.MinFreeBytes <= 0 {
		o.MinFreeBytes = DefaultBackupMinFreeBytes
	}
	return o
}

// BackupEtcd saves an etcd snapshot from the first control-plane node, validates
// its structure, and fetches it to backupDir on the Installer Host.
func BackupEtcd(ctx context.Context, cfg *cluster.Config, m *manifest.Manifest, ansibleDir, workDir, backupDir string, opts BackupOptions, log io.Writer) (result BackupResult, err error) {
	if cfg == nil {
		return result, errors.New("cluster config is nil")
	}
	if m == nil {
		return result, errors.New("resource manifest is nil")
	}
	opts = opts.withDefaults()
	ansibleDir, workDir, err = prepareDirs(ansibleDir, workDir)
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
	if err := ensureFreeSpace(backupDir, opts.MinFreeBytes); err != nil {
		return result, err
	}
	if err := renderForOps(cfg, m, workDir, log); err != nil {
		return result, err
	}

	now := time.Now().UTC()
	filename, path, err := uniqueBackupFilename(backupDir, cfg.Metadata.Name, now)
	if err != nil {
		return result, err
	}
	metadataPath := path + ".json"
	statusPath := path + ".status.json"
	succeeded := false
	defer func() {
		if succeeded {
			return
		}
		for _, candidate := range []string{path, metadataPath, statusPath} {
			if removeErr := os.Remove(candidate); removeErr == nil {
				logf(log, "removed incomplete backup artifact %s", candidate)
			}
		}
	}()

	logf(log, "==> saving etcd snapshot to %s", path)
	runCtx, cancel := contextWithDefaultTimeout(ctx, opts.Timeout)
	defer cancel()
	if err := install.Run(runCtx, install.Options{
		AnsibleDir: ansibleDir,
		Playbook:   "ops-etcd-backup.yml",
		Inventory:  filepath.Join(workDir, "inventory.ini"),
		WorkingDir: ansibleDir,
		ExtraVars: []string{
			"kube_do_backup_dir=" + backupDir,
			"kube_do_backup_filename=" + filename,
			"kube_do_backup_timestamp=" + now.Format(time.RFC3339),
			fmt.Sprintf("kube_do_backup_min_free_bytes=%d", remoteEtcdMinFreeBytes),
		},
		Stdout: log,
		Stderr: log,
	}); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return result, fmt.Errorf("etcd backup timed out after %s", opts.Timeout)
		}
		return result, err
	}

	info, err := os.Stat(path)
	if err != nil {
		return result, fmt.Errorf("verify etcd snapshot: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return result, fmt.Errorf("secure etcd snapshot: %w", err)
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return result, err
	}
	metadata, err := readBackupMetadata(metadataPath)
	if err != nil {
		return result, err
	}
	if !strings.EqualFold(metadata.SHA256, sum) {
		return result, fmt.Errorf("snapshot checksum mismatch: metadata=%s local=%s", metadata.SHA256, sum)
	}
	if metadata.Size != info.Size() {
		return result, fmt.Errorf("snapshot size mismatch: metadata=%d local=%d", metadata.Size, info.Size())
	}
	if metadata.SnapshotStatus == nil {
		return result, errors.New("snapshot metadata does not contain etcdutl status")
	}
	if metadata.SnapshotStatus.TotalSize <= 0 {
		return result, fmt.Errorf("etcdutl reported invalid snapshot size %d", metadata.SnapshotStatus.TotalSize)
	}

	result = BackupResult{
		Cluster:      cfg.Metadata.Name,
		Filename:     filename,
		Path:         path,
		MetadataPath: metadataPath,
		SHA256:       sum,
		Size:         info.Size(),
		CreatedAt:    now,
		Validated:    true,
		Snapshot:     *metadata.SnapshotStatus,
	}
	logf(log, "==> etcd snapshot validated: revision=%d keys=%d size=%d", result.Snapshot.Revision, result.Snapshot.TotalKey, result.Snapshot.TotalSize)

	if opts.Retain > 0 {
		deleted, pruneErr := pruneBackups(backupDir, cfg.Metadata.Name, opts.Retain, filename)
		result.RetentionDeleted = deleted
		if pruneErr != nil {
			logf(log, "warning: failed to prune old etcd snapshots: %v", pruneErr)
		} else if len(deleted) > 0 {
			logf(log, "==> pruned %d old etcd snapshots", len(deleted))
		}
	}
	logf(log, "==> etcd snapshot ready: %s (%d bytes, sha256=%s)", result.Path, result.Size, result.SHA256)
	succeeded = true
	return result, nil
}

func readBackupMetadata(path string) (backupMetadata, error) {
	var metadata backupMetadata
	data, err := os.ReadFile(path)
	if err != nil {
		return metadata, fmt.Errorf("read etcd backup metadata: %w", err)
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return metadata, fmt.Errorf("decode etcd backup metadata: %w", err)
	}
	return metadata, nil
}

func pruneBackups(backupDir, clusterName string, retain int, current string) ([]string, error) {
	if retain <= 0 {
		return nil, nil
	}
	matches, err := filepath.Glob(filepath.Join(backupDir, clusterName+"-etcd-*.db"))
	if err != nil {
		return nil, err
	}
	type backupFile struct {
		path    string
		modTime time.Time
	}
	files := make([]backupFile, 0, len(matches))
	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		files = append(files, backupFile{path: path, modTime: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].modTime.Equal(files[j].modTime) {
			return files[i].path > files[j].path
		}
		return files[i].modTime.After(files[j].modTime)
	})
	ordered := make([]backupFile, 0, len(files))
	for _, file := range files {
		if filepath.Base(file.path) == current {
			ordered = append(ordered, file)
			break
		}
	}
	for _, file := range files {
		if filepath.Base(file.path) != current {
			ordered = append(ordered, file)
		}
	}
	var deleted []string
	for index, file := range ordered {
		if index < retain {
			continue
		}
		for _, path := range []string{file.path, file.path + ".json", file.path + ".status.json"} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return deleted, err
			}
		}
		deleted = append(deleted, filepath.Base(file.path))
	}
	return deleted, nil
}
