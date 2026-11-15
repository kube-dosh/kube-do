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
)

type fakeExecutor struct {
	mu        sync.Mutex
	installed bool
	added     []cluster.Node
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
		case JobSucceeded, JobFailed:
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", id)
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
}

func TestAddNodesRejectsUnsupportedRoleAndDuplicates(t *testing.T) {
	_, _, httpServer := newTestServer(t, "")
	base := httpServer.URL
	resp := request(t, http.MethodPost, base+"/api/v1/clusters", "", []byte(testClusterYAML))
	resp.Body.Close()

	resp = request(t, http.MethodPost, base+"/api/v1/clusters/test/nodes", "", []byte(`{"role":"control-plane","nodes":[{"name":"cp-2","address":"10.0.0.12"}]}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("control-plane add status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = request(t, http.MethodPost, base+"/api/v1/clusters/test/nodes", "", []byte(`{"nodes":[{"name":"worker-1","address":"10.0.0.30"}]}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate add status = %d", resp.StatusCode)
	}
	resp.Body.Close()
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
