package ops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/kube-dosh/kube-do/internal/cluster"
	"github.com/kube-dosh/kube-do/internal/manifest"
	"github.com/kube-dosh/kube-do/internal/render"
)

// Upgrade performs an in-place kubeadm upgrade using the target Build. The
// caller must prepare the target binaries and images before invoking this
// function. A pre-upgrade snapshot is mandatory.
func Upgrade(ctx context.Context, cfg *cluster.Config, current, target *manifest.Manifest, ansibleDir, workDir, backupDir string, opts UpgradeOptions, log io.Writer) (result UpgradeResult, err error) {
	if cfg == nil || current == nil || target == nil {
		return result, errors.New("cluster config and source/target manifests are required")
	}
	if err := ValidateUpgradeTarget(current, target); err != nil {
		return result, err
	}
	ansibleDir, workDir, err = prepareDirs(ansibleDir, workDir)
	if err != nil {
		return result, err
	}
	result = UpgradeResult{
		Cluster:     cfg.Metadata.Name,
		FromBuild:   current.Metadata.Name,
		ToBuild:     target.Metadata.Name,
		FromVersion: current.Metadata.KubernetesVersion,
		ToVersion:   target.Metadata.KubernetesVersion,
	}
	logf(log, "==> creating a pre-upgrade etcd snapshot")
	result.PreUpgradeBackup, err = BackupEtcd(ctx, cfg, current, ansibleDir, workDir, backupDir, opts.Backup, log)
	if err != nil {
		return result, fmt.Errorf("pre-upgrade etcd backup: %w", err)
	}
	files, err := render.Render(cfg, target, workDir)
	if err != nil {
		return result, err
	}
	for _, file := range files {
		logf(log, "rendered %s", file)
	}
	extraVars := []string{
		"kube_do_upgrade_from_build=" + current.Metadata.Name,
		"kube_do_upgrade_to_build=" + target.Metadata.Name,
		"kube_do_upgrade_from_kubernetes_version=" + current.Metadata.KubernetesVersion,
		"kube_do_upgrade_snapshot=" + result.PreUpgradeBackup.Path,
	}
	extraVars = append(extraVars, opts.ExtraVars...)
	logf(log, "==> upgrading Kubernetes from %s to %s", current.Metadata.KubernetesVersion, target.Metadata.KubernetesVersion)
	if err := runLifecyclePlaybook(ctx, ansibleDir, workDir, "upgrade.yml", opts.Timeout, extraVars, log); err != nil {
		return result, err
	}
	result.UpgradedAt = newOperationTimestamp()
	return result, nil
}

// Rollback restores the control-plane configuration and etcd snapshot captured
// by a previous upgrade, then installs the target Build's binaries. This is a
// state rollback, not a kubeadm downgrade.
func Rollback(ctx context.Context, cfg *cluster.Config, current, target *manifest.Manifest, ansibleDir, workDir, backupDir, snapshotPath, expectedSHA256 string, snapshotSize int64, opts RollbackOptions, log io.Writer) (result RollbackResult, err error) {
	if cfg == nil || current == nil || target == nil {
		return result, errors.New("cluster config and source/target manifests are required")
	}
	size, sum, err := validateSnapshot(snapshotPath, expectedSHA256, snapshotSize)
	if err != nil {
		return result, err
	}
	absolute, err := filepath.Abs(snapshotPath)
	if err != nil {
		return result, err
	}
	ansibleDir, workDir, err = prepareDirs(ansibleDir, workDir)
	if err != nil {
		return result, err
	}
	result = RollbackResult{
		Cluster:        cfg.Metadata.Name,
		FromBuild:      current.Metadata.Name,
		ToBuild:        target.Metadata.Name,
		FromVersion:    current.Metadata.KubernetesVersion,
		ToVersion:      target.Metadata.KubernetesVersion,
		SnapshotPath:   absolute,
		SnapshotSHA256: sum,
	}
	logf(log, "==> creating a safety snapshot before rollback")
	result.SafetyBackup, err = BackupEtcd(ctx, cfg, current, ansibleDir, workDir, backupDir, BackupOptions{}, log)
	if err != nil {
		return result, fmt.Errorf("safety etcd backup: %w", err)
	}
	files, err := render.Render(cfg, target, workDir)
	if err != nil {
		return result, err
	}
	for _, file := range files {
		logf(log, "rendered %s", file)
	}
	extraVars := []string{
		"kube_do_rollback_from_build=" + current.Metadata.Name,
		"kube_do_rollback_to_build=" + target.Metadata.Name,
		"kube_do_rollback_snapshot=" + absolute,
		"kube_do_rollback_snapshot_sha256=" + sum,
		"kube_do_rollback_snapshot_size=" + fmt.Sprintf("%d", size),
		"kube_do_rollback_initial_cluster=" + etcdInitialCluster(cfg),
		"kube_do_rollback_timestamp=" + newOperationTimestamp().Format("20060102T150405Z"),
	}
	extraVars = append(extraVars, opts.ExtraVars...)
	logf(log, "==> rolling back cluster to build %s", target.Metadata.Name)
	if err := runLifecyclePlaybook(ctx, ansibleDir, workDir, "rollback.yml", opts.Timeout, extraVars, log); err != nil {
		return result, err
	}
	result.RolledBackAt = newOperationTimestamp()
	return result, nil
}
