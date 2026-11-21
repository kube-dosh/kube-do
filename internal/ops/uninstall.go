package ops

import (
	"context"
	"errors"
	"io"

	"github.com/kube-dosh/kube-do/internal/cluster"
	"github.com/kube-dosh/kube-do/internal/manifest"
	"github.com/kube-dosh/kube-do/internal/render"
)

// Uninstall resets Kubernetes state on every node. Registry data, the artifact
// cache and etcd snapshots remain on disk; deleting those is a separate,
// explicit host operation.
func Uninstall(ctx context.Context, cfg *cluster.Config, m *manifest.Manifest, ansibleDir, workDir string, opts UninstallOptions, log io.Writer) (result UninstallResult, err error) {
	if cfg == nil || m == nil {
		return result, errors.New("cluster config and manifest are required")
	}
	if err := requireConfirm(opts.Confirm, "cluster uninstall"); err != nil {
		return result, err
	}
	ansibleDir, workDir, err = prepareDirs(ansibleDir, workDir)
	if err != nil {
		return result, err
	}
	files, err := render.Render(cfg, m, workDir)
	if err != nil {
		return result, err
	}
	for _, file := range files {
		logf(log, "rendered %s", file)
	}
	logf(log, "==> uninstalling cluster %s", cfg.Metadata.Name)
	extraVars := []string{"kube_do_uninstall_confirm=true"}
	extraVars = append(extraVars, opts.ExtraVars...)
	if err := runLifecyclePlaybook(ctx, ansibleDir, workDir, "uninstall.yml", opts.Timeout, extraVars, log); err != nil {
		return result, err
	}
	result = UninstallResult{Cluster: cfg.Metadata.Name, UninstalledAt: newOperationTimestamp()}
	logf(log, "==> cluster state removed; Registry data and etcd snapshots were preserved")
	return result, nil
}
