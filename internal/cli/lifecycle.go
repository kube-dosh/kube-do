package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/kube-dosh/kube-do/internal/atomicfile"
	"github.com/kube-dosh/kube-do/internal/cluster"
	"github.com/kube-dosh/kube-do/internal/daemon"
	"github.com/kube-dosh/kube-do/internal/ops"
)

func newHostExecutor(cacheDir, ansibleDir, manifestPath string) *daemon.HostExecutor {
	if cacheDir == "" {
		cacheDir = defaultCacheDir()
	}
	return &daemon.HostExecutor{CacheDir: cacheDir, AnsibleDir: ansibleDir, ManifestPath: manifestPath}
}

func operationContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func writeClusterConfig(path string, cfg *cluster.Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode cluster config: %w", err)
	}
	return atomicfile.Write(path, data, 0o600)
}

func newNodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Add or remove cluster nodes",
	}
	cmd.AddCommand(newNodeAddCmd(), newNodeRemoveCmd())
	return cmd
}

func newNodeAddCmd() *cobra.Command {
	var (
		configPath   string
		manifestPath string
		ansibleDir   string
		cacheDir     string
		backupDir    string
		outDir       string
		role         string
		specs        []string
		skipConfig   bool
	)
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Add worker or control-plane nodes to an existing cluster",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, err := loadConfigAndManifest(configPath, manifestPath)
			if err != nil {
				return err
			}
			nodes, err := parseNodeSpecs(specs)
			if err != nil {
				return err
			}
			if len(nodes) == 0 {
				return errors.New("at least one --node name=address is required")
			}
			updated := *cfg
			executor := newHostExecutor(cacheDir, ansibleDir, manifestPath)
			ctx, stop := operationContext()
			defer stop()
			switch strings.ToLower(role) {
			case "worker":
				updated.Workers = append(append([]cluster.Node(nil), cfg.Workers...), nodes...)
				if err := updated.Validate(); err != nil {
					return err
				}
				if err := executor.AddWorkers(ctx, &updated, nodes, outDir, cmd.OutOrStdout()); err != nil {
					return err
				}
			case "control-plane":
				updated.ControlPlane.Nodes = append(append([]cluster.Node(nil), cfg.ControlPlane.Nodes...), nodes...)
				if err := updated.Validate(); err != nil {
					return err
				}
				result, err := executor.AddControlPlanes(ctx, &updated, nodes, outDir, backupDir, cmd.OutOrStdout())
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "backup=%s\n", result.Path)
			default:
				return errors.New("--role must be worker or control-plane")
			}
			if !skipConfig {
				if err := writeClusterConfig(configPath, &updated); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "updated %s\n", configPath)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "cluster.yaml", "path to cluster.yaml")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "path to a Resource Manifest (overrides the embedded one)")
	cmd.Flags().StringVar(&ansibleDir, "ansible-dir", defaultAnsibleDir(), "directory containing site.yml and roles")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "Artifact cache directory")
	cmd.Flags().StringVar(&backupDir, "backup-dir", "etcd-backups", "local directory for pre-change snapshots")
	cmd.Flags().StringVar(&outDir, "out", "rendered", "directory for generated Ansible inventory and group_vars")
	cmd.Flags().StringVar(&role, "role", "worker", "node role: worker or control-plane")
	cmd.Flags().StringArrayVar(&specs, "node", nil, "node in name=address form (repeatable)")
	cmd.Flags().BoolVar(&skipConfig, "skip-config", false, "do not write successful changes back to cluster.yaml")
	return cmd
}

func newNodeRemoveCmd() *cobra.Command {
	var (
		configPath   string
		manifestPath string
		ansibleDir   string
		cacheDir     string
		backupDir    string
		outDir       string
		name         string
		role         string
		confirm      bool
		skipConfig   bool
	)
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "Drain, reset and remove a worker or control-plane node",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !confirm {
				return errors.New("node removal requires --yes")
			}
			cfg, _, err := loadConfigAndManifest(configPath, manifestPath)
			if err != nil {
				return err
			}
			executor := newHostExecutor(cacheDir, ansibleDir, manifestPath)
			ctx, stop := operationContext()
			defer stop()
			result, err := executor.RemoveNode(ctx, cfg, name, role, outDir, backupDir, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			updated := *cfg
			if result.Role == ops.RoleControlPlane {
				updated.ControlPlane.Nodes = filterNodes(cfg.ControlPlane.Nodes, name)
			} else {
				updated.Workers = filterNodes(cfg.Workers, name)
			}
			if !skipConfig {
				if err := writeClusterConfig(configPath, &updated); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "updated %s\n", configPath)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "cluster.yaml", "path to cluster.yaml")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "path to a Resource Manifest (overrides the embedded one)")
	cmd.Flags().StringVar(&ansibleDir, "ansible-dir", defaultAnsibleDir(), "directory containing site.yml and roles")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "Artifact cache directory")
	cmd.Flags().StringVar(&backupDir, "backup-dir", "etcd-backups", "local directory for pre-change snapshots")
	cmd.Flags().StringVar(&outDir, "out", "rendered", "directory for generated Ansible inventory and group_vars")
	cmd.Flags().StringVar(&name, "name", "", "node name to remove")
	cmd.Flags().StringVar(&role, "role", "worker", "node role: worker or control-plane")
	cmd.Flags().BoolVar(&confirm, "yes", false, "confirm node removal")
	cmd.Flags().BoolVar(&skipConfig, "skip-config", false, "do not write successful changes back to cluster.yaml")
	return cmd
}

func parseNodeSpecs(specs []string) ([]cluster.Node, error) {
	nodes := make([]cluster.Node, 0, len(specs))
	for _, spec := range specs {
		name, address, ok := strings.Cut(spec, "=")
		if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(address) == "" {
			return nil, fmt.Errorf("invalid --node %q: expected name=address", spec)
		}
		nodes = append(nodes, cluster.Node{Name: strings.TrimSpace(name), Address: strings.TrimSpace(address)})
	}
	return nodes, nil
}

func filterNodes(nodes []cluster.Node, name string) []cluster.Node {
	out := make([]cluster.Node, 0, len(nodes))
	for _, node := range nodes {
		if node.Name != name {
			out = append(out, node)
		}
	}
	return out
}

func newUpgradeCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "upgrade", Short: "Upgrade or roll back an installed cluster"}
	cmd.AddCommand(newUpgradePlanCmd(), newUpgradeApplyCmd(), newUpgradeRollbackCmd())
	return cmd
}

func newUpgradePlanCmd() *cobra.Command {
	var configPath, manifestPath, targetBuild string
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Validate an in-place Kubernetes upgrade",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, current, err := loadConfigAndManifest(configPath, manifestPath)
			if err != nil {
				return err
			}
			target, err := loadManifest(targetBuild, manifestPath)
			if err != nil {
				return err
			}
			if target.Metadata.Name != targetBuild {
				return fmt.Errorf("manifest is for build %q, not %q", target.Metadata.Name, targetBuild)
			}
			if err := ops.ValidateUpgradeTarget(current, target); err != nil {
				return err
			}
			if !target.SupportsPlatform(cfg.Platform.ManifestOS(), cfg.Platform.Arch) {
				return fmt.Errorf("target Build %s does not support %s/%s", target.Metadata.Name, cfg.Platform.ManifestOS(), cfg.Platform.Arch)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "cluster=%s\nfromBuild=%s\nfromVersion=%s\ntoBuild=%s\ntoVersion=%s\n", cfg.Metadata.Name, current.Metadata.Name, current.Metadata.KubernetesVersion, target.Metadata.Name, target.Metadata.KubernetesVersion)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "cluster.yaml", "path to cluster.yaml")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "path to a Resource Manifest (overrides the embedded one)")
	cmd.Flags().StringVar(&targetBuild, "to-build", "", "target Build name")
	_ = cmd.MarkFlagRequired("to-build")
	return cmd
}

func newUpgradeApplyCmd() *cobra.Command {
	var (
		configPath   string
		manifestPath string
		ansibleDir   string
		cacheDir     string
		backupDir    string
		outDir       string
		targetBuild  string
		prepare      bool
		check        bool
		skipConfig   bool
		jsonOutput   bool
	)
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Perform an in-place Kubernetes upgrade",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, err := loadConfigAndManifest(configPath, manifestPath)
			if err != nil {
				return err
			}
			executor := newHostExecutor(cacheDir, ansibleDir, manifestPath)
			ctx, stop := operationContext()
			defer stop()
			logOutput := cmd.OutOrStdout()
			if jsonOutput {
				logOutput = cmd.ErrOrStderr()
			}
			result, err := executor.Upgrade(ctx, cfg, daemon.UpgradeRequest{TargetBuild: targetBuild, Prepare: &prepare, Check: &check}, outDir, backupDir, logOutput)
			if err != nil {
				return err
			}
			if !skipConfig {
				updated := *cfg
				updated.Build = targetBuild
				if err := writeClusterConfig(configPath, &updated); err != nil {
					return err
				}
			}
			if jsonOutput {
				return writeJSON(cmd, result)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "upgraded %s from %s to %s\n", result.Cluster, result.FromVersion, result.ToVersion)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "cluster.yaml", "path to cluster.yaml")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "path to a Resource Manifest (overrides the embedded one)")
	cmd.Flags().StringVar(&ansibleDir, "ansible-dir", defaultAnsibleDir(), "directory containing site.yml and roles")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "Artifact cache directory")
	cmd.Flags().StringVar(&backupDir, "backup-dir", "etcd-backups", "local directory for pre-upgrade snapshots")
	cmd.Flags().StringVar(&outDir, "out", "rendered", "directory for generated Ansible inventory and group_vars")
	cmd.Flags().StringVar(&targetBuild, "to-build", "", "target Build name")
	cmd.Flags().BoolVar(&prepare, "prepare", true, "download and mirror target Build artifacts")
	cmd.Flags().BoolVar(&check, "check", true, "run target Build preflight checks")
	cmd.Flags().BoolVar(&skipConfig, "skip-config", false, "do not write the new Build to cluster.yaml")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print the upgrade result as JSON")
	_ = cmd.MarkFlagRequired("to-build")
	return cmd
}

func newUpgradeRollbackCmd() *cobra.Command {
	var (
		configPath   string
		manifestPath string
		ansibleDir   string
		cacheDir     string
		backupDir    string
		outDir       string
		targetBuild  string
		snapshotPath string
		snapshotSHA  string
		snapshotSize int64
		prepare      bool
		confirm      bool
		skipConfig   bool
		jsonOutput   bool
	)
	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Restore a previous Build and its pre-upgrade etcd snapshot",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !confirm {
				return errors.New("upgrade rollback requires --yes")
			}
			cfg, _, err := loadConfigAndManifest(configPath, manifestPath)
			if err != nil {
				return err
			}
			executor := newHostExecutor(cacheDir, ansibleDir, manifestPath)
			ctx, stop := operationContext()
			defer stop()
			logOutput := cmd.OutOrStdout()
			if jsonOutput {
				logOutput = cmd.ErrOrStderr()
			}
			result, err := executor.Rollback(ctx, cfg, daemon.RollbackRequest{
				TargetBuild: targetBuild, SnapshotPath: snapshotPath, SnapshotSHA256: snapshotSHA,
				SnapshotSize: snapshotSize, Prepare: &prepare, Confirm: true,
			}, outDir, backupDir, logOutput)
			if err != nil {
				return err
			}
			if !skipConfig {
				updated := *cfg
				updated.Build = targetBuild
				if err := writeClusterConfig(configPath, &updated); err != nil {
					return err
				}
			}
			if jsonOutput {
				return writeJSON(cmd, result)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "rolled back %s to build %s\n", result.Cluster, result.ToBuild)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "cluster.yaml", "path to cluster.yaml")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "path to a Resource Manifest (overrides the embedded one)")
	cmd.Flags().StringVar(&ansibleDir, "ansible-dir", defaultAnsibleDir(), "directory containing site.yml and roles")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "Artifact cache directory")
	cmd.Flags().StringVar(&backupDir, "backup-dir", "etcd-backups", "local directory for safety snapshots")
	cmd.Flags().StringVar(&outDir, "out", "rendered", "directory for generated Ansible inventory and group_vars")
	cmd.Flags().StringVar(&targetBuild, "to-build", "", "Build to restore")
	cmd.Flags().StringVar(&snapshotPath, "snapshot", "", "pre-upgrade etcd snapshot")
	cmd.Flags().StringVar(&snapshotSHA, "snapshot-sha256", "", "expected snapshot SHA-256")
	cmd.Flags().Int64Var(&snapshotSize, "snapshot-size", 0, "expected snapshot size in bytes")
	cmd.Flags().BoolVar(&prepare, "prepare", true, "download and mirror rollback Build artifacts")
	cmd.Flags().BoolVar(&confirm, "yes", false, "confirm rollback")
	cmd.Flags().BoolVar(&skipConfig, "skip-config", false, "do not write the restored Build to cluster.yaml")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print the rollback result as JSON")
	_ = cmd.MarkFlagRequired("to-build")
	_ = cmd.MarkFlagRequired("snapshot")
	return cmd
}

func newClusterCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "cluster", Short: "Manage the cluster lifecycle"}
	cmd.AddCommand(newClusterUninstallCmd())
	return cmd
}

func newClusterUninstallCmd() *cobra.Command {
	var (
		configPath   string
		manifestPath string
		ansibleDir   string
		cacheDir     string
		outDir       string
		confirm      bool
		deleteConfig bool
	)
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Reset Kubernetes state on every node",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !confirm {
				return errors.New("cluster uninstall requires --yes")
			}
			cfg, _, err := loadConfigAndManifest(configPath, manifestPath)
			if err != nil {
				return err
			}
			executor := newHostExecutor(cacheDir, ansibleDir, manifestPath)
			ctx, stop := operationContext()
			defer stop()
			result, err := executor.Uninstall(ctx, cfg, daemon.UninstallRequest{Confirm: true}, outDir, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			if deleteConfig {
				absolute, err := filepath.Abs(configPath)
				if err != nil {
					return err
				}
				if err := os.Remove(absolute); err != nil {
					return fmt.Errorf("remove cluster config: %w", err)
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "uninstalled %s\n", result.Cluster)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "cluster.yaml", "path to cluster.yaml")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "path to a Resource Manifest (overrides the embedded one)")
	cmd.Flags().StringVar(&ansibleDir, "ansible-dir", defaultAnsibleDir(), "directory containing site.yml and roles")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "Artifact cache directory")
	cmd.Flags().StringVar(&outDir, "out", "rendered", "directory for generated Ansible inventory")
	cmd.Flags().BoolVar(&confirm, "yes", false, "confirm cluster uninstall")
	cmd.Flags().BoolVar(&deleteConfig, "delete-config", false, "delete cluster.yaml after a successful uninstall")
	return cmd
}
