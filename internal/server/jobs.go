package server

// Progress for work that outlives its request.
//
// Creating an app clones a repository, pulls images, and runs composer — minutes,
// not milliseconds. The add-app dialog used to hold a spinner for all of it and
// then show either a redirect or a wall of error text, which is the worst
// possible shape: no idea whether anything is happening, and no idea what failed
// when it did.
//
// So the dialog gets a job id immediately and polls it. Jobs live in memory
// only: they describe work in progress, they are worthless after a restart (the
// work died with the process), and writing them to SQLite would mean another
// table to prune for something nobody reads twice. The lasting record is the
// `deployments` row the create writes anyway.

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"xdev/internal/store"
)

// jobTTL is how long a finished job stays readable. Long enough for a slow poll
// or a page the user left open; short enough that nothing accumulates.
const jobTTL = 10 * time.Minute

// job is one long-running operation and everything a client needs to render it.
type job struct {
	mu       sync.Mutex
	steps    []store.DeployStep
	done     bool
	err      string
	redirect string
	touched  time.Time
}

func (j *job) step(name, output string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	// A step reported twice is the same step finishing: the first call announces
	// it (empty output), the second carries what it printed. Update in place, or
	// the list grows a duplicate for every long step.
	for i := range j.steps {
		if j.steps[i].Name == name && j.steps[i].Output == "" {
			j.steps[i].Output = output
			j.touched = time.Now()
			return
		}
	}
	j.steps = append(j.steps, store.DeployStep{Name: name, Output: output})
	j.touched = time.Now()
}

// finish closes a job. errMsg empty means it worked.
func (j *job) finish(errMsg, redirect string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.done, j.err, j.redirect, j.touched = true, errMsg, redirect, time.Now()
	// Whatever was in flight when it failed is the step that failed.
	if errMsg != "" && len(j.steps) > 0 {
		j.steps[len(j.steps)-1].Failed = true
	}
}

func (j *job) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	steps := make([]map[string]any, 0, len(j.steps))
	for _, s := range j.steps {
		steps = append(steps, map[string]any{
			"name": s.Name, "output": s.Output, "failed": s.Failed,
		})
	}
	return map[string]any{
		"steps": steps, "done": j.done, "error": j.err, "redirect": j.redirect,
	}
}

// jobs is the in-memory registry.
type jobs struct {
	mu sync.Mutex
	m  map[string]*job
}

func (js *jobs) start() (string, *job) {
	buf := make([]byte, 16)
	rand.Read(buf)
	id := hex.EncodeToString(buf)
	j := &job{touched: time.Now()}
	js.mu.Lock()
	defer js.mu.Unlock()
	if js.m == nil {
		js.m = map[string]*job{}
	}
	// Sweep on write rather than with a goroutine: jobs only appear here, so
	// this is the only moment the map can grow.
	for k, old := range js.m {
		old.mu.Lock()
		stale := old.done && time.Since(old.touched) > jobTTL
		old.mu.Unlock()
		if stale {
			delete(js.m, k)
		}
	}
	js.m[id] = j
	return id, j
}

func (js *jobs) get(id string) *job {
	js.mu.Lock()
	defer js.mu.Unlock()
	return js.m[id]
}

// handleJob reports a job's progress. Unknown ids 404 — a job that has expired
// is indistinguishable from one that never existed, and both mean "stop polling".
func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	j := s.jobs.get(r.PathValue("id"))
	if j == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, j.snapshot())
}
