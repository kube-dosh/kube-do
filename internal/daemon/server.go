package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kube-dosh/kube-do/internal/cluster"
	"github.com/kube-dosh/kube-do/internal/ops"
	"github.com/kube-dosh/kube-do/internal/serve"
)

const maxRequestBody = 1 << 20

type Options struct {
	Addr         string
	DataDir      string
	CacheDir     string
	AnsibleDir   string
	ManifestPath string
	Token        string
	// ArtifactAddr optionally starts a second HTTP server that serves the
	// Artifact cache at the same /artifacts/ URL used by cluster nodes.
	ArtifactAddr   string
	Logger         *log.Logger
	Executor       Executor
	JobTimeout     time.Duration
	MaxJobLogBytes int
	BackupOptions  ops.BackupOptions
	CertOptions    ops.CertCheckOptions
}

type Server struct {
	opts  Options
	store *Store
	jobs  *JobManager
}

func New(opts Options) (*Server, error) {
	if strings.TrimSpace(opts.DataDir) == "" {
		return nil, errors.New("data directory is required")
	}
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:8090"
	}
	dataDir, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data directory: %w", err)
	}
	opts.DataDir = dataDir
	if opts.Logger == nil {
		opts.Logger = log.New(os.Stderr, "kube-do-daemon: ", log.LstdFlags)
	}
	if opts.Executor == nil {
		if opts.CacheDir == "" {
			return nil, errors.New("cache directory is required")
		}
		if opts.AnsibleDir == "" {
			return nil, errors.New("ansible directory is required")
		}
		cacheDir, err := filepath.Abs(opts.CacheDir)
		if err != nil {
			return nil, fmt.Errorf("resolve cache directory: %w", err)
		}
		ansibleDir, err := filepath.Abs(opts.AnsibleDir)
		if err != nil {
			return nil, fmt.Errorf("resolve ansible directory: %w", err)
		}
		opts.CacheDir = cacheDir
		opts.AnsibleDir = ansibleDir
		opts.Executor = &HostExecutor{
			CacheDir:     cacheDir,
			AnsibleDir:   ansibleDir,
			ManifestPath: opts.ManifestPath,
		}
	}
	store, err := NewStore(opts.DataDir)
	if err != nil {
		return nil, err
	}
	jobs, err := NewJobManager(JobManagerOptions{
		StateDir:    filepath.Join(opts.DataDir, "jobs"),
		Timeout:     opts.JobTimeout,
		MaxLogBytes: opts.MaxJobLogBytes,
	})
	if err != nil {
		return nil, err
	}
	return &Server{opts: opts, store: store, jobs: jobs}, nil
}

func (s *Server) Run(ctx context.Context) error {
	s.jobs.SetBaseContext(ctx)
	if s.opts.ArtifactAddr != "" && s.opts.ArtifactAddr != s.opts.Addr && s.opts.CacheDir != "" {
		go func() {
			err := (&serve.Server{Addr: s.opts.ArtifactAddr, Dir: s.opts.CacheDir}).Run(ctx)
			if err != nil && ctx.Err() == nil {
				s.opts.Logger.Printf("artifact server: %v", err)
			}
		}()
	}
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.opts.Addr, err)
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	s.opts.Logger.Printf("API listening on http://%s", ln.Addr())
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/v1/clusters", s.handleListClusters)
	mux.HandleFunc("POST /api/v1/clusters", s.handleCreateCluster)
	mux.HandleFunc("GET /api/v1/clusters/{name}", s.handleGetCluster)
	mux.HandleFunc("POST /api/v1/clusters/{name}/install", s.handleInstallCluster)
	mux.HandleFunc("POST /api/v1/clusters/{name}/nodes", s.handleAddNodes)
	mux.HandleFunc("DELETE /api/v1/clusters/{name}/nodes/{node}", s.handleRemoveNode)
	mux.HandleFunc("POST /api/v1/clusters/{name}/uninstall", s.handleUninstallCluster)
	mux.HandleFunc("POST /api/v1/clusters/{name}/upgrade", s.handleUpgradeCluster)
	mux.HandleFunc("POST /api/v1/clusters/{name}/upgrade/rollback", s.handleRollbackCluster)
	mux.HandleFunc("POST /api/v1/clusters/{name}/etcd-backups", s.handleEtcdBackup)
	mux.HandleFunc("POST /api/v1/clusters/{name}/etcd-restores", s.handleRestoreEtcd)
	mux.HandleFunc("POST /api/v1/clusters/{name}/certs/check", s.handleCheckCerts)
	mux.HandleFunc("GET /api/v1/jobs", s.handleListJobs)
	mux.HandleFunc("GET /api/v1/jobs/{id}", s.handleGetJob)
	mux.HandleFunc("GET /api/v1/jobs/{id}/logs", s.handleGetJobLogs)
	mux.HandleFunc("POST /api/v1/jobs/{id}/cancel", s.handleCancelJob)
	if s.opts.CacheDir != "" {
		mux.Handle("GET /artifacts/", http.FileServer(http.Dir(s.opts.CacheDir)))
	}
	return s.authenticate(mux)
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	if s.opts.Token == "" {
		return next
	}
	expected := []byte("Bearer " + s.opts.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			got := []byte(r.Header.Get("Authorization"))
			if len(got) != len(expected) || subtle.ConstantTimeCompare(got, expected) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleCreateCluster(w http.ResponseWriter, r *http.Request) {
	body, err := readLimited(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg, err := cluster.Parse(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	exists, err := s.store.Exists(cfg.Metadata.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if exists {
		writeError(w, http.StatusConflict, fmt.Sprintf("cluster %q already exists", cfg.Metadata.Name))
		return
	}
	if err := s.store.Save(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	detail, err := clusterDetail(cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, detail)
}

func (s *Server) handleListClusters(w http.ResponseWriter, _ *http.Request) {
	clusters, err := s.store.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]ClusterSummary, 0, len(clusters))
	for _, cfg := range clusters {
		out = append(out, clusterSummary(cfg))
	}
	writeJSON(w, http.StatusOK, map[string]any{"clusters": out})
}

func (s *Server) handleGetCluster(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.store.Load(r.PathValue("name"))
	if errors.Is(err, ErrClusterNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	detail, err := clusterDetail(cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleInstallCluster(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, err := s.store.Load(name)
	if errors.Is(err, ErrClusterNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req InstallRequest
	if err := decodeOptionalJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	job, err := s.jobs.Start("install", name, nil, func(ctx context.Context, id string, log io.Writer) error {
		workDir, err := ensureWorkDir(s.opts.DataDir, id)
		if err != nil {
			return err
		}
		return s.opts.Executor.Install(ctx, cfg, req, workDir, log)
	})
	if errors.Is(err, ErrClusterBusy) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handleAddNodes(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, err := s.store.Load(name)
	if errors.Is(err, ErrClusterNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var req AddNodesRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	role := strings.ToLower(strings.TrimSpace(req.Role))
	if role == "" {
		role = "worker"
	}
	if role != "worker" && role != "control-plane" {
		writeError(w, http.StatusBadRequest, "role must be worker or control-plane")
		return
	}
	if len(req.Nodes) == 0 {
		writeError(w, http.StatusBadRequest, "nodes must not be empty")
		return
	}

	newNodes := make([]cluster.Node, 0, len(req.Nodes))
	apiNodes := make([]Node, 0, len(req.Nodes))
	for _, node := range req.Nodes {
		newNodes = append(newNodes, cluster.Node{Name: node.Name, Address: node.Address})
		apiNodes = append(apiNodes, Node{Name: node.Name, Address: node.Address})
	}
	updated := *cfg
	if role == "control-plane" {
		updated.ControlPlane.Nodes = append(append([]cluster.Node(nil), cfg.ControlPlane.Nodes...), newNodes...)
	} else {
		updated.Workers = append(append([]cluster.Node(nil), cfg.Workers...), newNodes...)
	}
	if err := updated.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	kind := "add-workers"
	if role == "control-plane" {
		kind = "add-control-plane"
	}
	backupDir := filepath.Join(s.opts.DataDir, "backups", name)
	job, err := s.jobs.Start(kind, name, apiNodes, func(ctx context.Context, id string, log io.Writer) error {
		workDir, err := ensureWorkDir(s.opts.DataDir, id)
		if err != nil {
			return err
		}
		if role == "control-plane" {
			if _, err := s.opts.Executor.AddControlPlanes(ctx, &updated, newNodes, workDir, backupDir, log); err != nil {
				return err
			}
		} else {
			if err := s.opts.Executor.AddWorkers(ctx, &updated, newNodes, workDir, log); err != nil {
				return err
			}
		}
		return s.store.Save(&updated)
	})
	if errors.Is(err, ErrClusterBusy) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handleRemoveNode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, err := s.store.Load(name)
	if errors.Is(err, ErrClusterNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if r.URL.Query().Get("confirm") != "true" {
		writeError(w, http.StatusBadRequest, "node removal requires confirm=true")
		return
	}
	nodeName := r.PathValue("node")
	role := ""
	if _, ok := nodeByName(cfg.ControlPlane.Nodes, nodeName); ok {
		role = "control-plane"
	} else if _, ok := nodeByName(cfg.Workers, nodeName); ok {
		role = "worker"
	} else {
		writeError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", nodeName))
		return
	}
	if role == "control-plane" && len(cfg.ControlPlane.Nodes) <= 1 {
		writeError(w, http.StatusConflict, "refusing to remove the last control-plane node")
		return
	}
	backupDir := filepath.Join(s.opts.DataDir, "backups", name)
	job, err := s.jobs.Start("remove-node", name, []Node{{Name: nodeName}}, func(ctx context.Context, id string, log io.Writer) error {
		workDir, err := ensureWorkDir(s.opts.DataDir, id)
		if err != nil {
			return err
		}
		result, err := s.opts.Executor.RemoveNode(ctx, cfg, nodeName, role, workDir, backupDir, log)
		if err != nil {
			return err
		}
		updated := *cfg
		if role == "control-plane" {
			updated.ControlPlane.Nodes = removeNode(cfg.ControlPlane.Nodes, nodeName)
		} else {
			updated.Workers = removeNode(cfg.Workers, nodeName)
		}
		if err := s.store.Save(&updated); err != nil {
			return err
		}
		s.jobs.SetResult(id, result)
		return nil
	})
	if errors.Is(err, ErrClusterBusy) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handleUninstallCluster(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, err := s.store.Load(name)
	if errors.Is(err, ErrClusterNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req UninstallRequest
	if err := decodeOptionalJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !req.Confirm {
		writeError(w, http.StatusBadRequest, "cluster uninstall requires confirm=true")
		return
	}
	job, err := s.jobs.Start("uninstall", name, nil, func(ctx context.Context, id string, log io.Writer) error {
		workDir, err := ensureWorkDir(s.opts.DataDir, id)
		if err != nil {
			return err
		}
		result, err := s.opts.Executor.Uninstall(ctx, cfg, req, workDir, log)
		if err != nil {
			return err
		}
		if !req.KeepCluster {
			if err := s.store.Delete(name); err != nil {
				return err
			}
		}
		s.jobs.SetResult(id, result)
		return nil
	})
	if errors.Is(err, ErrClusterBusy) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handleUpgradeCluster(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, err := s.store.Load(name)
	if errors.Is(err, ErrClusterNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req UpgradeRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.TargetBuild = strings.TrimSpace(req.TargetBuild)
	if req.TargetBuild == "" {
		writeError(w, http.StatusBadRequest, "targetBuild is required")
		return
	}
	if req.TargetBuild == cfg.Build {
		writeError(w, http.StatusConflict, fmt.Sprintf("cluster already uses build %q", cfg.Build))
		return
	}
	backupDir := filepath.Join(s.opts.DataDir, "backups", name)
	job, err := s.jobs.Start("upgrade", name, nil, func(ctx context.Context, id string, log io.Writer) error {
		workDir, err := ensureWorkDir(s.opts.DataDir, id)
		if err != nil {
			return err
		}
		result, err := s.opts.Executor.Upgrade(ctx, cfg, req, workDir, backupDir, log)
		if err != nil {
			return err
		}
		updated := *cfg
		updated.Build = req.TargetBuild
		if err := s.store.Save(&updated); err != nil {
			return err
		}
		s.jobs.SetResult(id, result)
		return nil
	})
	if errors.Is(err, ErrClusterBusy) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handleRollbackCluster(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, err := s.store.Load(name)
	if errors.Is(err, ErrClusterNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req RollbackRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.TargetBuild = strings.TrimSpace(req.TargetBuild)
	req.SnapshotPath = strings.TrimSpace(req.SnapshotPath)
	if req.TargetBuild == "" || req.SnapshotPath == "" {
		writeError(w, http.StatusBadRequest, "targetBuild and snapshotPath are required")
		return
	}
	if !req.Confirm {
		writeError(w, http.StatusBadRequest, "upgrade rollback requires confirm=true")
		return
	}
	backupDir := filepath.Join(s.opts.DataDir, "backups", name)
	job, err := s.jobs.Start("upgrade-rollback", name, nil, func(ctx context.Context, id string, log io.Writer) error {
		workDir, err := ensureWorkDir(s.opts.DataDir, id)
		if err != nil {
			return err
		}
		result, err := s.opts.Executor.Rollback(ctx, cfg, req, workDir, backupDir, log)
		if err != nil {
			return err
		}
		updated := *cfg
		updated.Build = req.TargetBuild
		if err := s.store.Save(&updated); err != nil {
			return err
		}
		s.jobs.SetResult(id, result)
		return nil
	})
	if errors.Is(err, ErrClusterBusy) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handleEtcdBackup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, err := s.store.Load(name)
	if errors.Is(err, ErrClusterNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	backupDir := filepath.Join(s.opts.DataDir, "backups", name)
	job, err := s.jobs.Start("etcd-backup", name, nil, func(ctx context.Context, id string, log io.Writer) error {
		workDir, err := ensureWorkDir(s.opts.DataDir, id)
		if err != nil {
			return err
		}
		result, err := s.opts.Executor.BackupEtcd(ctx, cfg, workDir, backupDir, s.opts.BackupOptions, log)
		if err != nil {
			return err
		}
		s.jobs.SetResult(id, result)
		return nil
	})
	if errors.Is(err, ErrClusterBusy) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handleRestoreEtcd(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, err := s.store.Load(name)
	if errors.Is(err, ErrClusterNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req EtcdRestoreRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.SnapshotPath = strings.TrimSpace(req.SnapshotPath)
	req.TargetNode = strings.TrimSpace(req.TargetNode)
	if req.SnapshotPath == "" {
		writeError(w, http.StatusBadRequest, "snapshotPath is required")
		return
	}
	if !req.Confirm {
		writeError(w, http.StatusBadRequest, "etcd restore requires confirm=true")
		return
	}
	backupDir := filepath.Join(s.opts.DataDir, "backups", name)
	job, err := s.jobs.Start("etcd-restore", name, nil, func(ctx context.Context, id string, log io.Writer) error {
		workDir, err := ensureWorkDir(s.opts.DataDir, id)
		if err != nil {
			return err
		}
		result, err := s.opts.Executor.RestoreEtcd(ctx, cfg, req, workDir, backupDir, log)
		if err != nil {
			return err
		}
		s.jobs.SetResult(id, result)
		return nil
	})
	if errors.Is(err, ErrClusterBusy) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handleCheckCerts(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg, err := s.store.Load(name)
	if errors.Is(err, ErrClusterNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	job, err := s.jobs.Start("certs-check", name, nil, func(ctx context.Context, id string, log io.Writer) error {
		workDir, err := ensureWorkDir(s.opts.DataDir, id)
		if err != nil {
			return err
		}
		result, err := s.opts.Executor.CheckCerts(ctx, cfg, workDir, s.opts.CertOptions, log)
		if err != nil {
			return err
		}
		s.jobs.SetResult(id, result)
		return nil
	})
	if errors.Is(err, ErrClusterBusy) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs := s.jobs.List(r.URL.Query().Get("cluster"))
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.jobs.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleGetJobLogs(w http.ResponseWriter, r *http.Request) {
	logs, ok := s.jobs.Logs(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, logs)
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.jobs.Cancel(r.PathValue("id"))
	switch {
	case errors.Is(err, ErrJobNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrJobFinished):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeJSON(w, http.StatusAccepted, job)
	}
}

func clusterSummary(cfg *cluster.Config) ClusterSummary {
	return ClusterSummary{
		Name:         cfg.Metadata.Name,
		Build:        cfg.Build,
		ControlPlane: toNodeViews(cfg.ControlPlane.Nodes, "control-plane"),
		Workers:      toNodeViews(cfg.Workers, "worker"),
		NodeCount:    len(cfg.ControlPlane.Nodes) + len(cfg.Workers),
	}
}

func clusterDetail(cfg *cluster.Config) (ClusterDetail, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return ClusterDetail{}, fmt.Errorf("encode cluster config: %w", err)
	}
	var document any
	if err := yaml.Unmarshal(data, &document); err != nil {
		return ClusterDetail{}, fmt.Errorf("decode cluster config: %w", err)
	}
	return ClusterDetail{
		Name:         cfg.Metadata.Name,
		Build:        cfg.Build,
		Config:       document,
		ControlPlane: toNodeViews(cfg.ControlPlane.Nodes, "control-plane"),
		Workers:      toNodeViews(cfg.Workers, "worker"),
	}, nil
}

func nodeByName(nodes []cluster.Node, name string) (cluster.Node, bool) {
	for _, node := range nodes {
		if node.Name == name {
			return node, true
		}
	}
	return cluster.Node{}, false
}

func removeNode(nodes []cluster.Node, name string) []cluster.Node {
	out := make([]cluster.Node, 0, len(nodes))
	for _, node := range nodes {
		if node.Name != name {
			out = append(out, node)
		}
	}
	return out
}

func decodeOptionalJSON(r io.Reader, value any) error {
	data, err := readLimited(r)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode JSON request: %w", err)
	}
	return nil
}

func decodeJSON(r io.Reader, value any) error {
	data, err := readLimited(r)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return errors.New("request body is required")
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode JSON request: %w", err)
	}
	return nil
}

func readLimited(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxRequestBody+1))
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	if len(data) > maxRequestBody {
		return nil, errors.New("request body exceeds 1 MiB")
	}
	return data, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
