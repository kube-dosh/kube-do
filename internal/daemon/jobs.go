package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/kube-dosh/kube-do/internal/atomicfile"
)

var (
	ErrClusterBusy = errors.New("cluster already has a running job")
	ErrJobNotFound = errors.New("job not found")
	ErrJobFinished = errors.New("job is already finished")
)

const (
	DefaultJobTimeout     = 2 * time.Hour
	DefaultMaxJobLogBytes = 8 << 20
	jobPersistInterval    = time.Second
	truncationMarker      = "\n[... earlier log output truncated ...]\n"
)

type JobManagerOptions struct {
	StateDir    string
	Timeout     time.Duration
	MaxLogBytes int
}

type JobManager struct {
	mu        sync.RWMutex
	jobs      map[string]*jobRecord
	active    map[string]string
	base      context.Context
	stateDir  string
	timeout   time.Duration
	maxLog    int
	persistMu sync.Mutex
}

type jobRecord struct {
	mu              sync.RWMutex
	job             Job
	log             []byte
	cancel          context.CancelFunc
	cancelRequested bool
}

func NewJobManager(opts JobManagerOptions) (*JobManager, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultJobTimeout
	}
	if opts.MaxLogBytes <= 0 {
		opts.MaxLogBytes = DefaultMaxJobLogBytes
	}
	if opts.StateDir != "" {
		if err := os.MkdirAll(opts.StateDir, 0o700); err != nil {
			return nil, fmt.Errorf("create job state directory: %w", err)
		}
	}
	m := &JobManager{
		jobs:     map[string]*jobRecord{},
		active:   map[string]string{},
		base:     context.Background(),
		stateDir: opts.StateDir,
		timeout:  opts.Timeout,
		maxLog:   opts.MaxLogBytes,
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *JobManager) SetBaseContext(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	m.base = ctx
	m.mu.Unlock()
}

func (m *JobManager) Start(kind, clusterName string, nodes []Node, run func(context.Context, string, io.Writer) error) (Job, error) {
	m.mu.Lock()
	if existing, ok := m.active[clusterName]; ok {
		m.mu.Unlock()
		return Job{}, fmt.Errorf("%w: %s", ErrClusterBusy, existing)
	}
	id := newJobID()
	for m.jobs[id] != nil {
		id = newJobID()
	}
	rec := &jobRecord{job: Job{
		ID:         id,
		Kind:       kind,
		Cluster:    clusterName,
		Status:     JobQueued,
		CreatedAt:  time.Now().UTC(),
		NewWorkers: append([]Node(nil), nodes...),
		Nodes:      append([]Node(nil), nodes...),
	}}
	m.jobs[id] = rec
	m.active[clusterName] = id
	base := m.base
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(base, m.timeout)
	rec.mu.Lock()
	rec.cancel = cancel
	rec.mu.Unlock()
	if err := m.persistRecord(rec); err != nil {
		cancel()
		m.mu.Lock()
		delete(m.jobs, id)
		delete(m.active, clusterName)
		m.mu.Unlock()
		return Job{}, err
	}

	go func() {
		defer cancel()
		now := time.Now().UTC()
		rec.mu.Lock()
		rec.job.Status = JobRunning
		rec.job.StartedAt = &now
		rec.mu.Unlock()
		_ = m.persistRecord(rec)

		writer := &jobLogWriter{manager: m, record: rec, maxBytes: m.maxLog}
		err := run(ctx, id, writer)

		finished := time.Now().UTC()
		rec.mu.Lock()
		rec.job.FinishedAt = &finished
		cancelRequested := rec.cancelRequested
		ctxErr := ctx.Err()
		switch {
		case err == nil && ctxErr == nil && !cancelRequested:
			rec.job.Status = JobSucceeded
			rec.job.Error = ""
		case errors.Is(ctxErr, context.DeadlineExceeded):
			rec.job.Status = JobFailed
			rec.job.Error = fmt.Sprintf("job timed out after %s", m.timeout)
		case cancelRequested || errors.Is(ctxErr, context.Canceled):
			if cancelRequested {
				rec.job.Status = JobCancelled
				rec.job.Error = ""
			} else {
				rec.job.Status = JobInterrupted
				rec.job.Error = "daemon stopped while the job was running"
			}
		default:
			rec.job.Status = JobFailed
			if err == nil {
				rec.job.Error = "job failed without an error"
			} else {
				rec.job.Error = err.Error()
			}
		}
		rec.cancel = nil
		rec.mu.Unlock()

		m.mu.Lock()
		delete(m.active, clusterName)
		m.mu.Unlock()
		_ = m.persistRecord(rec)
	}()
	return rec.snapshot(), nil
}

func (m *JobManager) Get(id string) (Job, bool) {
	m.mu.RLock()
	rec := m.jobs[id]
	m.mu.RUnlock()
	if rec == nil {
		return Job{}, false
	}
	return rec.snapshot(), true
}

func (m *JobManager) SetResult(id string, value any) {
	m.mu.RLock()
	rec := m.jobs[id]
	m.mu.RUnlock()
	if rec == nil {
		return
	}
	rec.mu.Lock()
	rec.job.Result = value
	rec.mu.Unlock()
	_ = m.persistRecord(rec)
}

func (m *JobManager) Cancel(id string) (Job, error) {
	m.mu.RLock()
	rec := m.jobs[id]
	m.mu.RUnlock()
	if rec == nil {
		return Job{}, ErrJobNotFound
	}
	rec.mu.Lock()
	switch rec.job.Status {
	case JobSucceeded, JobFailed, JobCancelled, JobInterrupted:
		rec.mu.Unlock()
		return Job{}, ErrJobFinished
	}
	rec.cancelRequested = true
	cancel := rec.cancel
	rec.mu.Unlock()
	if cancel == nil {
		return Job{}, errors.New("job has no active cancellation function")
	}
	cancel()
	return rec.snapshot(), nil
}

func (m *JobManager) Logs(id string) (string, bool) {
	m.mu.RLock()
	rec := m.jobs[id]
	m.mu.RUnlock()
	if rec == nil {
		return "", false
	}
	rec.mu.RLock()
	defer rec.mu.RUnlock()
	return string(rec.log), true
}

func (m *JobManager) List(clusterName string) []Job {
	m.mu.RLock()
	records := make([]*jobRecord, 0, len(m.jobs))
	for _, rec := range m.jobs {
		records = append(records, rec)
	}
	m.mu.RUnlock()

	out := make([]Job, 0, len(records))
	for _, rec := range records {
		job := rec.snapshot()
		if clusterName == "" || job.Cluster == clusterName {
			out = append(out, job)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

func (m *JobManager) load() error {
	if m.stateDir == "" {
		return nil
	}
	entries, err := os.ReadDir(m.stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read job state directory: %w", err)
	}
	now := time.Now().UTC()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(m.stateDir, entry.Name())
		data, err := os.ReadFile(filepath.Join(dir, "job.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read job metadata %s: %w", entry.Name(), err)
		}
		var job Job
		if err := json.Unmarshal(data, &job); err != nil {
			return fmt.Errorf("decode job metadata %s: %w", entry.Name(), err)
		}
		if job.ID == "" {
			job.ID = entry.Name()
		}
		logs, err := os.ReadFile(filepath.Join(dir, "job.log"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read job log %s: %w", entry.Name(), err)
		}
		cancelRequested := false
		if job.Status == JobQueued || job.Status == JobRunning {
			job.Status = JobInterrupted
			job.Error = "daemon restarted before the job completed"
			job.FinishedAt = &now
			cancelRequested = true
		}
		rec := &jobRecord{job: job, log: logs, cancelRequested: cancelRequested}
		m.jobs[job.ID] = rec
		_ = m.persistRecord(rec)
	}
	return nil
}

func (m *JobManager) persistRecord(rec *jobRecord) error {
	if m.stateDir == "" || rec == nil {
		return nil
	}
	rec.mu.RLock()
	job := rec.job
	job.NewWorkers = append([]Node(nil), rec.job.NewWorkers...)
	job.LogBytes = len(rec.log)
	logs := append([]byte(nil), rec.log...)
	rec.mu.RUnlock()
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return fmt.Errorf("encode job metadata: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Join(m.stateDir, job.ID)
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create job directory: %w", err)
	}
	if err := atomicfile.Write(filepath.Join(dir, "job.json"), data, 0o600); err != nil {
		return err
	}
	if err := atomicfile.Write(filepath.Join(dir, "job.log"), logs, 0o600); err != nil {
		return err
	}
	return nil
}

func (r *jobRecord) snapshot() Job {
	r.mu.RLock()
	defer r.mu.RUnlock()
	job := r.job
	job.NewWorkers = append([]Node(nil), r.job.NewWorkers...)
	job.LogBytes = len(r.log)
	return job
}

func (r *jobRecord) appendLog(p []byte, maxBytes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log = append(r.log, p...)
	if maxBytes <= 0 || len(r.log) <= maxBytes {
		return
	}
	r.job.LogTruncated = true
	marker := []byte(truncationMarker)
	if maxBytes <= len(marker) {
		r.log = append(r.log[:0], r.log[len(r.log)-maxBytes:]...)
		return
	}
	keep := maxBytes - len(marker)
	tail := append([]byte(nil), r.log[len(r.log)-keep:]...)
	r.log = append(append(r.log[:0], marker...), tail...)
}

type jobLogWriter struct {
	manager     *JobManager
	record      *jobRecord
	maxBytes    int
	lastPersist time.Time
}

func (w *jobLogWriter) Write(p []byte) (int, error) {
	w.record.appendLog(p, w.maxBytes)
	now := time.Now()
	if w.lastPersist.IsZero() || now.Sub(w.lastPersist) >= jobPersistInterval {
		_ = w.manager.persistRecord(w.record)
		w.lastPersist = now
	}
	return len(p), nil
}

func newJobID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("job-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
