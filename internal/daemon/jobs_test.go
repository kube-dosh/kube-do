package daemon

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitManagerJob(t *testing.T, manager *JobManager, id string) Job {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		job, ok := manager.Get(id)
		if !ok {
			t.Fatalf("job %s not found", id)
		}
		switch job.Status {
		case JobSucceeded, JobFailed, JobCancelled, JobInterrupted:
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", id)
	return Job{}
}

func TestJobManagerPersistsHistoryResultAndLogs(t *testing.T) {
	stateDir := t.TempDir()
	manager, err := NewJobManager(JobManagerOptions{StateDir: stateDir, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	job, err := manager.Start("test", "demo", nil, func(_ context.Context, id string, log io.Writer) error {
		fmt.Fprintln(log, "persistent hello")
		manager.SetResult(id, map[string]string{"status": "ok"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	job = waitManagerJob(t, manager, job.ID)
	if job.Status != JobSucceeded {
		t.Fatalf("job = %#v", job)
	}

	reloaded, err := NewJobManager(JobManagerOptions{StateDir: stateDir, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Get(job.ID)
	if !ok || got.Status != JobSucceeded {
		t.Fatalf("reloaded job = %#v, ok=%v", got, ok)
	}
	result, ok := got.Result.(map[string]any)
	if !ok || result["status"] != "ok" {
		t.Fatalf("reloaded result = %#v", got.Result)
	}
	logs, _ := reloaded.Logs(job.ID)
	if !strings.Contains(logs, "persistent hello") {
		t.Fatalf("reloaded logs = %q", logs)
	}
}

func TestJobManagerTimeoutCancellationAndLogLimit(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		manager, err := NewJobManager(JobManagerOptions{StateDir: t.TempDir(), Timeout: 30 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		job, err := manager.Start("test", "timeout", nil, func(ctx context.Context, _ string, _ io.Writer) error {
			<-ctx.Done()
			return ctx.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		job = waitManagerJob(t, manager, job.ID)
		if job.Status != JobFailed || !strings.Contains(job.Error, "timed out") {
			t.Fatalf("job = %#v", job)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		manager, err := NewJobManager(JobManagerOptions{StateDir: t.TempDir(), Timeout: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		started := make(chan struct{})
		job, err := manager.Start("test", "cancel", nil, func(ctx context.Context, _ string, _ io.Writer) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		<-started
		if _, err := manager.Cancel(job.ID); err != nil {
			t.Fatal(err)
		}
		job = waitManagerJob(t, manager, job.ID)
		if job.Status != JobCancelled {
			t.Fatalf("job = %#v", job)
		}
	})

	t.Run("log limit", func(t *testing.T) {
		manager, err := NewJobManager(JobManagerOptions{StateDir: t.TempDir(), Timeout: time.Minute, MaxLogBytes: 64})
		if err != nil {
			t.Fatal(err)
		}
		job, err := manager.Start("test", "logs", nil, func(_ context.Context, _ string, log io.Writer) error {
			_, err := fmt.Fprint(log, strings.Repeat("x", 256))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		job = waitManagerJob(t, manager, job.ID)
		logs, _ := manager.Logs(job.ID)
		if !job.LogTruncated || len(logs) > 64 || !strings.Contains(logs, "truncated") {
			t.Fatalf("job = %#v, logs=%q", job, logs)
		}
	})
}

func TestJobManagerMarksRunningJobInterruptedAfterRestart(t *testing.T) {
	stateDir := t.TempDir()
	dir := filepath.Join(stateDir, "stale")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	jobJSON := `{"id":"stale","kind":"install","cluster":"demo","status":"running","createdAt":"2026-11-15T00:00:00Z","logBytes":0}`
	if err := os.WriteFile(filepath.Join(dir, "job.json"), []byte(jobJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "job.log"), []byte("partial output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewJobManager(JobManagerOptions{StateDir: stateDir, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	job, ok := manager.Get("stale")
	if !ok || job.Status != JobInterrupted || !strings.Contains(job.Error, "restarted") {
		t.Fatalf("job = %#v, ok=%v", job, ok)
	}
	logs, _ := manager.Logs("stale")
	if !strings.Contains(logs, "partial output") {
		t.Fatalf("logs = %q", logs)
	}
}
