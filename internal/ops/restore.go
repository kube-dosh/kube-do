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

// RestoreEtcd restores one snapshot to every control-plane etcd member. The
// current cluster is snapshotted first. The playbook stops kubelet on all
// control-plane nodes, stages the restored data directories, swaps them into
// place, and then starts the static control-plane pods.
func RestoreEtcd(ctx context.Context, cfg *cluster.Config, m *manifest.Manifest, ansibleDir, workDir, backupDir string, opts RestoreOptions, log io.Writer) (result RestoreResult, err error) {
	if cfg == nil || m == nil {
		return result, errors.New("cluster config and manifest are required")
	}
	if err := requireConfirm(opts.Confirm, "etcd restore"); err != nil {
		return result, err
	}
	size, sum, err := validateSnapshot(opts.SnapshotPath, "", 0)
	if err != nil {
		return result, err
	}
	absolute, err := filepath.Abs(opts.SnapshotPath)
	if err != nil {
		return result, fmt.Errorf("resolve snapshot path: %w", err)
	}
	target := cfg.ControlPlane.Nodes[0].Name
	if opts.TargetNode != "" {
		if _, ok := findNode(cfg.ControlPlane.Nodes, opts.TargetNode); !ok {
			return result, fmt.Errorf("restore target %q is not a control-plane node", opts.TargetNode)
		}
		target = opts.TargetNode
	}
	ansibleDir, workDir, err = prepareDirs(ansibleDir, workDir)
	if err != nil {
		return result, err
	}
	logf(log, "==> creating a safety etcd snapshot before restore")
	safety, err := BackupEtcd(ctx, cfg, m, ansibleDir, workDir, backupDir, opts.Backup, log)
	if err != nil {
		return result, fmt.Errorf("safety etcd backup: %w", err)
	}
	files, err := render.RenderRestore(cfg, target, m, workDir)
	if err != nil {
		return result, err
	}
	for _, file := range files {
		logf(log, "rendered %s", file)
	}
	extraVars := []string{
		"kube_do_restore_snapshot=" + absolute,
		"kube_do_restore_snapshot_sha256=" + sum,
		"kube_do_restore_snapshot_size=" + fmt.Sprintf("%d", size),
		"kube_do_restore_target_node=" + target,
		"kube_do_restore_initial_cluster=" + etcdInitialCluster(cfg),
		"kube_do_restore_timestamp=" + newOperationTimestamp().Format("20060102T150405Z"),
	}
	extraVars = append(extraVars, opts.ExtraVars...)
	logf(log, "==> restoring etcd snapshot %s", absolute)
	if err := runLifecyclePlaybook(ctx, ansibleDir, workDir, "ops-etcd-restore.yml", opts.Timeout, extraVars, log); err != nil {
		return result, err
	}
	return RestoreResult{
		Cluster:        cfg.Metadata.Name,
		SnapshotPath:   absolute,
		SnapshotSHA256: sum,
		SnapshotSize:   size,
		TargetNode:     target,
		RestoredAt:     newOperationTimestamp(),
		SafetyBackup:   safety,
	}, nil
}
