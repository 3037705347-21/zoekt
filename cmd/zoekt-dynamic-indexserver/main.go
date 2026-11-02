// Copyright 2016 Google Inc. All rights reserved.
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

// Command zoekt-dynamic-indexserver starts a server to manage dynamic indexing. In contrast to
// zoekt-indexserver, it's designed for a "push-based" indexing model. The server
// * listens to indexing commands
// * reindexes specified repositories
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func loggedRun(cmd *exec.Cmd) error {
	outBuf := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf

	log.Printf("run %v", cmd.Args)
	if err := cmd.Run(); err != nil {
		log.Printf("command %s failed: %v\nOUT: %s\nERR: %s",
			cmd.Args, err, outBuf.String(), errBuf.String())
		return fmt.Errorf("command %s failed: %v", cmd.Args, err)
	}

	return nil
}

type Options struct {
	indexTimeout time.Duration
	repoDir      string
	indexDir     string
	listen       string
}

func (o *Options) createMissingDirectories() {
	for _, s := range []string{o.repoDir, o.indexDir} {
		if err := os.MkdirAll(s, 0o755); err != nil {
			log.Fatalf("MkdirAll %s: %v", s, err)
		}
	}
}

type indexRequest struct {
	CloneURL string // TODO: Decide if tokens can be in the URL or if we should pass separately
	RepoID   uint32

	// Version optionally identifies the pushed version (e.g. a commit SHA).
	// Requests with the same Version are deduplicated: retries join the
	// in-flight attempt or reuse the published result. A new Version starts a
	// new generation. When empty, every request is treated as a new
	// generation.
	Version string `json:"Version,omitempty"`
}

// This function is declared as var so that we can stub it in test
var executeCmd = func(ctx context.Context, name string, arg ...string) error {
	cmd := exec.CommandContext(ctx, name, arg...)
	cmd.Stdin = &bytes.Buffer{}
	err := loggedRun(cmd)

	return err
}

// buildIndex runs clone, fetch and index for one attempt. The index is always
// written into the private stagingDir; publishing (atomic replacement of the
// live shards) is the manager's responsibility, so a failed or interrupted
// build cannot damage the previously searchable index.
func buildIndex(ctx context.Context, opts Options, req indexRequest, stagingDir string) error {
	id := strconv.FormatUint(uint64(req.RepoID), 10)

	clone := func() error {
		return executeCmd(ctx, "zoekt-git-clone",
			"-dest", opts.repoDir,
			"-name", id,
			"-repoid", id,
			req.CloneURL,
		)
	}

	if err := clone(); err != nil {
		// A process killed during the initial clone can leave a partial
		// repository directory that zoekt-git-clone would mistake for an
		// existing clone. Discard it and retry once so that recovery works
		// without manual intervention.
		gitRepoPath := filepath.Join(opts.repoDir, fmt.Sprintf("%d.git", req.RepoID))
		if abs, absErr := filepath.Abs(gitRepoPath); absErr == nil {
			gitRepoPath = abs
		}
		if _, statErr := os.Stat(gitRepoPath); statErr == nil && !isValidGitRepo(ctx, gitRepoPath) {
			if rmErr := os.RemoveAll(gitRepoPath); rmErr != nil {
				return fmt.Errorf("remove partial clone %s: %w", gitRepoPath, rmErr)
			}
			if err = clone(); err != nil {
				return err
			}
		} else {
			return err
		}
	}

	gitRepoPath, err := filepath.Abs(filepath.Join(opts.repoDir, fmt.Sprintf("%d.git", req.RepoID)))
	if err != nil {
		return err
	}

	// A fetch killed mid-flight may leave stale lock files behind, which
	// would make the next (recovery) fetch fail. Attempts are serialized per
	// repository, so removing the locks is safe.
	removeGitLockFiles(gitRepoPath)

	if err := executeCmd(ctx, "git", "-C", gitRepoPath, "fetch"); err != nil {
		return err
	}

	return executeCmd(ctx, "zoekt-git-index",
		"-index", stagingDir,
		gitRepoPath,
	)
}

// isValidGitRepo reports whether dir is a usable git repository.
func isValidGitRepo(ctx context.Context, dir string) bool {
	return executeCmd(ctx, "git", "-C", dir, "rev-parse", "--git-dir") == nil
}

// removeGitLockFiles deletes stale lock files a killed git command can leave
// behind in a bare repository.
func removeGitLockFiles(repoDir string) {
	rootLocks := []string{
		"index.lock",
		"config.lock",
		"packed-refs.lock",
		"FETCH_HEAD.lock",
		"shallow.lock",
	}
	for _, name := range rootLocks {
		if err := os.Remove(filepath.Join(repoDir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("dynamic-indexserver: remove lock %s: %v", name, err)
		}
	}

	refsDir := filepath.Join(repoDir, "refs")
	_ = filepath.WalkDir(refsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(path, ".lock") {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				log.Printf("dynamic-indexserver: remove lock %s: %v", path, err)
			}
		}
		return nil
	})
}

type indexServer struct {
	opts                 Options
	mgr                  *manager
	promRegistry         *prometheus.Registry
	metricsRequestsTotal *prometheus.CounterVec
	metricsJobsTotal     *prometheus.CounterVec
}

func (s *indexServer) serveHealthCheck(w http.ResponseWriter, r *http.Request) {
	// Nothing to do. Just return 200
}

func (s *indexServer) serveMetrics(w http.ResponseWriter, r *http.Request) {
	promhttp.HandlerFor(s.promRegistry, promhttp.HandlerOpts{Registry: s.promRegistry}).ServeHTTP(w, r)
}

func (s *indexServer) serveIndex(w http.ResponseWriter, r *http.Request) {
	route := "index"
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req indexRequest
	err := dec.Decode(&req)
	if err != nil {
		log.Printf("Error decoding index request: %v", err)
		http.Error(w, "JSON parser error", http.StatusBadRequest)
		return
	}

	response, err := s.mgr.Submit(r.Context(), req)
	if err != nil {
		// The client gave up waiting (its timeout fired, it will retry);
		// the attempt keeps running server side, so this is not a failure.
		if errors.Is(r.Context().Err(), context.Canceled) {
			return
		}
		s.respondWithError(w, r.Method, route, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)

	s.incrementRequestsTotal(r.Method, route, http.StatusOK)
}

func (s *indexServer) serveTruncate(w http.ResponseWriter, r *http.Request) {
	route := "truncate"
	if err := s.mgr.Truncate(r.Context()); err != nil {
		s.respondWithError(w, r.Method, route, err)
		return
	}

	response := map[string]any{
		"Success": true,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)

	s.incrementRequestsTotal(r.Method, route, http.StatusOK)
}

func (s *indexServer) respondWithError(w http.ResponseWriter, method, route string, err error) {
	responseCode := http.StatusInternalServerError

	log.Print(err)
	s.incrementRequestsTotal(method, route, responseCode)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(responseCode)
	response := map[string]any{
		"Success": false,
		"Error":   err.Error(),
	}

	_ = json.NewEncoder(w).Encode(response)
}

func (s *indexServer) incrementRequestsTotal(method, route string, responseCode int) {
	s.metricsRequestsTotal.With(prometheus.Labels{"code": strconv.Itoa(responseCode), "method": method, "route": route}).Inc()
}

func (s *indexServer) initMetrics() {
	s.promRegistry = prometheus.NewRegistry()

	// Add go runtime metrics and process collectors.
	s.promRegistry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	s.metricsRequestsTotal = promauto.With(s.promRegistry).NewCounterVec(
		prometheus.CounterOpts{
			Name: "zoekt_dynamic_indexserver_requests_total",
			Help: "Total number of HTTP requests by status code, method, and route.",
		},
		[]string{"method", "route", "code"},
	)

	s.metricsJobsTotal = promauto.With(s.promRegistry).NewCounterVec(
		prometheus.CounterOpts{
			Name: "zoekt_dynamic_indexserver_index_jobs_total",
			Help: "Total number of indexing attempts by outcome " +
				"(published, reused, superseded, failed).",
		},
		[]string{"outcome"},
	)
}

func (s *indexServer) startIndexingApi(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.serveHealthCheck)
	mux.HandleFunc("/metrics", s.serveMetrics)
	mux.HandleFunc("/index", s.serveIndex)
	mux.HandleFunc("/truncate", s.serveTruncate)

	httpServer := &http.Server{
		Addr:    s.opts.listen,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP shutdown: %v", err)
		}
	}()

	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func emptyDirectory(dir string) error {
	files, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	for _, file := range files {
		filePath := filepath.Join(dir, file.Name())
		err := os.RemoveAll(filePath)
		if err != nil {
			return err
		}
	}

	return nil
}

func parseOptions() Options {
	repoDir := flag.String("repo_dir", "", "directory holding cloned repos.")
	indexDir := flag.String("index_dir", "", "directory holding index shards.")
	timeout := flag.Duration("index_timeout", time.Hour, "kill index job after this much time.")
	listen := flag.String("listen", ":6060", "listen on this address.")
	flag.Parse()

	if *repoDir == "" {
		log.Fatal("must set -repo_dir")
	}

	if *indexDir == "" {
		log.Fatal("must set -index_dir")
		*indexDir = filepath.Join(*repoDir, "index")
	}

	return Options{
		repoDir:      *repoDir,
		indexDir:     *indexDir,
		indexTimeout: *timeout,
		listen:       *listen,
	}
}

func main() {
	opts := parseOptions()
	opts.createMissingDirectories()

	// Cancelling on signal interrupts in-flight builds; their staging data
	// and journals are reconciled on the next start.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := indexServer{
		opts: opts,
	}
	server.initMetrics()

	server.mgr = newManager(opts, func(ctx context.Context, req indexRequest, stagingDir string) error {
		return buildIndex(ctx, opts, req, stagingDir)
	}, ctx)
	server.mgr.observe = func(outcome string) {
		server.metricsJobsTotal.WithLabelValues(outcome).Inc()
	}

	if err := server.mgr.Recover(); err != nil {
		log.Fatalf("startup recovery failed: %v", err)
	}

	server.startIndexingApi(ctx)
}
