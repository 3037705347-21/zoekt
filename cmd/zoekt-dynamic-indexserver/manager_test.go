package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type buildGate struct {
	ch        chan struct{}
	closeOnce sync.Once
}

func (g *buildGate) release() {
	g.closeOnce.Do(func() { close(g.ch) })
}

type fakeBuilder struct {
	mu        sync.Mutex
	count     int
	started   []string
	gates     map[string]*buildGate
	fail      map[string]bool
	gated     bool
	ignoreCtx bool
}

func newFakeBuilder(gated bool) *fakeBuilder {
	return &fakeBuilder{
		gates: map[string]*buildGate{},
		fail:  map[string]bool{},
		gated: gated,
	}
}

func (b *fakeBuilder) build(ctx context.Context, req indexRequest, staging string) error {
	b.mu.Lock()
	b.count++
	b.started = append(b.started, req.Version)
	key := fmt.Sprintf("%s#%d", req.Version, b.count)
	gate := &buildGate{ch: make(chan struct{})}
	b.gates[key] = gate
	fail := b.fail[req.Version]
	b.mu.Unlock()

	if b.gated {
		if b.ignoreCtx {
			<-gate.ch
		} else {
			select {
			case <-gate.ch:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	if fail {
		return fmt.Errorf("build failure for version %q", req.Version)
	}

	// Pretend the indexer produced exactly one shard, whose content records
	// the request version. This lets tests assert which generation is live.
	return os.WriteFile(filepath.Join(staging, shardName(req.RepoID, 0)),
		[]byte(req.Version), 0o644)
}

func (b *fakeBuilder) buildCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.count
}

func (b *fakeBuilder) startedVersions() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.started))
	copy(out, b.started)
	return out
}

func (b *fakeBuilder) gateForNth(version string, n int) *buildGate {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.gates[fmt.Sprintf("%s#%d", version, n)]
}

func (b *fakeBuilder) releaseAll() {
	b.mu.Lock()
	gates := make([]*buildGate, 0, len(b.gates))
	for key, gate := range b.gates {
		gates = append(gates, gate)
		delete(b.gates, key)
	}
	b.mu.Unlock()
	for _, gate := range gates {
		gate.release()
	}
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

type asyncResult struct {
	response map[string]any
	err      error
}

type asyncSubmit struct {
	ch chan asyncResult
}

func submitAsync(m *manager, req indexRequest) asyncSubmit {
	ch := make(chan asyncResult, 1)
	go func() {
		resp, err := m.Submit(context.Background(), req)
		ch <- asyncResult{response: resp, err: err}
	}()
	return asyncSubmit{ch: ch}
}

func (s asyncSubmit) wait(t *testing.T) asyncResult {
	t.Helper()
	select {
	case got := <-s.ch:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("submit did not return")
		return asyncResult{}
	}
}

func testManagerOptions(t *testing.T) Options {
	t.Helper()
	root := t.TempDir()
	opts := Options{
		repoDir:      filepath.Join(root, "repo"),
		indexDir:     filepath.Join(root, "index"),
		indexTimeout: time.Minute,
	}
	if err := os.MkdirAll(opts.repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(opts.indexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return opts
}

func liveShard(t *testing.T, opts Options, repoID uint32) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(opts.indexDir, shardName(repoID, 0)))
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data), true
}

func loadPersistedState(t *testing.T, opts Options, repoID uint32) persistedState {
	t.Helper()
	data, err := os.ReadFile(stateFilePath(opts.indexDir, repoID))
	if err != nil {
		t.Fatal(err)
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func req(version string, repoID uint32) indexRequest {
	return indexRequest{
		CloneURL: "https://example.com/repository.git",
		RepoID:   repoID,
		Version:  version,
	}
}

func TestSubmitConvergesToNewestGeneration(t *testing.T) {
	opts := testManagerOptions(t)
	builder := newFakeBuilder(true)
	t.Cleanup(builder.releaseAll)
	m := newManager(opts, builder.build, context.Background())

	first := submitAsync(m, req("v1", 100))
	waitUntil(t, time.Second, func() bool { return builder.buildCount() == 1 })

	// A newer version arrives while v1 is still indexing.
	second := submitAsync(m, req("v2", 100))

	waitUntil(t, time.Second, func() bool { return builder.buildCount() == 2 })
	builder.gateForNth("v1", 1).release()
	builder.gateForNth("v2", 2).release()

	r1 := first.wait(t)
	r2 := second.wait(t)
	if r1.err != nil || r2.err != nil {
		t.Fatalf("expected both requests to converge, got %v / %v", r1.err, r2.err)
	}

	content, ok := liveShard(t, opts, 100)
	if !ok || content != "v2" {
		t.Fatalf("live index = %q (exists=%v), want v2", content, ok)
	}

	state := loadPersistedState(t, opts, 100)
	if state.PublishedGeneration != 2 || state.PublishedVersion != "v2" {
		t.Fatalf("unexpected persisted state: %+v", state)
	}
	if got := r1.response["Generation"]; got != int64(2) {
		t.Fatalf("old request should receive converged generation 2, got %v", got)
	}
}

// TestObsoleteGenerationCannotPublish is the core guarantee: even if the old
// build ignores cancellation and finishes successfully after a newer request
// arrived, its output must never be published.
func TestObsoleteGenerationCannotPublish(t *testing.T) {
	opts := testManagerOptions(t)
	builder := newFakeBuilder(true)
	builder.ignoreCtx = true
	t.Cleanup(builder.releaseAll)
	m := newManager(opts, builder.build, context.Background())

	first := submitAsync(m, req("v1", 100))
	waitUntil(t, time.Second, func() bool { return builder.buildCount() == 1 })

	second := submitAsync(m, req("v2", 100))
	// Let the old build finish *after* v2 was requested. It must be discarded;
	// only then may the v2 build run and be released.
	builder.gateForNth("v1", 1).release()
	waitUntil(t, time.Second, func() bool { return builder.buildCount() == 2 })
	builder.gateForNth("v2", 2).release()

	if r := first.wait(t); r.err != nil {
		t.Fatalf("old request should converge, got %v", r.err)
	}
	if r := second.wait(t); r.err != nil {
		t.Fatalf("new request should succeed, got %v", r.err)
	}

	content, ok := liveShard(t, opts, 100)
	if !ok || content != "v2" {
		t.Fatalf("old generation overwrote the newer one: live = %q (exists=%v)", content, ok)
	}
}

func TestSubmitDeduplicatesInFlightRequest(t *testing.T) {
	opts := testManagerOptions(t)
	builder := newFakeBuilder(true)
	t.Cleanup(builder.releaseAll)
	m := newManager(opts, builder.build, context.Background())

	first := submitAsync(m, req("v1", 100))
	second := submitAsync(m, req("v1", 100))

	q := m.getOrCreateQueue(100)
	waitUntil(t, time.Second, func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return builder.buildCount() == 1 && len(q.waiters) == 2
	})

	builder.gateForNth("v1", 1).release()

	r1 := first.wait(t)
	r2 := second.wait(t)
	if r1.err != nil || r2.err != nil {
		t.Fatalf("duplicates should share one result, got %v / %v", r1.err, r2.err)
	}
	if builder.buildCount() != 1 {
		t.Fatalf("expected exactly one build, got %d", builder.buildCount())
	}
}

func TestSubmitReusesPublishedResult(t *testing.T) {
	opts := testManagerOptions(t)
	builder := newFakeBuilder(false) // builds complete immediately
	m := newManager(opts, builder.build, context.Background())

	resp, err := m.Submit(context.Background(), req("v1", 100))
	if err != nil {
		t.Fatal(err)
	}
	if reused, _ := resp["Reused"].(bool); reused {
		t.Fatal("first request should not be a reuse")
	}

	// CI retries after a timeout, or sends the same version again.
	resp, err = m.Submit(context.Background(), req("v1", 100))
	if err != nil {
		t.Fatal(err)
	}
	if reused, _ := resp["Reused"].(bool); !reused {
		t.Fatalf("identical published version should be reused, got %+v", resp)
	}
	if builder.buildCount() != 1 {
		t.Fatalf("expected no rebuild, got %d", builder.buildCount())
	}
}

func TestVersionlessRequestsAlwaysStartNewGeneration(t *testing.T) {
	opts := testManagerOptions(t)
	builder := newFakeBuilder(false)
	m := newManager(opts, builder.build, context.Background())

	for range 2 {
		if _, err := m.Submit(context.Background(), req("", 100)); err != nil {
			t.Fatal(err)
		}
	}
	if builder.buildCount() != 2 {
		t.Fatalf("versionless identical requests must both run, got %d builds", builder.buildCount())
	}
}

func TestFailedBuildKeepsPreviousIndex(t *testing.T) {
	opts := testManagerOptions(t)
	builder := newFakeBuilder(false)
	builder.fail["v2"] = true
	m := newManager(opts, builder.build, context.Background())

	if _, err := m.Submit(context.Background(), req("v1", 100)); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Submit(context.Background(), req("v2", 100)); err == nil {
		t.Fatal("expected v2 build failure")
	}

	content, ok := liveShard(t, opts, 100)
	if !ok || content != "v1" {
		t.Fatalf("previous searchable index must survive failure, got %q (exists=%v)", content, ok)
	}
	state := loadPersistedState(t, opts, 100)
	if state.PublishedGeneration != 1 {
		t.Fatalf("published generation must stay 1, got %d", state.PublishedGeneration)
	}

	// Fixed version is retried and converges.
	builder.fail["v2"] = false
	if _, err := m.Submit(context.Background(), req("v2", 100)); err != nil {
		t.Fatalf("retry should succeed, got %v", err)
	}
	content, _ = liveShard(t, opts, 100)
	if content != "v2" {
		t.Fatalf("live index = %q, want v2", content)
	}
}

func TestFailureDoesNotAffectOtherRepositories(t *testing.T) {
	opts := testManagerOptions(t)
	builder := newFakeBuilder(false)
	builder.fail["bad"] = true
	m := newManager(opts, builder.build, context.Background())

	bad := submitAsync(m, req("bad", 100))
	good := submitAsync(m, req("v1", 200))

	if r := bad.wait(t); r.err == nil {
		t.Fatal("expected failure for repo 100")
	}
	if r := good.wait(t); r.err != nil {
		t.Fatalf("repo 200 must be unaffected, got %v", r.err)
	}
	if content, ok := liveShard(t, opts, 200); !ok || content != "v1" {
		t.Fatalf("repo 200 index missing or wrong: %q (exists=%v)", content, ok)
	}
	if _, ok := liveShard(t, opts, 100); ok {
		t.Fatal("repo 100 should have no live index")
	}
}

func TestRecoverResumesPendingRequest(t *testing.T) {
	opts := testManagerOptions(t)

	ctx1, cancel1 := context.WithCancel(context.Background())
	builder1 := newFakeBuilder(true)
	m1 := newManager(opts, builder1.build, ctx1)

	pending := submitAsync(m1, req("v1", 100))
	waitUntil(t, time.Second, func() bool { return builder1.buildCount() == 1 })

	// Process dies (SIGTERM) mid-build.
	cancel1()
	if r := pending.wait(t); r.err == nil {
		t.Fatal("interrupted submit should report shutdown/context error")
	}

	// Restart: the newest request is durable and runs again automatically.
	builder2 := newFakeBuilder(true)
	t.Cleanup(builder2.releaseAll)
	m2 := newManager(opts, builder2.build, context.Background())
	if err := m2.Recover(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, time.Second, func() bool { return builder2.buildCount() == 1 })
	builder2.gateForNth("v1", 1).release()

	waitUntil(t, time.Second, func() bool {
		content, ok := liveShard(t, opts, 100)
		return ok && content == "v1"
	})

	// Wait until the worker has also recorded convergence, not just the file.
	q2 := m2.getOrCreateQueue(100)
	waitUntil(t, time.Second, func() bool {
		q2.mu.Lock()
		defer q2.mu.Unlock()
		return q2.state.PublishedGeneration == 1
	})

	// Re-sending the current request observes the converged result.
	resp, err := m2.Submit(context.Background(), req("v1", 100))
	if err != nil {
		t.Fatal(err)
	}
	if reused, _ := resp["Reused"].(bool); !reused {
		t.Fatalf("resumed version should be reported as reused, got %+v", resp)
	}
	assertHiddenDirsEmpty(t, opts.indexDir)
}

func TestTruncateWipesStorageAndState(t *testing.T) {
	opts := testManagerOptions(t)
	builder := newFakeBuilder(false)
	m := newManager(opts, builder.build, context.Background())

	if _, err := m.Submit(context.Background(), req("v1", 100)); err != nil {
		t.Fatal(err)
	}

	if err := m.Truncate(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{opts.indexDir, opts.repoDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("directory %s should be empty after truncate, contains %v", dir, entries)
		}
	}

	// Fresh requests work after truncation.
	if _, err := m.Submit(context.Background(), req("v2", 100)); err != nil {
		t.Fatal(err)
	}
	if content, ok := liveShard(t, opts, 100); !ok || content != "v2" {
		t.Fatalf("live index after truncate = %q (exists=%v), want v2", content, ok)
	}
}
