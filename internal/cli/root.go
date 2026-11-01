// Package cli wires the kubedo command tree.
package cli

import "github.com/spf13/cobra"

// NewRootCmd builds a fresh command tree (useful in tests).
func NewRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "kubedo",
		Short:         "Offline Kubernetes installer",
		Long:          "kubedo prepares, versions and distributes every resource needed to install Kubernetes offline, then drives Ansible to install the cluster.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(
		newVersionCmd(),
		newCheckCmd(),
		newRenderCmd(),
		newServeCmd(),
		newRegistryCmd(),
		newDownloadCmd(),
		newInstallCmd(),
		newManifestCmd(),
	)
	return cmd
}

// Execute runs the CLI.
func Execute() error {
	return NewRootCmd().Execute()
}
