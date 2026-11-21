package ops

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/kube-dosh/kube-do/internal/cluster"
	"github.com/kube-dosh/kube-do/internal/manifest"
	"github.com/kube-dosh/kube-do/internal/render"
)

// AddControlPlanes joins new control-plane nodes and re-applies the HA
// configuration to every control-plane node. A pre-change etcd snapshot is
// always created before the first remote mutation.
func AddControlPlanes(ctx context.Context, cfg *cluster.Config, m *manifest.Manifest, ansibleDir, workDir, backupDir string, newNodes []cluster.Node, backupOpts BackupOptions, log io.Writer) (result BackupResult, err error) {
	if cfg == nil || m == nil {
		return result, errors.New("cluster config and manifest are required")
	}
	if len(newNodes) == 0 {
		return result, errors.New("at least one control-plane node is required")
	}
	for _, node := range newNodes {
		if _, ok := findNode(cfg.ControlPlane.Nodes, node.Name); !ok {
			return result, fmt.Errorf("new control-plane node %q is not present in the target cluster config", node.Name)
		}
	}
	ansibleDir, workDir, err = prepareDirs(ansibleDir, workDir)
	if err != nil {
		return result, err
	}
	logf(log, "==> creating a pre-change etcd snapshot")
	result, err = BackupEtcd(ctx, cfg, m, ansibleDir, workDir, backupDir, backupOpts, log)
	if err != nil {
		return result, fmt.Errorf("pre-change etcd backup: %w", err)
	}
	files, err := render.RenderAddControlPlanes(cfg, newNodes, m, workDir)
	if err != nil {
		return result, err
	}
	for _, file := range files {
		logf(log, "rendered %s", file)
	}
	logf(log, "==> joining %d control-plane node(s)", len(newNodes))
	if err := runLifecyclePlaybook(ctx, ansibleDir, workDir, "add-control-plane.yml", DefaultLifecycleTimeout, nil, log); err != nil {
		return result, err
	}
	return result, nil
}

// RemoveNode drains and resets one worker or control-plane node. Removing a
// control-plane node also removes its etcd member and rolls the HA
// configuration on the surviving control-plane nodes.
func RemoveNode(ctx context.Context, cfg *cluster.Config, m *manifest.Manifest, ansibleDir, workDir, backupDir, nodeName, role string, backupOpts BackupOptions, log io.Writer) (result RemoveNodeResult, err error) {
	if cfg == nil || m == nil {
		return result, errors.New("cluster config and manifest are required")
	}
	role, err = normalizeRole(role)
	if err != nil {
		return result, err
	}
	result = RemoveNodeResult{Cluster: cfg.Metadata.Name, Node: nodeName, Role: role}
	var node cluster.Node
	var ok bool
	switch role {
	case RoleControlPlane:
		node, ok = findNode(cfg.ControlPlane.Nodes, nodeName)
		if !ok {
			return result, fmt.Errorf("control-plane node %q not found", nodeName)
		}
		if len(cfg.ControlPlane.Nodes) <= 1 {
			return result, errors.New("refusing to remove the last control-plane node")
		}
	case RoleWorker:
		node, ok = findNode(cfg.Workers, nodeName)
		if !ok {
			return result, fmt.Errorf("worker node %q not found", nodeName)
		}
	}
	result.Address = node.Address
	ansibleDir, workDir, err = prepareDirs(ansibleDir, workDir)
	if err != nil {
		return result, err
	}
	logf(log, "==> creating a pre-removal etcd snapshot")
	result.Backup, err = BackupEtcd(ctx, cfg, m, ansibleDir, workDir, backupDir, backupOpts, log)
	if err != nil {
		return result, fmt.Errorf("pre-removal etcd backup: %w", err)
	}
	files, err := render.RenderRemoveNode(cfg, node, role, m, workDir)
	if err != nil {
		return result, err
	}
	for _, file := range files {
		logf(log, "rendered %s", file)
	}
	logf(log, "==> removing %s node %s", role, nodeName)
	extraVars := []string{
		"kube_do_remove_node_name=" + nodeName,
		"kube_do_remove_node_address=" + node.Address,
		"kube_do_remove_node_role=" + role,
	}
	if err := runLifecyclePlaybook(ctx, ansibleDir, workDir, "remove-node.yml", DefaultLifecycleTimeout, extraVars, log); err != nil {
		return result, err
	}
	result.RemovedAt = newOperationTimestamp()
	return result, nil
}
