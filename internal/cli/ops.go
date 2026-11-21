package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/kube-dosh/kube-do/internal/daemon"
	"github.com/kube-dosh/kube-do/internal/ops"
)

func newEtcdCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "etcd",
		Short: "Operate the cluster etcd data store",
	}
	cmd.AddCommand(newEtcdBackupCmd(), newEtcdRestoreCmd())
	return cmd
}

func newEtcdRestoreCmd() *cobra.Command {
	var (
		configPath   string
		manifestPath string
		ansibleDir   string
		cacheDir     string
		outDir       string
		backupDir    string
		snapshotPath string
		targetNode   string
		confirm      bool
		jsonOutput   bool
	)
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Restore a cluster from an etcd snapshot",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !confirm {
				return fmt.Errorf("etcd restore requires --yes")
			}
			cfg, _, err := loadConfigAndManifest(configPath, manifestPath)
			if err != nil {
				return err
			}
			if ansibleDir == "" {
				ansibleDir = defaultAnsibleDir()
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			logOutput := cmd.OutOrStdout()
			if jsonOutput {
				logOutput = cmd.ErrOrStderr()
			}
			executor := newHostExecutor(cacheDir, ansibleDir, manifestPath)
			result, err := executor.RestoreEtcd(ctx, cfg, daemon.EtcdRestoreRequest{
				SnapshotPath: snapshotPath,
				TargetNode:   targetNode,
				Confirm:      confirm,
			}, outDir, backupDir, logOutput)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd, result)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "restored=%s\ncluster=%s\nsnapshotSha256=%s\n", result.SnapshotPath, result.Cluster, result.SnapshotSHA256)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "cluster.yaml", "path to cluster.yaml")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "path to a Resource Manifest (overrides the embedded one)")
	cmd.Flags().StringVar(&ansibleDir, "ansible-dir", "", "directory containing site.yml and roles")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "Artifact cache directory")
	cmd.Flags().StringVar(&outDir, "out", "rendered", "directory for generated Ansible inventory and group_vars")
	cmd.Flags().StringVar(&backupDir, "backup-dir", "etcd-backups", "local directory for the safety snapshot")
	cmd.Flags().StringVar(&snapshotPath, "snapshot", "", "snapshot file to restore")
	cmd.Flags().StringVar(&targetNode, "target-node", "", "optional control-plane node to name in the restore result")
	cmd.Flags().BoolVar(&confirm, "yes", false, "confirm etcd restore")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print the restore result as JSON")
	_ = cmd.MarkFlagRequired("snapshot")
	return cmd
}

func newEtcdBackupCmd() *cobra.Command {
	var (
		configPath   string
		manifestPath string
		ansibleDir   string
		outDir       string
		backupDir    string
		timeout      time.Duration
		retain       int
		minFreeBytes int64
		jsonOutput   bool
	)
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Save an etcd snapshot and fetch it to the Installer Host",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, m, err := loadConfigAndManifest(configPath, manifestPath)
			if err != nil {
				return err
			}
			if ansibleDir == "" {
				ansibleDir = defaultAnsibleDir()
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			logOutput := cmd.OutOrStdout()
			if jsonOutput {
				logOutput = cmd.ErrOrStderr()
			}
			result, err := ops.BackupEtcd(ctx, cfg, m, ansibleDir, outDir, backupDir, ops.BackupOptions{
				Timeout:      timeout,
				Retain:       retain,
				MinFreeBytes: minFreeBytes,
			}, logOutput)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd, result)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "snapshot=%s\nsize=%d\nsha256=%s\nrevision=%d\nkeys=%d\nvalidated=%t\n", result.Path, result.Size, result.SHA256, result.Snapshot.Revision, result.Snapshot.TotalKey, result.Validated)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "cluster.yaml", "path to cluster.yaml")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "path to a Resource Manifest (overrides the embedded one)")
	cmd.Flags().StringVar(&ansibleDir, "ansible-dir", "", "directory containing site.yml and roles")
	cmd.Flags().StringVar(&outDir, "out", "rendered", "directory for generated Ansible inventory and group_vars")
	cmd.Flags().StringVar(&backupDir, "backup-dir", "etcd-backups", "local directory for downloaded snapshots")
	cmd.Flags().DurationVar(&timeout, "timeout", ops.DefaultBackupTimeout, "etcd backup timeout")
	cmd.Flags().IntVar(&retain, "retain", 0, "keep only the newest N snapshots (0 keeps all)")
	cmd.Flags().Int64Var(&minFreeBytes, "min-free-bytes", ops.DefaultBackupMinFreeBytes, "minimum free space required for downloaded snapshots")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print the backup result as JSON")
	return cmd
}

func newCertsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "certs",
		Short: "Inspect Kubernetes certificates",
	}
	cmd.AddCommand(newCertsCheckCmd())
	return cmd
}

func newCertsCheckCmd() *cobra.Command {
	var (
		configPath   string
		manifestPath string
		ansibleDir   string
		outDir       string
		timeout      time.Duration
		warnDays     int
		criticalDays int
		jsonOutput   bool
	)
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Check Kubernetes certificate expiration on control-plane nodes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, m, err := loadConfigAndManifest(configPath, manifestPath)
			if err != nil {
				return err
			}
			if ansibleDir == "" {
				ansibleDir = defaultAnsibleDir()
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			logOutput := cmd.OutOrStdout()
			if jsonOutput {
				logOutput = cmd.ErrOrStderr()
			}
			result, err := ops.CheckCerts(ctx, cfg, m, ansibleDir, outDir, ops.CertCheckOptions{
				Timeout:      timeout,
				WarnDays:     warnDays,
				CriticalDays: criticalDays,
			}, logOutput)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd, result)
			}
			fmt.Fprint(cmd.OutOrStdout(), ops.FormatCertSummary(result))
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "cluster.yaml", "path to cluster.yaml")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "path to a Resource Manifest (overrides the embedded one)")
	cmd.Flags().StringVar(&ansibleDir, "ansible-dir", "", "directory containing site.yml and roles")
	cmd.Flags().StringVar(&outDir, "out", "rendered", "directory for generated Ansible inventory and group_vars")
	cmd.Flags().DurationVar(&timeout, "timeout", ops.DefaultCertCheckTimeout, "certificate check timeout")
	cmd.Flags().IntVar(&warnDays, "warn-days", ops.DefaultCertWarnDays, "warn when a certificate expires within this many days")
	cmd.Flags().IntVar(&criticalDays, "critical-days", ops.DefaultCertCriticalDays, "mark a certificate critical when it expires within this many days")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print the certificate result as JSON")
	return cmd
}

func writeJSON(cmd *cobra.Command, value any) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
