package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kube-dosh/kube-do/internal/cluster"
	"github.com/kube-dosh/kube-do/internal/ops"
)

type fakeExecutor struct {
	mu               sync.Mutex
	installed        bool
	added            []cluster.Node
	controlPlanes    []cluster.Node
	removedNode      string
	upgradedTo       string
	rolledBackTo     string
	restoredSnapshot string
	uninstalled      bool
	backedUp         bool
	checkedCerts     bool
	certBlock        chan struct{}
}

func (f *fakeExecutor) Install(_ context.Context, cfg *cluster.Config, _ InstallRequest, _ string, log io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installed = true
	fmt.Fprintf(log, "installed %s\n", cfg.Metadata.Name)
	return nil
}

func (f *fakeExecutor) AddWorkers(_ context.Context, _ *cluster.Config, newWorkers []cluster.Node, _ string, log io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, newWorkers...)
	fmt.Fprintf(log, "added %d workers\n", len(newWorkers))
	return nil
}

func (f *fakeExecutor) AddControlPlanes(_ context.Context, _ *cluster.Config, newNodes []cluster.Node, _, _ string, log io.Writer) (ops.BackupResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.controlPlanes = append(f.controlPlanes, newNodes...)
	fmt.Fprintf(log, "added %d control-plane nodes\n", len(newNodes))
	return ops.BackupResult{Validated: true}, nil
}

func (f *fakeExecutor) RemoveNode(_ context.Context, cfg *cluster.Config, nodeName, role, _, _ string, log io.Writer) (ops.RemoveNodeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedNode = nodeName
	fmt.Fprintf(log, "removed %s %s\n", role, nodeName)
	return ops.RemoveNodeResult{Cluster: cfg.Metadata.Name, Node: nodeName, Role: role}, nil
}

func (f *fakeExecutor) Upgrade(_ context.Context, cfg *cluster.Config, req UpgradeRequest, _, _ string, log io.Writer) (ops.UpgradeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upgradedTo = req.TargetBuild
	fmt.Fprintf(log, "upgraded %s to %s\n", cfg.Metadata.Name, req.TargetBuild)
	return ops.UpgradeResult{Cluster: cfg.Metadata.Name, ToBuild: req.TargetBuild}, nil
}

func (f *fakeExecutor) Rollback(_ context.Context, cfg *cluster.Config, req RollbackRequest, _, _ string, log io.Writer) (ops.RollbackResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rolledBackTo = req.TargetBuild
	fmt.Fprintf(log, "rolled back %s to %s\n", cfg.Metadata.Name, req.TargetBuild)
	return ops.RollbackResult{Cluster: cfg.Metadata.Name, ToBuild: req.TargetBuild}, nil
}

func (f *fakeExecutor) RestoreEtcd(_ context.Context, cfg *cluster.Config, req EtcdRestoreRequest, _, _ string, log io.Writer) (ops.RestoreResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restoredSnapshot = req.SnapshotPath
	fmt.Fprintf(log, "restored %s from %s\n", cfg.Metadata.Name, req.SnapshotPath)
	return ops.RestoreResult{Cluster: cfg.Metadata.Name, SnapshotPath: req.SnapshotPath}, nil
}

func (f *fakeExecutor) Uninstall(_ context.Context, cfg *cluster.Config, _ UninstallRequest, _ string, log io.Writer) (ops.UninstallResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uninstalled = true
	fmt.Fprintf(log, "uninstalled %s\n", cfg.Metadata.Name)
	return ops.UninstallResult{Cluster: cfg.Metadata.Name}, nil
}

func (f *fakeExecutor) BackupEtcd(_ context.Context, cfg *cluster.Config, _, backupDir string, _ ops.BackupOptions, log io.Writer) (ops.BackupResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.backedUp = true
	result := ops.BackupResult{
		Cluster:   cfg.Metadata.Name,
		Path:      backupDir + "/snapshot.db",
		SHA256:    strings.Repeat("a", 64),
		Size:      1024,
		Validated: true,
	}
	fmt.Fprintln(log, "etcd backup complete")
	return result, nil
}

func (f *fakeExecutor) CheckCerts(ctx context.Context, cfg *cluster.Config, _ string, _ ops.CertCheckOptions, log io.Writer) (ops.CertCheckResult, error) {
	f.mu.Lock()
	f.checkedCerts = true
	block := f.certBlock
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ops.CertCheckResult{}, ctx.Err()
		}
	}
	fmt.Fprintln(log, "certificate check complete")
	return ops.CertCheckResult{Cluster: cfg.Metadata.Name, Healthy: true, CertificateCount: 1}, nil
}

const testClusterYAML = `apiVersion: kube-do.io/v1alpha1
kind: ClusterConfig
metadata:
  name: test
build: test-build
installer:
  address: 10.0.0.10
registry:
  address: 10.0.0.10:5000
controlPlane:
  vip: 10.0.0.100
  nodes:
    - name: cp-1
      address: 10.0.0.11
workers:
  - name: worker-1
    address: 10.0.0.21
cni:
  name: cilium
  kubeProxyReplacement: true
`

func newTestServer(t *testing.T, token string) (*Server, *fakeExecutor, *httptest.Server) {
	t.Helper()
	executor := &fakeExecutor{}
	srv, err := New(Options{
		Addr:     "127.0.0.1:0",
		DataDir:  t.TempDir(),
		Token:    token,
		Logger:   log.New(io.Discard, "", 0),
		Executor: executor,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	httpServer := httptest.NewServer(srv.Handler())
	t.Cleanup(httpServer.Close)
	return srv, executor, httpServer
}

func request(t *testing.T, method, url, token string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var value T
	if err := json.NewDecoder(resp.Body).Decode(&value); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return value
}

func waitJob(t *testing.T, baseURL, id string) Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp := request(t, http.MethodGet, baseURL+"/api/v1/jobs/"+id, "", nil)
		job := decode[Job](t, resp)
		switch job.Status {
		case JobSucceeded, JobFailed, JobCancelled, JobInterrupted:
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", id)
	return Job{}
}

func waitJobStatus(t *testing.T, baseURL, id string, want JobStatus) Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp := request(t, http.MethodGet, baseURL+"/api/v1/jobs/"+id, "", nil)
		job := decode[Job](t, resp)
		if job.Status == want {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach status %s", id, want)
	return Job{}
}

func TestClusterLifecycleAPI(t *testing.T) {
	srv, executor, httpServer := newTestServer(t, "")
	base := httpServer.URL

	resp := request(t, http.MethodPost, base+"/api/v1/clusters", "", []byte(testClusterYAML))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = request(t, http.MethodGet, base+"/api/v1/clusters", "", nil)
	var list struct {
		Clusters []ClusterSummary `json:"clusters"`
	}
	list = decode[struct {
		Clusters []ClusterSummary `json:"clusters"`
	}](t, resp)
	if len(list.Clusters) != 1 || list.Clusters[0].Name != "test" || list.Clusters[0].NodeCount != 2 {
		t.Fatalf("clusters = %#v", list.Clusters)
	}

	resp = request(t, http.MethodPost, base+"/api/v1/clusters/test/install", "", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("install status = %d", resp.StatusCode)
	}
	installJob := decode[Job](t, resp)
	installJob = waitJob(t, base, installJob.ID)
	if installJob.Status != JobSucceeded {
		t.Fatalf("install job = %#v", installJob)
	}
	executor.mu.Lock()
	installed := executor.installed
	executor.mu.Unlock()
	if !installed {
		t.Fatal("executor was not called")
	}

	body := []byte(`{"role":"worker","nodes":[{"name":"worker-2","address":"10.0.0.22"}]}`)
	resp = request(t, http.MethodPost, base+"/api/v1/clusters/test/nodes", "", body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("add nodes status = %d", resp.StatusCode)
	}
	addJob := decode[Job](t, resp)
	addJob = waitJob(t, base, addJob.ID)
	if addJob.Status != JobSucceeded {
		t.Fatalf("add job = %#v", addJob)
	}

	cfg, err := srv.store.Load("test")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Workers) != 2 || cfg.Workers[1].Name != "worker-2" {
		t.Fatalf("workers = %#v", cfg.Workers)
	}

	resp = request(t, http.MethodGet, base+"/api/v1/jobs/"+addJob.ID+"/logs", "", nil)
	logs, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logs), "added 1 workers") {
		t.Fatalf("logs = %q", logs)
	}

	resp = request(t, http.MethodPost, base+"/api/v1/clusters/test/etcd-backups", "", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("etcd backup status = %d", resp.StatusCode)
	}
	backupJob := decode[Job](t, resp)
	backupJob = waitJob(t, base, backupJob.ID)
	if backupJob.Status != JobSucceeded {
		t.Fatalf("etcd backup job = %#v", backupJob)
	}
	result, ok := backupJob.Result.(map[string]any)
	if !ok || result["path"] == "" {
		t.Fatalf("etcd backup result = %#v", backupJob.Result)
	}

	resp = request(t, http.MethodPost, base+"/api/v1/clusters/test/certs/check", "", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("certs check status = %d", resp.StatusCode)
	}
	certJob := decode[Job](t, resp)
	certJob = waitJob(t, base, certJob.ID)
	if certJob.Status != JobSucceeded {
		t.Fatalf("certs check job = %#v", certJob)
	}
	if result, ok := certJob.Result.(map[string]any); !ok || result["healthy"] != true {
		t.Fatalf("certs check result = %#v", certJob.Result)
	}
	executor.mu.Lock()
	backedUp, checkedCerts := executor.backedUp, executor.checkedCerts
	executor.mu.Unlock()
	if !backedUp || !checkedCerts {
		t.Fatalf("executor calls: backup=%v certs=%v", backedUp, checkedCerts)
	}
}

func TestCancelRunningJobAPI(t *testing.T) {
	_, executor, httpServer := newTestServer(t, "")
	executor.certBlock = make(chan struct{})
	defer close(executor.certBlock)
	base := httpServer.URL

	resp := request(t, http.MethodPost, base+"/api/v1/clusters", "", []byte(testClusterYAML))
	resp.Body.Close()
	resp = request(t, http.MethodPost, base+"/api/v1/clusters/test/certs/check", "", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("certs check status = %d", resp.StatusCode)
	}
	job := decode[Job](t, resp)
	waitJobStatus(t, base, job.ID, JobRunning)

	resp = request(t, http.MethodPost, base+"/api/v1/jobs/"+job.ID+"/cancel", "", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("cancel status = %d", resp.StatusCode)
	}
	resp.Body.Close()
	job = waitJob(t, base, job.ID)
	if job.Status != JobCancelled {
		t.Fatalf("cancelled job = %#v", job)
	}
}

func TestAddNodesSupportsControlPlaneAndRejectsDuplicates(t *testing.T) {
	_, executor, httpServer := newTestServer(t, "")
	base := httpServer.URL
	resp := request(t, http.MethodPost, base+"/api/v1/clusters", "", []byte(testClusterYAML))
	resp.Body.Close()

	resp = request(t, http.MethodPost, base+"/api/v1/clusters/test/nodes", "", []byte(`{"role":"control-plane","nodes":[{"name":"cp-2","address":"10.0.0.12"}]}`))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("control-plane add status = %d", resp.StatusCode)
	}
	job := waitJob(t, base, decode[Job](t, resp).ID)
	if job.Status != JobSucceeded {
		t.Fatalf("control-plane add job = %#v", job)
	}
	executor.mu.Lock()
	if len(executor.controlPlanes) != 1 || executor.controlPlanes[0].Name != "cp-2" {
		t.Fatalf("control-plane executor calls = %#v", executor.controlPlanes)
	}
	executor.mu.Unlock()

	resp = request(t, http.MethodPost, base+"/api/v1/clusters/test/nodes", "", []byte(`{"nodes":[{"name":"worker-1","address":"10.0.0.30"}]}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate add status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestLifecycleOperationAPIs(t *testing.T) {
	srv, executor, httpServer := newTestServer(t, "")
	base := httpServer.URL
	resp := request(t, http.MethodPost, base+"/api/v1/clusters", "", []byte(testClusterYAML))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = request(t, http.MethodPost, base+"/api/v1/clusters/test/upgrade", "", []byte(`{"targetBuild":"test-build-2","prepare":false,"check":false}`))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("upgrade status = %d", resp.StatusCode)
	}
	job := waitJob(t, base, decode[Job](t, resp).ID)
	if job.Status != JobSucceeded {
		t.Fatalf("upgrade job = %#v", job)
	}
	cfg, err := srv.store.Load("test")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Build != "test-build-2" {
		t.Fatalf("build after upgrade = %q", cfg.Build)
	}

	resp = request(t, http.MethodPost, base+"/api/v1/clusters/test/upgrade/rollback", "", []byte(`{"targetBuild":"test-build","snapshotPath":"/var/lib/kube-do/backups/test.db","confirm":true,"prepare":false}`))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("rollback status = %d", resp.StatusCode)
	}
	job = waitJob(t, base, decode[Job](t, resp).ID)
	if job.Status != JobSucceeded {
		t.Fatalf("rollback job = %#v", job)
	}
	cfg, err = srv.store.Load("test")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Build != "test-build" {
		t.Fatalf("build after rollback = %q", cfg.Build)
	}

	resp = request(t, http.MethodPost, base+"/api/v1/clusters/test/etcd-restores", "", []byte(`{"snapshotPath":"/var/lib/kube-do/backups/test.db","confirm":true}`))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("etcd restore status = %d", resp.StatusCode)
	}
	job = waitJob(t, base, decode[Job](t, resp).ID)
	if job.Status != JobSucceeded {
		t.Fatalf("etcd restore job = %#v", job)
	}

	resp = request(t, http.MethodDelete, base+"/api/v1/clusters/test/nodes/worker-1?confirm=true", "", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("remove worker status = %d", resp.StatusCode)
	}
	job = waitJob(t, base, decode[Job](t, resp).ID)
	if job.Status != JobSucceeded {
		t.Fatalf("remove worker job = %#v", job)
	}
	cfg, err = srv.store.Load("test")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Workers) != 0 {
		t.Fatalf("workers after removal = %#v", cfg.Workers)
	}

	resp = request(t, http.MethodDelete, base+"/api/v1/clusters/test/nodes/cp-1?confirm=true", "", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("remove last control-plane status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = request(t, http.MethodPost, base+"/api/v1/clusters/test/uninstall", "", []byte(`{"confirm":true}`))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("uninstall status = %d", resp.StatusCode)
	}
	job = waitJob(t, base, decode[Job](t, resp).ID)
	if job.Status != JobSucceeded {
		t.Fatalf("uninstall job = %#v", job)
	}
	resp = request(t, http.MethodGet, base+"/api/v1/clusters/test", "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cluster after uninstall status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	executor.mu.Lock()
	if executor.upgradedTo != "test-build-2" || executor.rolledBackTo != "test-build" || executor.restoredSnapshot == "" || executor.removedNode != "worker-1" || !executor.uninstalled {
		t.Fatalf("executor lifecycle calls = %#v", executor)
	}
	executor.mu.Unlock()
}

func TestLifecycleOperationsRequireConfirmation(t *testing.T) {
	_, _, httpServer := newTestServer(t, "")
	base := httpServer.URL
	resp := request(t, http.MethodPost, base+"/api/v1/clusters", "", []byte(testClusterYAML))
	resp.Body.Close()

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"rollback", http.MethodPost, "/api/v1/clusters/test/upgrade/rollback", `{"targetBuild":"test-build","snapshotPath":"/tmp/snapshot.db"}`},
		{"restore", http.MethodPost, "/api/v1/clusters/test/etcd-restores", `{"snapshotPath":"/tmp/snapshot.db"}`},
		{"uninstall", http.MethodPost, "/api/v1/clusters/test/uninstall", `{}`},
		{"remove", http.MethodDelete, "/api/v1/clusters/test/nodes/worker-1", ``},
	} {
		resp := request(t, tc.method, base+tc.path, "", []byte(tc.body))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s confirmation status = %d", tc.name, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestAPIToken(t *testing.T) {
	_, _, httpServer := newTestServer(t, "secret")
	base := httpServer.URL

	resp := request(t, http.MethodGet, base+"/healthz", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", resp.StatusCode)
	}

	resp = request(t, http.MethodGet, base+"/api/v1/clusters", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", resp.StatusCode)
	}

	resp = request(t, http.MethodGet, base+"/api/v1/clusters", "secret", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated status = %d", resp.StatusCode)
	}
}
