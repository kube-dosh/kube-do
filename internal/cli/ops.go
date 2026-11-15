package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/kube-dosh/kube-do/internal/ops"
)

func newEtcdCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "etcd",
		Short: "Operate the cluster etcd data store",
	}
	cmd.AddCommand(newEtcdBackupCmd())
	return cmd
}

func newEtcdBackupCmd() *cobra.Command {
	var (
		configPath   string
		manifestPath string
		ansibleDir   string
		outDir       string
		backupDir    string
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
			result, err := ops.BackupEtcd(ctx, cfg, m, ansibleDir, outDir, backupDir, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "snapshot=%s size=%d sha256=%s\n", result.Path, result.Size, result.SHA256)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "cluster.yaml", "path to cluster.yaml")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "path to a Resource Manifest (overrides the embedded one)")
	cmd.Flags().StringVar(&ansibleDir, "ansible-dir", "", "directory containing site.yml and roles")
	cmd.Flags().StringVar(&outDir, "out", "rendered", "directory for generated Ansible inventory and group_vars")
	cmd.Flags().StringVar(&backupDir, "backup-dir", "etcd-backups", "local directory for downloaded snapshots")
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
			return ops.CheckCerts(ctx, cfg, m, ansibleDir, outDir, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "cluster.yaml", "path to cluster.yaml")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "path to a Resource Manifest (overrides the embedded one)")
	cmd.Flags().StringVar(&ansibleDir, "ansible-dir", "", "directory containing site.yml and roles")
	cmd.Flags().StringVar(&outDir, "out", "rendered", "directory for generated Ansible inventory and group_vars")
	return cmd
}
