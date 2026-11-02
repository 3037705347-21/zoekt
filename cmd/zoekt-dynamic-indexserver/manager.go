// Copyright 2026 Sourcegraph, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// buildFunc runs the clone/fetch/index steps for one attempt into the private
// stagingDir. It is a variable in production and stubbed out in tests.
type buildFunc func(ctx context.Context, req indexRequest, stagingDir string) error

// Job outcome labels used in metrics.
const (
	outcomePublished  = "published"
	outcomeReused     = "reused"
	outcomeFailed     = "failed"
	outcomeSuperseded = "superseded"
)

var errTruncated = errors.New("index storage was truncated while the request was queued")

// submitResult is delivered to all HTTP requests waiting on a repository.
type submitResult struct {
	response map[string]any
	err      error
}

// persistedState is the durable state of one repository. It is rewritten
// atomically whenever a new request is enqueued or a generation is published.
type persistedState struct {
	RepoID uint32 `json:"repo_id"`

	// Generation is the monotonically increasing number of the newest enqueued
	// request for this repository.
	Generation int64 `json:"generation"`
	// Desired is the newest request (at Generation).
	Desired *indexRequest `json:"desired,omitempty"`

	// PublishedGeneration is the generation whose shards are currently live.
	// PublishedVersion records the client supplied version of that build.
	PublishedGeneration int64  `json:"published_generation"`
	PublishedVersion    string `json:"published_version,omitempty"`

	UpdatedAt time.Time `json:"updated_at"`
}

// repoQueue serializes indexing attempts for one repository and makes all
// concurrent requests converge on the newest generation.
type repoQueue struct {
	id  uint32
	mgr *manager

	mu    sync.Mutex
	state persistedState

	// running reports whether a worker goroutine is active.
	running bool
	// cancel interrupts the currently running attempt when a newer
	// generation arrives.
	cancel context.CancelFunc

	// waiters are all HTTP requests waiting for the repository to converge.
	waiters []chan submitResult
}

// manager owns the per-repository queues and their durable state.
type manager struct {
	opts    Options
	build   buildFunc
	rootCtx context.Context

	// observe, when set, records a job outcome for metrics.
	observe func(outcome string)

	// runMu serializes attempts (read lock) against truncate (write lock).
	runMu  sync.RWMutex
	mu     sync.Mutex
	queues map[uint32]*repoQueue
}

func newManager(opts Options, build buildFunc, rootCtx context.Context) *manager {
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	return &manager{
		opts:    opts,
		build:   build,
		rootCtx: rootCtx,
		queues:  map[uint32]*repoQueue{},
	}
}

func (m *manager) recordOutcome(outcome string) {
	if m.observe != nil {
		m.observe(outcome)
	}
}

func (m *manager) getOrCreateQueue(repoID uint32) *repoQueue {
	m.mu.Lock()
	defer m.mu.Unlock()
	q, ok := m.queues[repoID]
	if !ok {
		q = &repoQueue{
			id:  repoID,
			mgr: m,
			state: persistedState{
				RepoID: repoID,
			},
		}
		m.queues[repoID] = q
	}
	return q
}

// Submit enqueues an index request and waits for the repository to converge.
//
// Requests carrying the same non-empty Version as the newest enqueued request
// are duplicates: they reuse the in-flight attempt, or the already published
// result. Every other request starts a new generation; attempts of older
// generations that are still building get interrupted and their output is
// discarded, so an old request can never overwrite a newer one.
func (m *manager) Submit(ctx context.Context, req indexRequest) (map[string]any, error) {
	q := m.getOrCreateQueue(req.RepoID)

	q.mu.Lock()
	duplicate := req.Version != "" &&
		q.state.Desired != nil &&
		req.Version == q.state.Desired.Version &&
		req.CloneURL == q.state.Desired.CloneURL

	if duplicate &&
		q.state.Generation == q.state.PublishedGeneration &&
		q.state.PublishedVersion == req.Version {
		generation := q.state.PublishedGeneration
		q.mu.Unlock()
		m.recordOutcome(outcomeReused)
		return indexResponse(req, generation, true), nil
	}

	if !duplicate {
		q.state.RepoID = req.RepoID
		q.state.Generation++
		q.state.Desired = &req
		q.state.UpdatedAt = time.Now()
		if err := m.saveState(q.state); err != nil {
			log.Printf("dynamic-indexserver: persist state for repo %d failed: %v", req.RepoID, err)
		}
		// A newer generation makes any running attempt obsolete. Interrupt it
		// so the newest request starts as soon as possible.
		if q.cancel != nil {
			q.cancel()
		}
	}

	ch := make(chan submitResult, 1)
	q.waiters = append(q.waiters, ch)
	if !q.running {
		q.running = true
		go q.run()
	}
	q.mu.Unlock()

	select {
	case result := <-ch:
		return result.response, result.err
	case <-ctx.Done():
		// The client went away (e.g. timed out and retried), but the attempt
		// keeps running server side so the retry can join it.
		return nil, ctx.Err()
	case <-m.rootCtx.Done():
		return nil, fmt.Errorf("indexserver is shutting down")
	}
}

// run is the per-repository worker. It keeps building the newest request until
// the repository has converged.
func (q *repoQueue) run() {
	for {
		if q.mgr.rootCtx.Err() != nil {
			q.mu.Lock()
			q.running = false
			q.cancel = nil
			q.mu.Unlock()
			return
		}

		// Acquired per iteration so that truncate can wait for the active
		// attempt to finish before wiping the storage directories.
		q.mgr.runMu.RLock()
		q.mu.Lock()
		if q.state.Desired == nil || q.state.Generation == q.state.PublishedGeneration {
			q.running = false
			q.cancel = nil
			q.mu.Unlock()
			q.mgr.runMu.RUnlock()
			return
		}
		gen := q.state.Generation
		req := *q.state.Desired
		q.mu.Unlock()

		published, err := q.attempt(gen, req)

		q.mu.Lock()
		if q.mgr.rootCtx.Err() != nil {
			// Shutting down: durable state allows resuming on the next start.
			q.running = false
			q.cancel = nil
			q.mu.Unlock()
			q.mgr.runMu.RUnlock()
			return
		}

		if published {
			q.state.PublishedGeneration = gen
			q.state.PublishedVersion = req.Version
			q.state.UpdatedAt = time.Now()
			if err := q.mgr.saveState(q.state); err != nil {
				log.Printf("dynamic-indexserver: persist state for repo %d failed: %v", q.id, err)
			}

			if q.state.Generation == gen {
				q.broadcast(submitResult{response: indexResponse(req, gen, false)})
				q.running = false
				q.cancel = nil
				q.mu.Unlock()
				q.mgr.runMu.RUnlock()
				return
			}

			// A newer generation was requested while this one was building.
			// Loop and build it; the current waiters keep waiting.
			q.mu.Unlock()
			q.mgr.runMu.RUnlock()
			continue
		}

		if q.state.Generation != gen {
			// The failed attempt belonged to an obsolete generation; try the
			// newest request immediately without disturbing the waiters.
			q.mu.Unlock()
			q.mgr.runMu.RUnlock()
			continue
		}

		q.broadcast(submitResult{err: err})
		q.running = false
		q.cancel = nil
		q.mu.Unlock()
		q.mgr.runMu.RUnlock()
		return
	}
}

// attempt builds and publishes exactly one generation.
func (q *repoQueue) attempt(gen int64, req indexRequest) (published bool, err error) {
	indexDir := q.mgr.opts.indexDir

	ctx, cancel := context.WithTimeout(q.mgr.rootCtx, q.mgr.opts.indexTimeout)
	defer cancel()

	// Register the attempt and re-check liveness under the same lock used by
	// Submit, so a generation bump can never be missed.
	q.mu.Lock()
	if q.state.Generation != gen {
		q.mu.Unlock()
		q.mgr.recordOutcome(outcomeSuperseded)
		return false, nil
	}
	q.cancel = cancel
	q.mu.Unlock()

	staging, err := newStagingDir(indexDir, q.id, gen)
	if err != nil {
		return false, fmt.Errorf("create staging directory: %w", err)
	}

	buildErr := q.mgr.build(ctx, req, staging)

	q.mu.Lock()
	currentGen := q.state.Generation
	q.cancel = nil
	q.mu.Unlock()

	if buildErr != nil {
		_ = os.RemoveAll(staging)
		if currentGen != gen {
			q.mgr.recordOutcome(outcomeSuperseded)
			return false, nil
		}
		q.mgr.recordOutcome(outcomeFailed)
		return false, fmt.Errorf("index repository %d: %w", q.id, buildErr)
	}

	if currentGen != gen {
		// A newer request exists. Its output must never reach the live
		// directory, so discard the whole staging directory.
		_ = os.RemoveAll(staging)
		q.mgr.recordOutcome(outcomeSuperseded)
		return false, nil
	}

	if err := publishNewIndex(indexDir, q.id, gen, staging); err != nil {
		q.mgr.recordOutcome(outcomeFailed)
		return false, fmt.Errorf("publish index for repository %d: %w", q.id, err)
	}

	q.mgr.recordOutcome(outcomePublished)
	return true, nil
}

func (q *repoQueue) broadcast(result submitResult) {
	for _, ch := range q.waiters {
		select {
		case ch <- result:
		default:
		}
	}
	q.waiters = nil
}

// Recover reconciles interrupted publishes and resumes requests that had not
// been published yet when the process last stopped.
func (m *manager) Recover() error {
	if err := recoverInterruptedPublishes(m.opts.indexDir); err != nil {
		return fmt.Errorf("recover interrupted publishes: %w", err)
	}

	states, err := loadStates(m.opts.indexDir)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, state := range states {
		if state.Desired == nil {
			continue
		}
		q := &repoQueue{
			id:    state.RepoID,
			mgr:   m,
			state: state,
		}
		m.queues[state.RepoID] = q
		if state.Generation > state.PublishedGeneration {
			log.Printf("dynamic-indexserver: resuming pending request for repo %d (generation %d)",
				state.RepoID, state.Generation)
			q.running = true
			go q.run()
		}
	}
	return nil
}

// Truncate waits for running attempts, empties the storage directories and
// forgets all queued requests. It never runs concurrently with an attempt.
func (m *manager) Truncate(ctx context.Context) error {
	locked := make(chan struct{})
	go func() {
		m.runMu.Lock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer m.runMu.Unlock()

	m.mu.Lock()
	queues := m.queues
	m.queues = map[uint32]*repoQueue{}
	m.mu.Unlock()

	for _, q := range queues {
		q.mu.Lock()
		q.state = persistedState{RepoID: q.id}
		q.running = false
		q.cancel = nil
		q.broadcast(submitResult{err: errTruncated})
		q.mu.Unlock()
	}

	if err := emptyDirectory(m.opts.repoDir); err != nil {
		return fmt.Errorf("empty repoDir %s: %w", m.opts.repoDir, err)
	}
	if err := emptyDirectory(m.opts.indexDir); err != nil {
		return fmt.Errorf("empty indexDir %s: %w", m.opts.indexDir, err)
	}
	return nil
}

func indexResponse(req indexRequest, generation int64, reused bool) map[string]any {
	return map[string]any{
		"Success":    true,
		"Version":    req.Version,
		"Generation": generation,
		"Reused":     reused,
	}
}

// --- durable per-repository state ---

func stateFilePath(indexDir string, repoID uint32) string {
	return filepath.Join(indexDir, stateDirName, fmt.Sprintf("%d.json", repoID))
}

func (m *manager) saveState(state persistedState) error {
	return writeJSONAtomic(stateFilePath(m.opts.indexDir, state.RepoID), state)
}

func loadStates(indexDir string) ([]persistedState, error) {
	dir := filepath.Join(indexDir, stateDirName)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state directory: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	var states []persistedState
	for _, name := range names {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read state file %s: %w", path, err)
		}
		var state persistedState
		if err := json.Unmarshal(data, &state); err != nil {
			// A corrupt record must not block startup: the live shards are
			// untouched and the next request rebuilds the repository.
			log.Printf("dynamic-indexserver: ignoring unreadable state file %s: %v", path, err)
			continue
		}
		states = append(states, state)
	}
	return states, nil
}
