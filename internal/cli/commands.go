package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/kubedo/kubedo/internal/check"
	"github.com/kubedo/kubedo/internal/cluster"
	"github.com/kubedo/kubedo/internal/download"
	"github.com/kubedo/kubedo/internal/install"
	"github.com/kubedo/kubedo/internal/layout"
	"github.com/kubedo/kubedo/internal/manifest"
	"github.com/kubedo/kubedo/internal/registry"
	"github.com/kubedo/kubedo/internal/render"
	"github.com/kubedo/kubedo/internal/serve"
	"github.com/kubedo/kubedo/internal/version"
	"github.com/kubedo/kubedo/manifests"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), version.String())
		},
	}
}

func newCheckCmd() *cobra.Command {
	var (
		configPath   string
		manifestPath string
		cacheDir     string
		probeReg     bool
		strict       bool
	)
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Validate cluster.yaml, the Resource Manifest and cached Artifacts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, m, err := loadConfigAndManifest(configPath, manifestPath)
			if err != nil {
				return err
			}
			if cacheDir == "" {
				cacheDir = defaultCacheDir()
			}
			results := check.Run(check.Options{
				Config:        cfg,
				Manifest:      m,
				CacheDir:      cacheDir,
				CheckRegistry: probeReg,
			})
			for _, r := range results {
				fmt.Fprintf(cmd.OutOrStdout(), "[%-4s] %-10s %s\n", r.Status, r.Name, r.Detail)
			}
			if check.HasFailure(results) {
				return errors.New("check failed")
			}
			if strict && check.HasWarning(results) {
				return errors.New("check failed: warnings treated as errors")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "cluster.yaml", "path to cluster.yaml")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "path to a Resource Manifest (overrides the embedded one)")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "Artifact cache directory (default: user cache dir)")
	cmd.Flags().BoolVar(&probeReg, "registry", false, "probe the configured Registry over HTTPS")
	cmd.Flags().BoolVar(&strict, "strict", false, "treat warnings as failures")
	return cmd
}

func newRenderCmd() *cobra.Command {
	var (
		configPath   string
		manifestPath string
		outDir       string
	)
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Generate Ansible inventory and group_vars from cluster.yaml",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, m, err := loadConfigAndManifest(configPath, manifestPath)
			if err != nil {
				return err
			}
			files, err := render.Render(cfg, m, outDir)
			if err != nil {
				return err
			}
			for _, f := range files {
				fmt.Fprintln(cmd.OutOrStdout(), f)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "cluster.yaml", "path to cluster.yaml")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "path to a Resource Manifest (overrides the embedded one)")
	cmd.Flags().StringVar(&outDir, "out", "rendered", "output directory for Ansible files")
	return cmd
}

func newServeCmd() *cobra.Command {
	var (
		dir  string
		addr string
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve cached Artifacts over HTTP for the cluster nodes",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if dir == "" {
				dir = defaultCacheDir()
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return (&serve.Server{Addr: addr, Dir: dir}).Run(ctx)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "directory to serve (default: Artifact cache dir)")
	cmd.Flags().StringVar(&addr, "addr", ":8080", "listen address")
	return cmd
}

func newDownloadCmd() *cobra.Command {
	var (
		configPath      string
		manifestPath    string
		build           string
		cacheDir        string
		binariesOnly    bool
		imagesOnly      bool
		insecure        bool
		caFile          string
		credentialsFile string
	)
	cmd := &cobra.Command{
		Use:   "download",
		Short: "Download binaries, build Installer images, and mirror node images",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if binariesOnly && imagesOnly {
				return errors.New("--binaries-only and --images-only are mutually exclusive")
			}
			cfg, err := cluster.LoadFile(configPath)
			if err != nil {
				return err
			}
			selectedBuild := cfg.Build
			if build != "" {
				selectedBuild = build
			}
			m, err := loadManifest(selectedBuild, manifestPath)
			if err != nil {
				return err
			}
			if m.Metadata.Name != selectedBuild {
				return fmt.Errorf("manifest is for build %q, not %q", m.Metadata.Name, selectedBuild)
			}
			if cacheDir == "" {
				cacheDir = defaultCacheDir()
			}
			if caFile == "" {
				caFile = cfg.RegistryCAPath()
			}
			if credentialsFile == "" {
				credentialsFile = cfg.Registry.CredentialsFile
			}
			if selectedBuild != cfg.Build {
				fmt.Fprintf(cmd.OutOrStdout(), "note: downloading build %q while cluster.yaml pins %q\n", selectedBuild, cfg.Build)
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			sum, err := download.Run(ctx, download.Options{
				CacheDir:        cacheDir,
				RegistryAddress: cfg.Registry.Address,
				Manifest:        m,
				OS:              cfg.Platform.ManifestOS(),
				Arch:            cfg.Platform.Arch,
				BinariesOnly:    binariesOnly,
				ImagesOnly:      imagesOnly,
				Insecure:        insecure,
				CAFile:          caFile,
				CredentialsFile: credentialsFile,
				Logf: func(format string, args ...any) {
					fmt.Fprintf(cmd.OutOrStdout(), format+"\n", args...)
				},
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "platform=%s downloaded=%d skipped=%d built_images=%d mirrored=%d\n", sum.Platform, sum.Downloaded, sum.Skipped, sum.Built, sum.Mirrored)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "cluster.yaml", "path to cluster.yaml")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "path to a Resource Manifest (overrides the embedded one)")
	cmd.Flags().StringVar(&build, "build", "", "Build to download (default: cluster.yaml build)")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "Artifact cache directory (default: user cache dir)")
	cmd.Flags().BoolVar(&binariesOnly, "binaries-only", false, "download binaries only (also prepares Installer image inputs)")
	cmd.Flags().BoolVar(&imagesOnly, "images-only", false, "build and mirror images only (requires cached Installer binaries)")
	cmd.Flags().BoolVar(&insecure, "insecure", false, "allow plain HTTP registries")
	cmd.Flags().StringVar(&caFile, "ca-file", "", "extra CA bundle for the Registry (default: registry.tls.caFile)")
	cmd.Flags().StringVar(&credentialsFile, "credentials-file", "", "Docker-compatible credentials file (default: registry.credentialsFile)")
	return cmd
}

func newInstallCmd() *cobra.Command {
	var (
		configPath   string
		manifestPath string
		outDir       string
		ansibleDir   string
		playbook     string
		limit        string
		tags         []string
		extraVars    []string
		checkMode    bool
		verbosity    int
		skipRender   bool
	)
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the cluster by running ansible-playbook",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, m, err := loadConfigAndManifest(configPath, manifestPath)
			if err != nil {
				return err
			}
			outAbs, err := filepath.Abs(outDir)
			if err != nil {
				return fmt.Errorf("resolve rendered directory: %w", err)
			}
			ansibleAbs, err := filepath.Abs(ansibleDir)
			if err != nil {
				return fmt.Errorf("resolve ansible directory: %w", err)
			}
			if !skipRender {
				files, err := render.Render(cfg, m, outAbs)
				if err != nil {
					return err
				}
				for _, f := range files {
					fmt.Fprintf(cmd.OutOrStdout(), "rendered %s\n", f)
				}
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return install.Run(ctx, install.Options{
				AnsibleDir: ansibleAbs,
				Playbook:   playbook,
				Inventory:  filepath.Join(outAbs, "inventory.ini"),
				WorkingDir: ansibleAbs,
				ExtraVars:  extraVars,
				Limit:      limit,
				Tags:       tags,
				Check:      checkMode,
				Verbosity:  verbosity,
				Stdout:     cmd.OutOrStdout(),
				Stderr:     cmd.ErrOrStderr(),
			})
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "cluster.yaml", "path to cluster.yaml")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "path to a Resource Manifest (overrides the embedded one)")
	cmd.Flags().StringVar(&outDir, "out", "rendered", "directory for generated Ansible inventory and group_vars")
	cmd.Flags().StringVar(&ansibleDir, "ansible-dir", defaultAnsibleDir(), "directory containing site.yml, ansible.cfg and roles")
	cmd.Flags().StringVar(&playbook, "playbook", "site.yml", "playbook path, relative to --ansible-dir unless absolute")
	cmd.Flags().StringVar(&limit, "limit", "", "limit execution to matching hosts")
	cmd.Flags().StringSliceVar(&tags, "tags", nil, "only run matching Ansible tags")
	cmd.Flags().StringArrayVar(&extraVars, "extra-var", nil, "extra Ansible variable in key=value form (repeatable)")
	cmd.Flags().BoolVar(&checkMode, "check", false, "run ansible-playbook in check mode")
	cmd.Flags().CountVarP(&verbosity, "verbose", "v", "increase Ansible verbosity (repeatable)")
	cmd.Flags().BoolVar(&skipRender, "skip-render", false, "use the existing inventory instead of rendering again")
	return cmd
}

func defaultAnsibleDir() string {
	if dir := os.Getenv("KUBEDO_ANSIBLE_DIR"); dir != "" {
		return dir
	}
	return "ansible"
}

func loadConfigAndManifest(configPath, manifestPath string) (*cluster.Config, *manifest.Manifest, error) {
	cfg, err := cluster.LoadFile(configPath)
	if err != nil {
		return nil, nil, err
	}
	m, err := loadManifest(cfg.Build, manifestPath)
	if err != nil {
		return nil, nil, err
	}
	if cfg.Build != m.Metadata.Name {
		return nil, nil, fmt.Errorf("build mismatch: cluster config pins %q but manifest is %q", cfg.Build, m.Metadata.Name)
	}
	return cfg, m, nil
}

func loadManifest(build, manifestPath string) (*manifest.Manifest, error) {
	if manifestPath != "" {
		return manifest.LoadFile(manifestPath)
	}
	return manifests.Get(build)
}

func newRegistryCmd() *cobra.Command {
	var (
		configPath   string
		manifestPath string
		build        string
		cacheDir     string
		dryRun       bool
		outputDir    string
	)
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Ensure a private Registry is available (deploy or reuse)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := cluster.LoadFile(configPath)
			if err != nil {
				return err
			}
			selectedBuild := cfg.Build
			if build != "" {
				selectedBuild = build
			}
			m, err := loadManifest(selectedBuild, manifestPath)
			if err != nil {
				return err
			}
			if m.Metadata.Name != selectedBuild {
				return fmt.Errorf("manifest is for build %q, not %q", m.Metadata.Name, selectedBuild)
			}
			registryBinary, ok := m.BinaryFor("registry", cfg.Platform.ManifestOS(), cfg.Platform.Arch)
			if !ok {
				return fmt.Errorf("build %q has no installer binary named %q for %s/%s", m.Metadata.Name, "registry", cfg.Platform.ManifestOS(), cfg.Platform.Arch)
			}
			if runtime.GOOS != cfg.Platform.ManifestOS() || runtime.GOARCH != cfg.Platform.Arch {
				return fmt.Errorf("Registry must run on an Installer Host matching the target platform %s/%s; this host is %s/%s", cfg.Platform.ManifestOS(), cfg.Platform.Arch, runtime.GOOS, runtime.GOARCH)
			}
			if cacheDir == "" {
				cacheDir = defaultCacheDir()
			}
			if !dryRun && os.Geteuid() != 0 {
				return errors.New("registry deployment must run as root (use --dry-run to inspect the bundle)")
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			res, err := registry.Ensure(ctx, registry.Options{
				Address:       cfg.Registry.Address,
				DataDir:       cfg.Registry.DataDir,
				TLSMode:       cfg.Registry.TLS.Mode,
				CAFile:        cfg.Registry.TLS.CAFile,
				CertFile:      cfg.Registry.TLS.CertFile,
				KeyFile:       cfg.Registry.TLS.KeyFile,
				SourceArchive: layout.BinaryPath(cacheDir, m.Metadata.Name, registryBinary),
				DryRun:        dryRun,
				OutputDir:     outputDir,
				Logf: func(format string, args ...any) {
					fmt.Fprintf(cmd.OutOrStdout(), format+"\n", args...)
				},
			})
			if err != nil {
				return err
			}
			if dryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "registry bundle ready (dry-run)")
			} else if res.Reused {
				fmt.Fprintln(cmd.OutOrStdout(), "registry reused")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "registry ready")
			}
			for _, f := range res.Files {
				fmt.Fprintln(cmd.OutOrStdout(), f)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "cluster.yaml", "path to cluster.yaml")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "path to a Resource Manifest (overrides the embedded one)")
	cmd.Flags().StringVar(&build, "build", "", "Build whose registry binary to use (default: cluster.yaml build)")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "Artifact cache directory (default: user cache dir)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "render the bundle to --output-dir without touching the system")
	cmd.Flags().StringVar(&outputDir, "output-dir", "registry-bundle", "output directory for --dry-run")
	return cmd
}

func defaultCacheDir() string {
	if d := os.Getenv("KUBEDO_CACHE_DIR"); d != "" {
		return d
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "kubedo")
	}
	return filepath.Join(".", ".kubedo")
}
