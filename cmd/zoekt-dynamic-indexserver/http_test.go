package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func newTestHTTPServer(t *testing.T, builder *fakeBuilder, m *manager) *httptest.Server {
	t.Helper()
	server := &indexServer{opts: m.opts, mgr: m}
	server.initMetrics()
	mux := http.NewServeMux()
	mux.HandleFunc("/index", server.serveIndex)
	mux.HandleFunc("/truncate", server.serveTruncate)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func postIndex(t *testing.T, url string, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url+"/index", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if len(data) > 0 {
		_ = json.Unmarshal(data, &decoded)
	}
	return resp.StatusCode, decoded
}

func TestHTTPIndexDeduplicatesAndConverges(t *testing.T) {
	opts := testManagerOptions(t)
	builder := newFakeBuilder(true)
	t.Cleanup(builder.releaseAll)
	m := newManager(opts, builder.build, context.Background())
	ts := newTestHTTPServer(t, builder, m)

	body := `{"CloneURL":"https://example.com/repo.git","RepoID":100,"Version":"v1"}`

	var wg sync.WaitGroup
	statuses := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, _ := postIndex(t, ts.URL, body)
			statuses[i] = status
		}(i)
	}

	waitUntil(t, time.Second, func() bool { return builder.buildCount() == 1 })
	builder.gateForNth("v1", 1).release()
	wg.Wait()

	for i, status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, status)
		}
	}
	if builder.buildCount() != 1 {
		t.Fatalf("duplicate HTTP requests should share one build, got %d", builder.buildCount())
	}

	// The same version again returns the cached result without building.
	status, decoded := postIndex(t, ts.URL, body)
	if status != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", status)
	}
	if reused, _ := decoded["Reused"].(bool); !reused {
		t.Fatalf("retry should be reported as reused, got %+v", decoded)
	}

	// A newer version drives a second generation.
	v2Done := make(chan int, 1)
	go func() {
		status, _ := postIndex(t, ts.URL,
			`{"CloneURL":"https://example.com/repo.git","RepoID":100,"Version":"v2"}`)
		v2Done <- status
	}()
	waitUntil(t, time.Second, func() bool { return builder.buildCount() == 2 })
	builder.gateForNth("v2", 2).release()
	if status := <-v2Done; status != http.StatusOK {
		t.Fatalf("v2 status = %d", status)
	}

	waitUntil(t, time.Second, func() bool {
		content, ok := liveShard(t, opts, 100)
		return ok && content == "v2"
	})
}

func TestHTTPIndexRejectsBadJSON(t *testing.T) {
	opts := testManagerOptions(t)
	builder := newFakeBuilder(false)
	m := newManager(opts, builder.build, context.Background())
	ts := newTestHTTPServer(t, builder, m)

	if status, _ := postIndex(t, ts.URL, `{not json`); status != http.StatusBadRequest {
		t.Fatalf("bad JSON status = %d, want 400", status)
	}

	// Unknown fields are rejected by DisallowUnknownFields.
	if status, _ := postIndex(t, ts.URL,
		`{"CloneURL":"u","RepoID":1,"Unexpected":true}`); status != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d, want 400", status)
	}
}

func TestHTTPIndexReportsBuildFailure(t *testing.T) {
	opts := testManagerOptions(t)
	builder := newFakeBuilder(false)
	builder.fail["bad"] = true
	m := newManager(opts, builder.build, context.Background())
	ts := newTestHTTPServer(t, builder, m)

	status, decoded := postIndex(t, ts.URL,
		`{"CloneURL":"https://example.com/repo.git","RepoID":100,"Version":"bad"}`)
	if status != http.StatusInternalServerError {
		t.Fatalf("failure status = %d, want 500", status)
	}
	if success, _ := decoded["Success"].(bool); success {
		t.Fatalf("failure response should report Success=false, got %+v", decoded)
	}
	if content, ok := liveShard(t, opts, 100); ok {
		t.Fatalf("failed first index must leave no live shard, got %q", content)
	}
}
