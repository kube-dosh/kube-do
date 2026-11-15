package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

var ErrClusterBusy = errors.New("cluster already has a running job")

const maxJobLogBytes = 2 << 20

type JobManager struct {
	mu     sync.RWMutex
	jobs   map[string]*jobRecord
	active map[string]string
	base   context.Context
}

type jobRecord struct {
	mu  sync.RWMutex
	job Job
	log []byte
}

func NewJobManager() *JobManager {
	return &JobManager{
		jobs:   map[string]*jobRecord{},
		active: map[string]string{},
		base:   context.Background(),
	}
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
	}}
	m.jobs[id] = rec
	m.active[clusterName] = id
	base := m.base
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(base)
	go func() {
		defer cancel()
		now := time.Now().UTC()
		rec.mu.Lock()
		rec.job.Status = JobRunning
		rec.job.StartedAt = &now
		rec.mu.Unlock()

		err := run(ctx, id, jobLogWriter{record: rec})

		finished := time.Now().UTC()
		rec.mu.Lock()
		rec.job.FinishedAt = &finished
		if err != nil {
			rec.job.Status = JobFailed
			rec.job.Error = err.Error()
		} else {
			rec.job.Status = JobSucceeded
		}
		rec.mu.Unlock()

		m.mu.Lock()
		delete(m.active, clusterName)
		m.mu.Unlock()
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

func (r *jobRecord) snapshot() Job {
	r.mu.RLock()
	defer r.mu.RUnlock()
	job := r.job
	job.NewWorkers = append([]Node(nil), r.job.NewWorkers...)
	job.LogBytes = len(r.log)
	return job
}

func (r *jobRecord) appendLog(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log = append(r.log, p...)
	if len(r.log) > maxJobLogBytes {
		trim := len(r.log) - maxJobLogBytes
		copy(r.log, r.log[trim:])
		r.log = r.log[:maxJobLogBytes]
	}
}

type jobLogWriter struct {
	record *jobRecord
}

func (w jobLogWriter) Write(p []byte) (int, error) {
	w.record.appendLog(p)
	return len(p), nil
}

func newJobID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("job-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
