package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
	"github.com/sourcegraph/zoekt/search"
)

// requestInfo records what one request to flakyServer looked like and what
// status the server answered with.
type requestInfo struct {
	hasRange    bool
	rangeHeader string
	status      int
}

// flakyServer serves archive data with full Range/If-Range support, but can
// be configured to cut the connection after a fixed number of bytes for the
// next N requests, simulating object storage dropping the last segment.
type flakyServer struct {
	t        *testing.T
	mu       sync.Mutex
	data     []byte
	etag     string
	failNext int
	cutBytes int64
	adDigest bool
	reqs     []requestInfo
}

func (s *flakyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	data := bytes.Clone(s.data)
	etag := s.etag
	failing := s.failNext > 0
	if failing {
		s.failNext--
	}
	info := requestInfo{
		hasRange:    r.Header.Get("Range") != "",
		rangeHeader: r.Header.Get("Range"),
		status:      http.StatusOK,
	}
	adDigest := s.adDigest
	s.mu.Unlock()

	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Last-Modified", modTime.UTC().Format(http.TimeFormat))
	h.Set("Accept-Ranges", "bytes")
	if adDigest {
		sum := sha256.Sum256(data)
		h.Set("Digest", `sha-256=`+base64.StdEncoding.EncodeToString(sum[:]))
	}

	sw := &statusWriter{ResponseWriter: w}
	defer func() {
		if sw.code == 0 {
			sw.code = http.StatusOK
		}
		info.status = sw.code
		s.mu.Lock()
		s.reqs = append(s.reqs, info)
		s.mu.Unlock()
	}()

	if failing {
		s.serveTruncated(sw, r, data, &info)
		return
	}

	http.ServeContent(sw, r, "archive", modTime, bytes.NewReader(data))
}

// serveTruncated answers with the correct headers and Content-Length but only
// writes part of the body before returning, which drops the connection.
func (s *flakyServer) serveTruncated(w http.ResponseWriter, r *http.Request, data []byte, info *requestInfo) {
	w.Header().Set("Content-Type", "application/x-gzip")

	rng := r.Header.Get("Range")
	if rng == "" {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		n := min64(s.cutBytes, int64(len(data)))
		_, _ = w.Write(data[:n])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}

	start := parseSuffixRange(rng, s.t)
	chunk := data[start:]
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(data)-1, len(data)))
	w.Header().Set("Content-Length", strconv.Itoa(len(chunk)))
	w.WriteHeader(http.StatusPartialContent)
	info.status = http.StatusPartialContent

	n := min64(s.cutBytes, int64(len(chunk)))
	_, _ = w.Write(chunk[:n])
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *flakyServer) swap(data []byte, etag string) {
	s.mu.Lock()
	s.data = data
	s.etag = etag
	s.failNext = 0
	s.mu.Unlock()
}

func (s *flakyServer) requests() []requestInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]requestInfo(nil), s.reqs...)
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func parseSuffixRange(v string, t *testing.T) int64 {
	t.Helper()
	const prefix = "bytes="
	if !strings.HasPrefix(v, prefix) {
		t.Fatalf("unexpected range header %q", v)
	}
	part := strings.TrimPrefix(v, prefix)
	if i := strings.Index(part, "-"); i >= 0 {
		part = part[:i]
	}
	n, err := strconv.ParseInt(part, 10, 64)
	require.NoError(t, err)
	return n
}

// withFastRetries tightens the downloader retry knobs for tests.
func withFastRetries(t *testing.T, attempts int) {
	t.Helper()

	oldAttempts := downloadMaxAttempts
	oldDelay := downloadRetryDelay
	downloadMaxAttempts = attempts
	downloadRetryDelay = time.Millisecond
	t.Cleanup(func() {
		downloadMaxAttempts = oldAttempts
		downloadRetryDelay = oldDelay
	})
}

func makeArchive(t *testing.T, format, marker string, seed int64) []byte {
	t.Helper()

	files := map[string]string{}
	for i := range 12 {
		name := fmt.Sprintf("dir/file%d.txt", i)
		var b strings.Builder
		rnd := rand.New(rand.NewSource(seed + int64(i)))
		for b.Len() < 40*1024 {
			b.WriteString(marker + " ")
			for range 32 {
				b.WriteByte(byte('a' + rnd.Intn(26)))
			}
			b.WriteByte('\n')
		}
		files[name] = b.String()
	}

	var buf bytes.Buffer
	require.NoError(t, writeArchive(&buf, format, files))
	return buf.Bytes()
}

func indexOptions(indexDir string) index.Options {
	return index.Options{IndexDir: indexDir}
}

func archiveOptions(u, name, commit string) Options {
	return Options{
		Archive: u,
		Name:    name,
		Branch:  "master",
		Commit:  commit,
	}
}

func searchHits(t *testing.T, indexDir, q string) int {
	t.Helper()

	ss, err := search.NewDirectorySearcher(indexDir)
	require.NoError(t, err)
	defer ss.Close()

	qq, err := query.Parse(q)
	require.NoError(t, err)

	res, err := ss.Search(context.Background(), qq, &zoekt.SearchOptions{})
	require.NoError(t, err)
	return len(res.Files)
}

func rootGlob(t *testing.T, indexDir, pattern string) []string {
	t.Helper()
	fs, err := filepath.Glob(filepath.Join(indexDir, pattern))
	require.NoError(t, err)
	return fs
}

func readCheckpoint(t *testing.T, cacheDir, u, commit string) checkpoint {
	t.Helper()
	key := downloadCacheKey(u, commit)
	data, err := os.ReadFile(filepath.Join(cacheDir, key+".json"))
	require.NoError(t, err)
	var cp checkpoint
	require.NoError(t, json.Unmarshal(data, &cp))
	return cp
}

func startFlakyServer(t *testing.T, data []byte, etag string, failNext int, cut int64) *serverHandle {
	t.Helper()
	h := &flakyServer{
		t:        t,
		data:     bytes.Clone(data),
		etag:     etag,
		failNext: failNext,
		cutBytes: cut,
		adDigest: true,
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return &serverHandle{flakyServer: h, url: ts.URL}
}

// serverHandle pairs the handler with the test server URL.
type serverHandle struct {
	*flakyServer
	url string
}

// TestResumeFromVerifiedOffset verifies that an archive whose connection is
// repeatedly dropped is resumed from the last verified offset on the next run,
// that no searchable shard is published while incomplete, and that the cache is
// cleaned after success.
func TestResumeFromVerifiedOffset(t *testing.T) {
	withFastRetries(t, 3)

	data := makeArchive(t, "tgz", "MARKER_RESUME", 1)
	srv := startFlakyServer(t, data, `"v1"`, 3, 64*1024)

	indexDir := t.TempDir()
	cacheDir := filepath.Join(indexDir, ".archive")
	opts := archiveOptions(srv.url, "repo/resume", "cccccccccccccccccccccccccccccccccccccccc")

	// First run: all three attempts are cut short.
	err := Index(opts, indexOptions(indexDir))
	require.Error(t, err)

	// Nothing searchable may exist, and no temp shards may be left behind.
	require.Empty(t, rootGlob(t, indexDir, "*.zoekt"))
	require.Empty(t, rootGlob(t, indexDir, "*.tmp"))

	cp := readCheckpoint(t, cacheDir, srv.url, opts.Commit)
	require.False(t, cp.Complete)
	require.Greater(t, cp.Offset, int64(0))
	require.Less(t, cp.Offset, int64(len(data)))
	require.NotEmpty(t, cp.SHA256)
	require.Equal(t, `"v1"`, cp.ETag)

	// Retries after the first attempt must have used Range requests.
	reqs := srv.requests()
	require.GreaterOrEqual(t, len(reqs), 2)
	require.False(t, reqs[0].hasRange)
	for _, r := range reqs[1:] {
		require.True(t, r.hasRange, "retry should request a byte range")
	}

	// Second run: server is healthy again; resume and finish.
	err = Index(opts, indexOptions(indexDir))
	require.NoError(t, err)

	require.NotEmpty(t, rootGlob(t, indexDir, "*.zoekt"))
	require.Empty(t, rootGlob(t, indexDir, "*.tmp"))
	require.Greater(t, searchHits(t, indexDir, "MARKER_RESUME"), 0)

	// Cache is removed only after the verified index was published.
	key := downloadCacheKey(srv.url, opts.Commit)
	require.NoFileExists(t, filepath.Join(cacheDir, key+".part"))
	require.NoFileExists(t, filepath.Join(cacheDir, key+".json"))
}

// TestResumeWithinSingleRun verifies a single Index call retries and resumes
// by itself, including for zip archives which require a complete local file.
func TestResumeWithinSingleRun(t *testing.T) {
	for _, format := range []string{"tgz", "zip"} {
		t.Run(format, func(t *testing.T) {
			withFastRetries(t, 5)

			data := makeArchive(t, format, "MARKER_INFLIGHT", 7)
			srv := startFlakyServer(t, data, `"v1"`, 2, 64*1024)

			indexDir := t.TempDir()
			opts := archiveOptions(srv.url, "repo/inflight-"+format, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

			require.NoError(t, Index(opts, indexOptions(indexDir)))

			reqs := srv.requests()
			require.GreaterOrEqual(t, len(reqs), 3)
			require.False(t, reqs[0].hasRange)
			require.True(t, reqs[1].hasRange)
			require.Greater(t, searchHits(t, indexDir, "MARKER_INFLIGHT"), 0)
			require.Empty(t, rootGlob(t, indexDir, "*.tmp"))
		})
	}
}

// TestReplacedSourceRestarts verifies that when the content behind the same
// archive URL and commit is replaced between runs, the partial bytes are
// discarded and the whole archive is fetched again: two reads are never
// spliced into one index.
func TestReplacedSourceRestarts(t *testing.T) {
	withFastRetries(t, 3)

	v1 := makeArchive(t, "tgz", "MARKER_OLD_VERSION", 2)
	v2 := makeArchive(t, "tgz", "MARKER_NEW_VERSION", 99)
	require.NotEqual(t, v1, v2)

	srv := startFlakyServer(t, v1, `"v1"`, 3, 64*1024)

	indexDir := t.TempDir()
	commit := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	opts := archiveOptions(srv.url, "repo/replaced", commit)

	require.Error(t, Index(opts, indexOptions(indexDir)))

	// Same URL, same commit, different bytes.
	srv.swap(v2, `"v2"`)

	require.NoError(t, Index(opts, indexOptions(indexDir)))

	// The old content must not leak into the new index.
	require.Equal(t, 0, searchHits(t, indexDir, "MARKER_OLD_VERSION"))
	require.Greater(t, searchHits(t, indexDir, "MARKER_NEW_VERSION"), 0)

	// The second run must have restarted from byte zero: a Range/If-Range
	// request answered 200 followed by a plain GET.
	reqs := srv.requests()
	var sawResume, sawRestart bool
	for _, r := range reqs {
		if r.hasRange {
			sawResume = true
		}
		if sawResume && !r.hasRange && r.status == http.StatusOK {
			sawRestart = true
		}
	}
	require.True(t, sawResume, "expected a resume attempt with If-Range")
	require.True(t, sawRestart, "expected a full restart after source changed")
}

// TestDamagedCheckpointRestarts verifies that locally corrupt partial bytes
// (checksum no longer matches the sidecar) are discarded and the archive is
// downloaded from scratch.
func TestDamagedCheckpointRestarts(t *testing.T) {
	withFastRetries(t, 3)

	data := makeArchive(t, "tgz", "MARKER_DAMAGED", 3)

	indexDir := t.TempDir()
	cacheDir := filepath.Join(indexDir, ".archive")
	commit := "dddddddddddddddddddddddddddddddddddddddd"

	// Produce a valid checkpoint from a run where every attempt is cut.
	bad := startFlakyServer(t, data, `"v1"`, 3, 64*1024)
	opts := archiveOptions(bad.url, "repo/damaged", commit)
	require.Error(t, Index(opts, indexOptions(indexDir)))

	// Corrupt the stored prefix while keeping the sidecar offset.
	key := downloadCacheKey(bad.url, commit)
	partPath := filepath.Join(cacheDir, key+".part")
	part, err := os.ReadFile(partPath)
	require.NoError(t, err)
	copy(part[8:40], bytes.Repeat([]byte{'Z'}, 32))
	require.NoError(t, os.WriteFile(partPath, part, 0o600))

	// A fresh server instance lets us assert the first request is a full
	// GET: the corrupt checkpoint must not be resumed.
	good := startFlakyServer(t, data, `"v1"`, 0, 0)
	opts.Archive = good.url
	require.NoError(t, Index(opts, indexOptions(indexDir)))

	reqs := good.requests()
	require.NotEmpty(t, reqs)
	for _, r := range reqs {
		require.False(t, r.hasRange, "damaged prefix must not be resumed with a range")
	}
	require.Greater(t, searchHits(t, indexDir, "MARKER_DAMAGED"), 0)
}

// TestFailureDoesNotTouchOtherRepos verifies that a repeatedly failing
// download leaves another repository's published index untouched and leaves
// no half-built shards of its own.
func TestFailureDoesNotTouchOtherRepos(t *testing.T) {
	withFastRetries(t, 2)

	indexDir := t.TempDir()

	// Publish an index for repo B from a local archive.
	goodData := makeArchive(t, "tgz", "MARKER_REPO_B", 5)
	localArchive := filepath.Join(t.TempDir(), "b.tgz")
	require.NoError(t, os.WriteFile(localArchive, goodData, 0o600))
	bOpts := archiveOptions(localArchive, "repo/b", "1111111111111111111111111111111111111111")
	require.NoError(t, Index(bOpts, indexOptions(indexDir)))
	before := rootGlob(t, indexDir, "*.zoekt")
	require.Len(t, before, 1)
	require.Greater(t, searchHits(t, indexDir, "MARKER_REPO_B"), 0)

	// Repo A's remote download keeps failing.
	aData := makeArchive(t, "tgz", "MARKER_REPO_A", 6)
	srv := startFlakyServer(t, aData, `"v1"`, 100, 64*1024)
	aOpts := archiveOptions(srv.url, "repo/a", "2222222222222222222222222222222222222222")
	require.Error(t, Index(aOpts, indexOptions(indexDir)))

	// Only repo B's shard exists; no tmp shards for repo A.
	require.Equal(t, before, rootGlob(t, indexDir, "*.zoekt"))
	require.Empty(t, rootGlob(t, indexDir, "*.tmp"))
	require.Equal(t, 0, searchHits(t, indexDir, "MARKER_REPO_A"))
	require.Greater(t, searchHits(t, indexDir, "MARKER_REPO_B"), 0)

	// Repo A's partial download is namespaced in the cache only.
	key := downloadCacheKey(srv.url, aOpts.Commit)
	require.FileExists(t, filepath.Join(indexDir, ".archive", key+".part"))
}

// TestDuplicateExecutionsSerialize verifies two concurrent invocations for the
// same archive and commit do not corrupt each other and leave no residue.
func TestDuplicateExecutionsSerialize(t *testing.T) {
	data := makeArchive(t, "tgz", "MARKER_CONCURRENT", 8)
	srv := startFlakyServer(t, data, `"v1"`, 0, 0)

	indexDir := t.TempDir()
	opts := archiveOptions(srv.url, "repo/concurrent", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- Index(opts, indexOptions(indexDir))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	require.NotEmpty(t, rootGlob(t, indexDir, "*.zoekt"))
	require.Empty(t, rootGlob(t, indexDir, "*.tmp"))
	require.Greater(t, searchHits(t, indexDir, "MARKER_CONCURRENT"), 0)

	entries, err := filepath.Glob(filepath.Join(indexDir, ".archive", "*.part"))
	require.NoError(t, err)
	require.Empty(t, entries)
}

// TestCheckpointKeyedByCommit verifies that different commits at the same URL
// get isolated downloads rather than sharing a partial file.
func TestCheckpointKeyedByCommit(t *testing.T) {
	c1 := "1111111111111111111111111111111111111111"
	c2 := "2222222222222222222222222222222222222222"
	require.NotEqual(t, downloadCacheKey("https://x/a", c1), downloadCacheKey("https://x/a", c2))
	require.Equal(t, downloadCacheKey("https://x/a", c1), downloadCacheKey("https://x/a", c1))
}

// TestReleasePolicy verifies failed partial downloads are kept for resume
// while a complete download rejected during verification/indexing is removed.
func TestReleasePolicy(t *testing.T) {
	newDownload := func(t *testing.T) (*archiveDownload, string, string) {
		dir := t.TempDir()
		url := "https://example.invalid/a.tgz"
		commit := "3333333333333333333333333333333333333333"
		key := downloadCacheKey(url, commit)
		d := &archiveDownload{
			dir:      dir,
			key:      key,
			url:      url,
			commit:   commit,
			partPath: filepath.Join(dir, key+".part"),
			metaPath: filepath.Join(dir, key+".json"),
			lockPath: filepath.Join(dir, key+".lock"),
		}
		return d, d.partPath, d.metaPath
	}

	t.Run("incomplete failure resumes later", func(t *testing.T) {
		d, part, meta := newDownload(t)
		require.NoError(t, os.WriteFile(part, []byte("partial"), 0o600))
		cp := d.freshCheckpoint()
		cp.Offset = int64(len("partial"))
		cp.SHA256 = "x"
		data, err := json.Marshal(cp)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(meta, data, 0o600))

		d.release(false)

		require.FileExists(t, part)
		require.FileExists(t, meta)
	})

	t.Run("complete failure is discarded", func(t *testing.T) {
		d, part, meta := newDownload(t)
		require.NoError(t, os.WriteFile(part, []byte("full"), 0o600))
		cp := d.freshCheckpoint()
		cp.Offset = int64(len("full"))
		cp.Complete = true
		cp.SHA256 = "x"
		data, err := json.Marshal(cp)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(meta, data, 0o600))

		d.release(false)

		require.NoFileExists(t, part)
		require.NoFileExists(t, meta)
	})
}
