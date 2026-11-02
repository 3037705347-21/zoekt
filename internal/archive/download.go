package archive

// This file implements resumable, identity-checked downloads of remote
// archives.
//
// A download of a given (archive URL, commit) pair lives in its own private
// cache directory and consists of three files:
//
//   - <key>.part: the archive bytes downloaded so far.
//   - <key>.json: a checkpoint describing the last *verified* byte offset,
//     rolling checksums of those bytes, and the source representation's
//     identity (ETag / Last-Modified / whole-object digest).
//   - <key>.lock: an flock serializing concurrent runs for the same archive
//     and commit.
//
// The checkpoint and the partial file never live next to the *.zoekt shards
// the searcher loads, so an interrupted download is never visible as a
// searchable index. Resume is only allowed after the source is proven to be
// the same representation: HTTP validators (If-Range / ETag /
// Last-Modified), a re-requested overlap window compared byte-for-byte with
// the local tail, and a locally recomputed checksum of the already stored
// prefix. Any mismatch discards the partial download and starts over.

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	checkpointVersion = 1

	// maxDownloadRestarts bounds how often a single run throws away a
	// partial download because the source representation changed.
	maxDownloadRestarts = 2

	// overlapWindow is the number of bytes re-fetched before the last
	// verified offset so a resumed response can be proven to continue the
	// exact bytes already stored.
	overlapWindow int64 = 4096

	// checkpointInterval is how many bytes are appended before the verified
	// offset is durably recorded in the checkpoint sidecar.
	checkpointInterval int64 = 4 << 20
)

// Knobs (variables so tests can tighten them).
var (
	downloadMaxAttempts = 5
	downloadRetryDelay  = 500 * time.Millisecond
)

// Errors that trigger specific recovery actions.
var (
	errCheckpointStale   = errors.New("checkpoint does not match the requested archive and commit")
	errCheckpointDamaged = errors.New("partial archive failed local checksum verification")
	errContentChanged    = errors.New("archive content at the source changed")
	errOverlapMismatch   = errors.New("resumed bytes do not line up with the verified prefix")
)

// checkpoint is the JSON sidecar persisted next to a partial download.
type checkpoint struct {
	Version int `json:"version"`

	URL    string `json:"url"`
	Commit string `json:"commit,omitempty"`

	// Offset is the number of bytes that have been written and verified.
	Offset int64 `json:"offset"`
	// Size is the archive's total size in bytes, when the server reported
	// it. Zero means the size is unknown (chunked transfer).
	Size int64 `json:"size,omitempty"`
	// Complete is true once the full archive has been downloaded and
	// verified.
	Complete bool `json:"complete"`

	// SHA256 and MD5 are lowercase hex checksums of the first Offset bytes.
	SHA256 string `json:"sha256"`
	MD5    string `json:"md5,omitempty"`

	// Identity of the source representation. ETag only stores strong
	// validators.
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`

	// Whole-object digest advertised by the server (if any). DigestAlgo is
	// "sha256" or "md5", Digest is lowercase hex.
	DigestAlgo string `json:"digest_algo,omitempty"`
	Digest     string `json:"digest,omitempty"`
}

// archiveDownload is a fully downloaded, verified local copy of a remote
// archive.
type archiveDownload struct {
	dir      string
	key      string
	url      string
	commit   string
	partPath string
	metaPath string
	lockPath string
	lockFile *os.File
}

// hashes maintains the running checksums of the stored bytes.
type hashes struct {
	sha hash.Hash
	md5 hash.Hash
}

func newHashes() *hashes {
	return &hashes{sha: sha256.New(), md5: md5.New()}
}

func (h *hashes) write(p []byte) {
	// hash.Hash.Write never returns an error.
	_, _ = h.sha.Write(p)
	_, _ = h.md5.Write(p)
}

func (h *hashes) hex(algo string) string {
	switch algo {
	case "md5":
		return hex.EncodeToString(h.md5.Sum(nil))
	default:
		return hex.EncodeToString(h.sha.Sum(nil))
	}
}

// downloadCacheKey derives the on-disk key for an (url, commit) pair. The
// commit is part of the identity: interrupted reads only resume when both the
// archive location and the commit are the same.
func downloadCacheKey(u, commit string) string {
	sum := sha256.Sum256([]byte(u + "\x00" + commit))
	return hex.EncodeToString(sum[:])
}

// downloadArchive downloads u into dir, resuming from any verified checkpoint
// for (u, commit). The returned archiveDownload must be released by the
// caller: the cache is kept when indexing failed (so the next run can resume)
// and removed when indexing succeeded.
func downloadArchive(ctx context.Context, u, commit, dir string) (_ *archiveDownload, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create archive download directory %s: %w", dir, err)
	}

	key := downloadCacheKey(u, commit)
	d := &archiveDownload{
		dir:      dir,
		key:      key,
		url:      u,
		commit:   commit,
		partPath: filepath.Join(dir, key+".part"),
		metaPath: filepath.Join(dir, key+".json"),
		lockPath: filepath.Join(dir, key+".lock"),
	}

	if err := d.acquireLock(ctx); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			d.release(false)
		}
	}()

	if err := d.fetch(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// release unlocks the download and removes the cached bytes and checkpoint
// once they are no longer needed. The cache is kept after a failure of an
// incomplete download so the next invocation can resume; a fully downloaded
// archive whose later verification or indexing failed is discarded, since
// reusing it would repeat the same failure indefinitely.
func (d *archiveDownload) release(success bool) {
	remove := success
	if !remove && d.checkpointComplete() {
		remove = true
	}
	if remove {
		_ = os.Remove(d.partPath)
		_ = os.Remove(d.metaPath)
		_ = os.Remove(d.metaPath + ".tmp")
	}
	if d.lockFile != nil {
		for {
			err := unix.Flock(int(d.lockFile.Fd()), unix.LOCK_UN)
			if !errors.Is(err, unix.EINTR) {
				break
			}
		}
		_ = d.lockFile.Close()
		d.lockFile = nil
	}
}

// checkpointComplete reports whether the sidecar describes a fully downloaded
// archive. A missing or unreadable sidecar is treated as incomplete so a
// partial download is preserved.
func (d *archiveDownload) checkpointComplete() bool {
	data, err := os.ReadFile(d.metaPath)
	if err != nil {
		return false
	}
	var cp checkpoint
	return json.Unmarshal(data, &cp) == nil && cp.Complete
}

func (d *archiveDownload) acquireLock(ctx context.Context) error {
	f, err := os.OpenFile(d.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open download lock %s: %w", d.lockPath, err)
	}
	d.lockFile = f

	// Block until any other invocation for the same archive and commit is
	// done. Duplicate executions must not interleave writes.
	locked := make(chan error, 1)
	go func() {
		for {
			err := unix.Flock(int(f.Fd()), unix.LOCK_EX)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			locked <- err
			return
		}
	}()
	select {
	case err := <-locked:
		if err != nil {
			return fmt.Errorf("lock download %s: %w", d.lockPath, err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// fetch runs the download/verify loop until the complete archive is available
// locally and verified.
func (d *archiveDownload) fetch(ctx context.Context) error {
	// A missing sidecar is normal (first run); any other error means the
	// checkpoint is unusable, so a fresh start is safe.
	cp, _ := d.loadCheckpoint()

	f, err := os.OpenFile(d.partPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open partial archive %s: %w", d.partPath, err)
	}

	h, err := d.verifyPrefix(f, &cp)
	if err != nil {
		// Locally stored bytes do not match the checkpoint anymore. Discard
		// everything and re-download from the start.
		if f, h, err = d.reset(f); err != nil {
			return err
		}
		cp = d.freshCheckpoint()
	}

	var (
		lastErr  error
		restarts int
	)
	for attempt := 0; attempt < downloadMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(downloadRetryDelay):
			}
		}

		outcome, oerr := d.fetchOnce(ctx, f, &cp, h)
		switch outcome {
		case fetchDone:
			return nil
		case fetchFatal:
			return oerr
		case fetchRestart:
			restarts++
			if restarts > maxDownloadRestarts {
				return fmt.Errorf("archive at %s kept changing, giving up: %w", d.url, oerr)
			}
			if f, h, err = d.reset(f); err != nil {
				return err
			}
			cp = d.freshCheckpoint()
			lastErr = oerr
		case fetchRetry:
			lastErr = oerr
		}
	}

	if lastErr == nil {
		lastErr = errors.New("download did not complete")
	}
	return fmt.Errorf("downloading %s after %d attempts: %w", d.url, downloadMaxAttempts, lastErr)
}

func (d *archiveDownload) freshCheckpoint() checkpoint {
	return checkpoint{
		Version: checkpointVersion,
		URL:     d.url,
		Commit:  d.commit,
	}
}

type fetchOutcome int

const (
	fetchDone fetchOutcome = iota
	fetchRetry
	fetchRestart
	fetchFatal
)

// fetchOnce performs a single HTTP attempt. It either completes the download,
// records progress for a retry, or detects that the source changed and the
// partial download must be discarded.
func (d *archiveDownload) fetchOnce(ctx context.Context, f *os.File, cp *checkpoint, h *hashes) (fetchOutcome, error) {
	switch {
	case cp.Complete:
		return d.validateComplete(ctx, cp)
	case cp.Offset > 0:
		return d.resume(ctx, f, cp, h)
	default:
		return d.fetchFresh(ctx, f, cp, h)
	}
}

// validateComplete re-validates an already fully downloaded archive against
// the source before reusing it, so replaced content is never re-indexed.
func (d *archiveDownload) validateComplete(ctx context.Context, cp *checkpoint) (fetchOutcome, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.url, nil)
	if err != nil {
		return fetchFatal, err
	}
	setRange(req, 0, 0)
	setValidator(req, cp)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fetchRetry, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		if identityChanged(resp.Header, cp) {
			return fetchRestart, errContentChanged
		}
		start, _, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok || start != 0 || (cp.Size > 0 && total >= 0 && total != cp.Size) {
			return fetchRestart, errContentChanged
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return fetchDone, nil
	case http.StatusOK:
		// If-Range was rejected (content changed) or the server does not
		// support ranges: re-download the whole archive.
		return fetchRestart, errContentChanged
	case http.StatusRequestedRangeNotSatisfiable:
		return fetchRestart, errContentChanged
	default:
		return fetchFatal, responseError(resp)
	}
}

// resume continues an incomplete download with a Range request.
func (d *archiveDownload) resume(ctx context.Context, f *os.File, cp *checkpoint, h *hashes) (fetchOutcome, error) {
	start := cp.Offset
	if start > overlapWindow {
		start -= overlapWindow
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.url, nil)
	if err != nil {
		return fetchFatal, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
	setValidator(req, cp)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fetchRetry, err
	}

	switch resp.StatusCode {
	case http.StatusOK:
		// The entity changed (If-Range rejected) or the server ignores
		// Range requests: discard and start over.
		_ = resp.Body.Close()
		return fetchRestart, errContentChanged
	case http.StatusRequestedRangeNotSatisfiable:
		_ = resp.Body.Close()
		return fetchRestart, errContentChanged
	case http.StatusPartialContent:
		// Fall through below after closing over err path.
	default:
		err := responseError(resp)
		_ = resp.Body.Close()
		return fetchFatal, err
	}

	defer resp.Body.Close()

	if identityChanged(resp.Header, cp) {
		return fetchRestart, errContentChanged
	}

	crStart, _, crTotal, ok := parseContentRange(resp.Header.Get("Content-Range"))
	if !ok || crStart != start {
		// A different range than asked for cannot be spliced safely.
		return fetchRestart, errContentChanged
	}
	if crTotal >= 0 {
		if cp.Size > 0 && crTotal != cp.Size {
			return fetchRestart, errContentChanged
		}
		cp.Size = crTotal
	}
	captureIdentity(resp.Header, cp)

	// Re-fetch the bytes just before the verified offset and prove they are
	// identical. This catches silent content replacement even when servers
	// mis-handle (or lack) conditional validators.
	if err := d.verifyOverlap(resp.Body, f, start, cp.Offset); err != nil {
		if errors.Is(err, errOverlapMismatch) {
			return fetchRestart, err
		}
		return fetchRetry, err
	}

	if _, err := f.Seek(cp.Offset, io.SeekStart); err != nil {
		return fetchFatal, err
	}

	if err := d.copyBody(resp.Body, f, cp, h); err != nil {
		return fetchRetry, err
	}
	return d.finalize(cp, h)
}

// fetchFresh downloads the whole archive from offset zero.
func (d *archiveDownload) fetchFresh(ctx context.Context, f *os.File, cp *checkpoint, h *hashes) (fetchOutcome, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.url, nil)
	if err != nil {
		return fetchFatal, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fetchRetry, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		// Fall through.
	case resp.StatusCode == http.StatusPartialContent:
		// Server answered a plain GET with partial content: cannot tell
		// which bytes these are, so do not splice them.
		return fetchFatal, fmt.Errorf("server returned %q for a request without Range", resp.Status)
	default:
		return fetchFatal, responseError(resp)
	}

	captureIdentity(resp.Header, cp)
	if resp.ContentLength >= 0 {
		cp.Size = resp.ContentLength
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fetchFatal, err
	}
	if err := f.Truncate(0); err != nil {
		return fetchFatal, err
	}

	if err := d.copyBody(resp.Body, f, cp, h); err != nil {
		return fetchRetry, err
	}
	return d.finalize(cp, h)
}

// finalize checks the fully downloaded archive against everything the server
// promised and marks the checkpoint complete.
func (d *archiveDownload) finalize(cp *checkpoint, h *hashes) (fetchOutcome, error) {
	if cp.Size > 0 && cp.Offset != cp.Size {
		return fetchRetry, fmt.Errorf("short archive: got %d of %d bytes", cp.Offset, cp.Size)
	}
	if cp.DigestAlgo != "" && h.hex(cp.DigestAlgo) != cp.Digest {
		// The bytes do not match the source's whole-object digest: the two
		// reads must never be joined. Drop everything and re-fetch.
		return fetchRestart, fmt.Errorf("%w: %s digest mismatch", errContentChanged, cp.DigestAlgo)
	}
	cp.Complete = true
	if err := d.saveCheckpoint(nil, cp, h); err != nil {
		return fetchFatal, err
	}
	return fetchDone, nil
}

// copyBody appends body to f, updating checksums and the checkpoint. On read
// errors the verified offset is persisted before returning, so the next
// attempt resumes exactly there.
func (d *archiveDownload) copyBody(body io.Reader, f *os.File, cp *checkpoint, h *hashes) error {
	buf := make([]byte, 128<<10)
	var sinceFlush int64
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				return err
			}
			h.write(buf[:n])
			cp.Offset += int64(n)
			sinceFlush += int64(n)
			if sinceFlush >= checkpointInterval {
				if err := d.saveCheckpoint(f, cp, h); err != nil {
					return err
				}
				sinceFlush = 0
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return d.saveCheckpoint(f, cp, h)
			}
			// Persist progress despite the failed read, then report.
			_ = d.saveCheckpoint(f, cp, h)
			return rerr
		}
	}
}

// verifyOverlap reads offset-start bytes from body and compares them with the
// local bytes at the same positions.
func (d *archiveDownload) verifyOverlap(body io.Reader, f *os.File, start, offset int64) error {
	n := offset - start
	if n <= 0 {
		return nil
	}
	got := make([]byte, n)
	if _, err := io.ReadFull(body, got); err != nil {
		return err
	}
	want := make([]byte, n)
	if _, err := f.ReadAt(want, start); err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return errOverlapMismatch
	}
	return nil
}

// verifyPrefix validates the on-disk prefix against the checkpoint and seeds
// the rolling hashes at the verified offset. Bytes beyond the verified offset
// are truncated; a checksum mismatch is reported so the caller restarts.
func (d *archiveDownload) verifyPrefix(f *os.File, cp *checkpoint) (*hashes, error) {
	h := newHashes()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() < cp.Offset {
		return nil, errCheckpointDamaged
	}
	if fi.Size() > cp.Offset {
		if err := f.Truncate(cp.Offset); err != nil {
			return nil, err
		}
	}

	if cp.Offset > 0 {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		if _, err := io.CopyN(io.MultiWriter(h.sha, h.md5), f, cp.Offset); err != nil {
			return nil, err
		}
		if cp.SHA256 != "" && h.hex("sha256") != cp.SHA256 {
			return nil, errCheckpointDamaged
		}
		if cp.MD5 != "" && h.hex("md5") != cp.MD5 {
			return nil, errCheckpointDamaged
		}
	}

	if _, err := f.Seek(cp.Offset, io.SeekStart); err != nil {
		return nil, err
	}
	return h, nil
}

// reset throws away the partial download (and its checkpoint) and starts with
// an empty file. A nil file is allowed when nothing has been opened yet.
func (d *archiveDownload) reset(f *os.File) (*os.File, *hashes, error) {
	if f != nil {
		_ = f.Close()
	}
	for _, p := range []string{d.metaPath, d.metaPath + ".tmp", d.partPath} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("discard partial archive %s: %w", p, err)
		}
	}
	nf, err := os.OpenFile(d.partPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("recreate partial archive %s: %w", d.partPath, err)
	}
	return nf, newHashes(), nil
}

func (d *archiveDownload) loadCheckpoint() (checkpoint, error) {
	data, err := os.ReadFile(d.metaPath)
	if errors.Is(err, os.ErrNotExist) {
		return d.freshCheckpoint(), nil
	}
	if err != nil {
		return d.freshCheckpoint(), err
	}
	var cp checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return d.freshCheckpoint(), err
	}
	if cp.Version != checkpointVersion || cp.URL != d.url || cp.Commit != d.commit {
		return d.freshCheckpoint(), errCheckpointStale
	}
	return cp, nil
}

// saveCheckpoint atomically updates the sidecar. The data file is synced
// first, then the sidecar is written to a temp file and renamed, so a crash
// can never expose a sidecar past the bytes on disk.
func (d *archiveDownload) saveCheckpoint(f *os.File, cp *checkpoint, h *hashes) error {
	cp.Version = checkpointVersion
	cp.URL = d.url
	cp.Commit = d.commit
	cp.SHA256 = h.hex("sha256")
	cp.MD5 = h.hex("md5")

	if f != nil {
		if err := f.Sync(); err != nil {
			return err
		}
	}

	data, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	tmp := d.metaPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, d.metaPath); err != nil {
		return err
	}
	return nil
}

// setRange adds a Range: bytes=start-end header.
func setRange(req *http.Request, start, end int64) {
	var value string
	if end < 0 {
		value = fmt.Sprintf("bytes=%d-", start)
	} else {
		value = fmt.Sprintf("bytes=%d-%d", start, end)
	}
	req.Header.Set("Range", value)
}

// setValidator attaches the strongest available validator for If-Range, so
// compliant servers answer a Range request only while the representation is
// unchanged.
func setValidator(req *http.Request, cp *checkpoint) {
	if cp.ETag != "" {
		req.Header.Set("If-Range", cp.ETag)
	} else if cp.LastModified != "" {
		req.Header.Set("If-Range", cp.LastModified)
	}
}

// identityChanged compares the representation described by the response
// headers with the checkpoint.
func identityChanged(hdr http.Header, cp *checkpoint) bool {
	if e := hdr.Get("ETag"); e != "" && cp.ETag != "" && !strings.HasPrefix(e, "W/") && e != cp.ETag {
		return true
	}
	if lm := hdr.Get("Last-Modified"); lm != "" && cp.LastModified != "" && lm != cp.LastModified {
		return true
	}
	if algo, sum := advertisedDigest(hdr); algo != "" && cp.DigestAlgo != "" && (algo != cp.DigestAlgo || sum != cp.Digest) {
		return true
	}
	return false
}

// captureIdentity records representation metadata seen on a response for the
// first time, so later resumes can be validated against it.
func captureIdentity(hdr http.Header, cp *checkpoint) {
	if e := hdr.Get("ETag"); e != "" && !strings.HasPrefix(e, "W/") && cp.ETag == "" {
		cp.ETag = e
	}
	if lm := hdr.Get("Last-Modified"); lm != "" && cp.LastModified == "" {
		cp.LastModified = lm
	}
	if algo, sum := advertisedDigest(hdr); algo != "" && cp.DigestAlgo == "" {
		cp.DigestAlgo = algo
		cp.Digest = sum
	}
}

// parseContentRange parses a "bytes start-end/total" Content-Range value. A
// total of "*" is returned as -1.
func parseContentRange(v string) (start, end, total int64, ok bool) {
	const prefix = "bytes "
	if !strings.HasPrefix(v, prefix) {
		return 0, 0, 0, false
	}
	spec := strings.TrimPrefix(v, prefix)
	slash := strings.Index(spec, "/")
	if slash < 0 {
		return 0, 0, 0, false
	}
	rng, totalSpec := spec[:slash], spec[slash+1:]
	if totalSpec == "*" {
		total = -1
	} else {
		t, err := strconv.ParseInt(totalSpec, 10, 64)
		if err != nil || t < 0 {
			return 0, 0, 0, false
		}
		total = t
	}
	dash := strings.Index(rng, "-")
	if dash < 0 {
		return 0, 0, 0, false
	}
	s, err1 := strconv.ParseInt(rng[:dash], 10, 64)
	e, err2 := strconv.ParseInt(rng[dash+1:], 10, 64)
	if err1 != nil || err2 != nil || s < 0 || e < s {
		return 0, 0, 0, false
	}
	return s, e, total, true
}

// advertisedDigest extracts a whole-object digest the server vouchs for. It
// understands RFC 9530 Digest headers (sha-256/md5, base64) and the GCS
// x-goog-hash header (md5=base64). Returned values are lowercase hex.
func advertisedDigest(hdr http.Header) (algo, sumHex string) {
	prefer := func(a, s string) {
		if sumHex == "" || (a == "sha256" && algo != "sha256") {
			algo, sumHex = a, s
		}
	}
	for _, d := range hdr.Values("Digest") {
		if a, s := parseDigestHeader(d); s != "" {
			prefer(a, s)
		}
	}
	if g := hdr.Get("X-Goog-Hash"); g != "" {
		// e.g. "crc32c=....=,md5=....="
		for _, part := range strings.Split(g, ",") {
			part = strings.TrimSpace(part)
			if a, s, ok := strings.Cut(part, "="); ok && strings.EqualFold(a, "md5") {
				if decoded, err := base64.StdEncoding.DecodeString(strings.Trim(s, `" `)); err == nil {
					prefer("md5", hex.EncodeToString(decoded))
				}
			}
		}
	}
	return algo, sumHex
}

func parseDigestHeader(v string) (algo, sumHex string) {
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		name, raw, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		raw = strings.Trim(raw, `" `)
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "sha-256":
			return "sha256", hex.EncodeToString(decoded)
		case "md5":
			algo, sumHex = "md5", hex.EncodeToString(decoded)
		}
	}
	return algo, sumHex
}

func responseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("download %s: %s: %s", resp.Request.URL, resp.Status, strings.TrimSpace(string(body)))
}
