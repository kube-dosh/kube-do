package cli

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/kube-dosh/kube-do/internal/daemon"
	"github.com/kube-dosh/kube-do/internal/ops"
)

func newDaemonCmd() *cobra.Command {
	var (
		addr             string
		dataDir          string
		cacheDir         string
		ansibleDir       string
		manifestPath     string
		token            string
		artifactAddr     string
		jobTimeout       time.Duration
		maxLogBytes      int64
		backupTimeout    time.Duration
		backupRetain     int
		backupMinFree    int64
		certTimeout      time.Duration
		certWarnDays     int
		certCriticalDays int
	)
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the cluster management HTTP API",
		Long:  "Run the kube-do daemon, which exposes cluster registration, asynchronous installation, worker addition and job status/log APIs.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cacheDir == "" {
				cacheDir = defaultCacheDir()
			}
			if dataDir == "" {
				dataDir = defaultStateDir()
			}
			if ansibleDir == "" {
				ansibleDir = defaultAnsibleDir()
			}
			if token == "" {
				token = os.Getenv("KUBE_DO_API_TOKEN")
			}
			logger := log.New(cmd.ErrOrStderr(), "kube-do-daemon: ", log.LstdFlags)
			srv, err := daemon.New(daemon.Options{
				Addr:           addr,
				DataDir:        dataDir,
				CacheDir:       cacheDir,
				AnsibleDir:     ansibleDir,
				ManifestPath:   manifestPath,
				Token:          token,
				ArtifactAddr:   artifactAddr,
				Logger:         logger,
				JobTimeout:     jobTimeout,
				MaxJobLogBytes: int(maxLogBytes),
				BackupOptions:  ops.BackupOptions{Timeout: backupTimeout, Retain: backupRetain, MinFreeBytes: backupMinFree},
				CertOptions:    ops.CertCheckOptions{Timeout: certTimeout, WarnDays: certWarnDays, CriticalDays: certCriticalDays},
			})
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return srv.Run(ctx)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:8090", "API listen address")
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "daemon state directory (default: user state dir)")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "Artifact cache directory (default: user cache dir)")
	cmd.Flags().StringVar(&ansibleDir, "ansible-dir", "", "directory containing site.yml, add-worker.yml and roles")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "path to a Resource Manifest (overrides the embedded one)")
	cmd.Flags().StringVar(&token, "api-token", "", "require this bearer token for /api/ requests (default: KUBE_DO_API_TOKEN)")
	cmd.Flags().StringVar(&artifactAddr, "artifact-addr", ":8080", "Artifact download listen address (empty disables the second server)")
	cmd.Flags().DurationVar(&jobTimeout, "job-timeout", daemon.DefaultJobTimeout, "maximum runtime for one API job")
	cmd.Flags().Int64Var(&maxLogBytes, "max-job-log-bytes", daemon.DefaultMaxJobLogBytes, "maximum retained log bytes per job")
	cmd.Flags().DurationVar(&backupTimeout, "backup-timeout", ops.DefaultBackupTimeout, "etcd backup timeout")
	cmd.Flags().IntVar(&backupRetain, "backup-retain", 0, "keep only the newest N etcd snapshots per cluster (0 keeps all)")
	cmd.Flags().Int64Var(&backupMinFree, "backup-min-free-bytes", ops.DefaultBackupMinFreeBytes, "minimum free space required for etcd backups")
	cmd.Flags().DurationVar(&certTimeout, "cert-timeout", ops.DefaultCertCheckTimeout, "certificate check timeout")
	cmd.Flags().IntVar(&certWarnDays, "cert-warn-days", ops.DefaultCertWarnDays, "warn when a certificate expires within this many days")
	cmd.Flags().IntVar(&certCriticalDays, "cert-critical-days", ops.DefaultCertCriticalDays, "mark a certificate critical when it expires within this many days")
	return cmd
}

func defaultStateDir() string {
	if dir := os.Getenv("KUBE_DO_STATE_DIR"); dir != "" {
		return dir
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "kube-do", "daemon")
	}
	return filepath.Join(".", ".kube-do", "daemon")
}
