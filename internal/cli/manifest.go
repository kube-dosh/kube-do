package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/kubedo/kubedo/internal/bump"
	"github.com/kubedo/kubedo/internal/manifest"
	"github.com/kubedo/kubedo/manifests"
)

func newManifestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "manifest",
		Short: "Manage Resource Manifests",
	}
	cmd.AddCommand(newManifestBumpCmd())
	return cmd
}

func newManifestBumpCmd() *cobra.Command {
	var (
		fromPath   string
		fromBuild  string
		newBuild   string
		outPath    string
		dryRun     bool
		force      bool
		kubernetes string
		cilium     string
		containerd string
		runc       string
		cniPlugins string
		criTools   string
		helm       string
		ciliumCLI  string
		registry   string
		haproxy    string
		keepalived string
	)
	run := &cobra.Command{
		Use:   "bump",
		Short: "Build a new Resource Manifest from explicit upstream versions",
		Long:  "bump updates an existing Resource Manifest, recomputes checksums for changed binaries, and resolves dependent kubeadm/Cilium image versions. All changes are written to a new Build for review; the base Build is never modified.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			base, err := loadBaseManifest(fromPath, fromBuild)
			if err != nil {
				return err
			}
			result, err := bump.Run(cmd.Context(), bump.Options{
				Base:  base,
				Build: newBuild,
				Versions: bump.Versions{
					Kubernetes: kubernetes,
					Cilium:     cilium,
					Containerd: containerd,
					Runc:       runc,
					CNIPlugins: cniPlugins,
					CRITools:   criTools,
					Helm:       helm,
					CiliumCLI:  ciliumCLI,
					Registry:   registry,
					HAProxy:    haproxy,
					Keepalived: keepalived,
				},
				Logf: func(format string, args ...any) {
					fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", args...)
				},
			})
			if err != nil {
				return err
			}
			body, err := yaml.Marshal(result.Manifest)
			if err != nil {
				return err
			}
			if dryRun {
				for _, change := range result.Changes {
					fmt.Fprintln(cmd.ErrOrStderr(), change)
				}
				_, err := cmd.OutOrStdout().Write(body)
				return err
			}
			if outPath == "" {
				outPath = filepath.Join("manifests", "builds", result.Build+".yaml")
			}
			if _, err := os.Stat(outPath); err == nil && !force {
				return fmt.Errorf("%s already exists; use --force to overwrite", outPath)
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(outPath, body, 0o644); err != nil {
				return err
			}
			for _, change := range result.Changes {
				fmt.Fprintln(cmd.OutOrStdout(), change)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", outPath)
			return nil
		},
	}
	run.Flags().StringVar(&fromPath, "from", "", "base Resource Manifest file (default: newest embedded Build)")
	run.Flags().StringVar(&fromBuild, "from-build", "", "base embedded Build name (default: newest embedded Build)")
	run.Flags().StringVar(&newBuild, "build", "", "new Build name (default: derived from Kubernetes version)")
	run.Flags().StringVar(&outPath, "out", "", "output manifest path (default: manifests/builds/<build>.yaml)")
	run.Flags().BoolVar(&dryRun, "dry-run", false, "print the new manifest to stdout without writing")
	run.Flags().BoolVar(&force, "force", false, "overwrite an existing output file")
	run.Flags().StringVar(&kubernetes, "kubernetes", "", "Kubernetes version, with or without v prefix")
	run.Flags().StringVar(&cilium, "cilium", "", "Cilium chart and image version")
	run.Flags().StringVar(&containerd, "containerd", "", "containerd version")
	run.Flags().StringVar(&runc, "runc", "", "runc version")
	run.Flags().StringVar(&cniPlugins, "cni-plugins", "", "CNI plugins version")
	run.Flags().StringVar(&criTools, "cri-tools", "", "cri-tools/crictl version")
	run.Flags().StringVar(&helm, "helm", "", "Helm version")
	run.Flags().StringVar(&ciliumCLI, "cilium-cli", "", "cilium-cli version")
	run.Flags().StringVar(&registry, "registry", "", "CNCF Distribution Registry version")
	run.Flags().StringVar(&haproxy, "haproxy", "", "HAProxy source version")
	run.Flags().StringVar(&keepalived, "keepalived", "", "keepalived source version")
	return run
}

func loadBaseManifest(path, build string) (*manifest.Manifest, error) {
	if path != "" && build != "" {
		return nil, errors.New("--from and --from-build are mutually exclusive")
	}
	if path != "" {
		return manifest.LoadFile(path)
	}
	if build == "" {
		names, err := manifests.Names()
		if err != nil {
			return nil, err
		}
		if len(names) == 0 {
			return nil, errors.New("no embedded Builds")
		}
		build = names[len(names)-1]
	}
	return manifests.Get(build)
}
