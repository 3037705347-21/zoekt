// Copyright 2026 Sourcegraph, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gitindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
)

// Submodule failure modes. They are sentinels so the tree walker can tell
// recoverable, roster-handled failures apart from ordinary walk errors.
var (
	// errSubmoduleUnavailable means that a submodule attributed to the
	// parent repository could not be opened or read, and no usable
	// last-known-good snapshot exists for it. The whole index update must
	// be abandoned: landing a shard missing this submodule would silently
	// drop it from search.
	errSubmoduleUnavailable = errors.New("submodule is unavailable")

	// errSubmoduleUnattributable means that a submodule gitlink in the
	// parent tree cannot be matched to a .gitmodules entry (or its URL
	// cannot be resolved). The content's ownership in the parent repository
	// cannot be confirmed, so the entire update must be abandoned.
	errSubmoduleUnattributable = errors.New("submodule attribution cannot be confirmed")
)

const (
	// submoduleRosterVersion is the manifest format version.
	submoduleRosterVersion = 1

	// submoduleRosterManifestName is the committed roster manifest. Writes
	// go through a temporary file that is atomically renamed.
	submoduleRosterManifestName = "manifest.json"

	// submoduleSnapshotMagic prefixes every content-addressed submodule
	// snapshot blob.
	submoduleSnapshotMagic = "ZOEKT-SUBMOD-1\n"

	// submoduleSnapshotFlagMissing marks a blob that the submodule itself
	// could not provide when the snapshot was taken (for example blobs
	// filtered out of a partial clone). The document is reproduced as a
	// skipped document instead of disappearing.
	submoduleSnapshotFlagMissing uint8 = 1 << 0
)

// submoduleRoster persists, for one parent repository, the last-known-good
// contents of each of its submodules together with the submodule's
// attribution (stable identity, URL, path inside the parent, and gitlink
// commit).
//
// The roster is the source used to keep serving a submodule's content when
// the submodule repository temporarily cannot be opened. A submodule is only
// retired after a complete, successful indexing run no longer lists it.
//
// Directory layout:
//
//	<baseDir>/<parentID>/manifest.json
//	<baseDir>/<parentID>/<snapshot-sha256>.bin
//
// parentID is derived from the parent repository's normalized source path,
// so every parent repository gets an independent roster. Snapshot blobs are
// content-addressed: moving a submodule (which leaves its contents unchanged)
// keeps referencing the same blob and therefore leaves no duplicate copy.
//
// All state changes happen by writing whole files and atomically renaming
// them into place, so an interrupted run leaves the previous state intact
// and an identical rerun is a no-op.
type submoduleRoster struct {
	dir    string
	source string
	name   string

	// prev holds the last committed entries, keyed by the submodule's
	// stable identity (the .gitmodules section name).
	prev map[string]*submoduleRosterEntry

	// desired collects the attribution observed during the current run,
	// keyed by identity.
	desired map[string]submoduleRosterEntry

	// fresh holds snapshots captured from readable submodules during the
	// current run, keyed by identity.
	fresh map[string]*submoduleSnapshot
}

// submoduleRosterEntry is one persisted submodule record.
type submoduleRosterEntry struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Path   string `json:"path"`
	Commit string `json:"commit"`

	// Snapshot is the content-addressed blob name (sha256 hex) holding the
	// last-known-good file contents.
	Snapshot string                `json:"snapshot"`
	Files    []submoduleRosterFile `json:"files"`
}

// submoduleRosterFile is the per-file metadata used to verify snapshot
// integrity when restoring content.
type submoduleRosterFile struct {
	// Sub is the file's SubRepositoryPath relative to the submodule root
	// (empty for the submodule's own files, non-empty for nested
	// submodules).
	Sub string `json:"sub"`
	// Path is the file path relative to Sub.
	Path string `json:"path"`

	Size    int64  `json:"size"`
	Hash    string `json:"hash,omitempty"`
	Missing bool   `json:"missing,omitempty"`
}

type submoduleRosterParent struct {
	Source string `json:"source"`
	Name   string `json:"name"`
}

type submoduleRosterManifest struct {
	Version    int                     `json:"version"`
	Parent     submoduleRosterParent   `json:"parent"`
	Submodules []*submoduleRosterEntry `json:"submodules"`
}

// submoduleSnapshotFile is one file as captured from a readable submodule.
type submoduleSnapshotFile struct {
	sub     string
	path    string
	content []byte
	// missing reproduces a blob the submodule repository itself could not
	// provide (content is nil).
	missing bool
}

// submoduleSnapshot is the last-known-good file set of one submodule.
type submoduleSnapshot struct {
	files []submoduleSnapshotFile
}

// normalizeRosterSource canonicalizes a parent repository source path the
// same way the local sync inventory does, so roster records and index shards
// agree on repository identity.
func normalizeRosterSource(source string) (string, error) {
	absolute, err := filepath.Abs(source)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	if filepath.Base(absolute) == ".git" {
		absolute = filepath.Dir(absolute)
	}
	return absolute, nil
}

// rosterParentID returns the stable, filesystem-safe identity of a parent
// repository within the roster directory.
func rosterParentID(source string) string {
	sum := sha256.Sum256([]byte(source))
	return hex.EncodeToString(sum[:])
}

// openSubmoduleRoster loads (or prepares) the roster for the parent
// repository identified by source. An existing roster whose parent
// attribution does not match is rejected rather than adopted.
func openSubmoduleRoster(baseDir, source, name string) (*submoduleRoster, error) {
	source, err := normalizeRosterSource(source)
	if err != nil {
		return nil, fmt.Errorf("normalize parent repository source: %w", err)
	}

	dir := filepath.Join(baseDir, rosterParentID(source))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create submodule roster directory %s: %w", dir, err)
	}

	r := &submoduleRoster{
		dir:     dir,
		source:  source,
		name:    name,
		prev:    map[string]*submoduleRosterEntry{},
		desired: map[string]submoduleRosterEntry{},
		fresh:   map[string]*submoduleSnapshot{},
	}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

// DeleteSubmoduleRoster removes persisted submodule state for the parent
// repository identified by source. It is used when the parent repository
// itself is retired from the index. A missing roster is not an error.
func DeleteSubmoduleRoster(baseDir, source string) error {
	if baseDir == "" {
		return nil
	}
	source, err := normalizeRosterSource(source)
	if err != nil {
		return err
	}
	err = os.RemoveAll(filepath.Join(baseDir, rosterParentID(source)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func rosterManifestPath(dir string) string {
	return filepath.Join(dir, submoduleRosterManifestName)
}

// load reads the committed manifest and reconciles the directory against it,
// discarding leftovers (staged temps, unreferenced blobs) from interrupted
// runs. The on-disk roster is therefore always usable as rescue state even
// after a crash.
func (r *submoduleRoster) load() error {
	referenced := map[string]struct{}{}

	data, err := os.ReadFile(rosterManifestPath(r.dir))
	if errors.Is(err, fs.ErrNotExist) {
		// No committed state: everything in the directory is a staging
		// leftover.
		return r.garbageCollect(referenced)
	}
	if err != nil {
		return fmt.Errorf("read submodule roster manifest: %w", err)
	}

	var manifest submoduleRosterManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse submodule roster manifest: %w", err)
	}
	if manifest.Version != submoduleRosterVersion {
		return fmt.Errorf("unsupported submodule roster version %d", manifest.Version)
	}
	// Ownership check: the roster directory is derived from the source
	// path, and the manifest must agree. Refuse to adopt state whose
	// attribution cannot be confirmed.
	if manifest.Parent.Source != r.source {
		return fmt.Errorf("%w: roster belongs to %q, refusing to use it for %q",
			errSubmoduleUnattributable, manifest.Parent.Source, r.source)
	}

	for _, entry := range manifest.Submodules {
		if entry == nil || entry.Name == "" || entry.Snapshot == "" {
			return fmt.Errorf("submodule roster manifest contains an invalid entry: %+v", entry)
		}
		if _, dup := r.prev[entry.Name]; dup {
			return fmt.Errorf("submodule roster manifest lists %q more than once", entry.Name)
		}
		r.prev[entry.Name] = entry
		referenced[entry.Snapshot] = struct{}{}
	}

	return r.garbageCollect(referenced)
}

// previous returns the last committed entry for identity, or nil.
func (r *submoduleRoster) previous(identity string) *submoduleRosterEntry {
	return r.prev[identity]
}

// noteFresh records that identity was fully readable this run: path and url
// carry its current attribution and snapshot the content captured from the
// submodule. The first capture for an identity wins (content is identical on
// every indexed branch).
func (r *submoduleRoster) noteFresh(identity, path, url, commit string, snapshot *submoduleSnapshot) {
	if identity == "" || snapshot == nil {
		return
	}
	r.desired[identity] = submoduleRosterEntry{Name: identity, URL: url, Path: path, Commit: commit}
	if _, ok := r.fresh[identity]; !ok {
		r.fresh[identity] = snapshot
	}
}

// noteReused records that identity could not be read this run and is being
// served from its last-known-good snapshot. The current attribution (path,
// url, gitlink commit) is remembered so that moves are tracked.
func (r *submoduleRoster) noteReused(identity, path, url, commit string) {
	if identity == "" {
		return
	}
	r.desired[identity] = submoduleRosterEntry{Name: identity, URL: url, Path: path, Commit: commit}
}

// loadSnapshot reads and verifies the persisted snapshot referenced by entry.
func (r *submoduleRoster) loadSnapshot(entry *submoduleRosterEntry) (*submoduleSnapshot, error) {
	path := filepath.Join(r.dir, entry.Snapshot)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read saved content for submodule %q: %w", entry.Name, err)
	}
	snapshot, err := unmarshalSubmoduleSnapshot(data)
	if err != nil {
		return nil, fmt.Errorf("decode saved content for submodule %q: %w", entry.Name, err)
	}
	if len(snapshot.files) != len(entry.Files) {
		return nil, fmt.Errorf("saved content for submodule %q lists %d files, manifest records %d",
			entry.Name, len(snapshot.files), len(entry.Files))
	}
	for i, meta := range entry.Files {
		file := snapshot.files[i]
		if file.sub != meta.Sub || file.path != meta.Path || file.missing != meta.Missing {
			return nil, fmt.Errorf("saved content for submodule %q does not match its recorded attribution at file %d",
				entry.Name, i)
		}
		if meta.Missing {
			continue
		}
		sum := sha256.Sum256(file.content)
		if got := hex.EncodeToString(sum[:]); got != meta.Hash {
			return nil, fmt.Errorf("saved content for submodule %q is corrupt at %s/%s",
				entry.Name, meta.Sub, meta.Path)
		}
		if int64(len(file.content)) != meta.Size {
			return nil, fmt.Errorf("saved content for submodule %q has wrong size at %s/%s",
				entry.Name, meta.Sub, meta.Path)
		}
	}
	return snapshot, nil
}

// commit lands the new roster state after a complete successful index build:
// fresh snapshots are persisted, reused identities keep their existing
// blobs (with updated attribution), and identities no longer present are
// retired together with their blobs.
//
// The manifest is published last with an atomic rename, so commit is
// all-or-nothing from a reader's point of view.
func (r *submoduleRoster) commit() error {
	identities := make([]string, 0, len(r.desired))
	for identity := range r.desired {
		identities = append(identities, identity)
	}
	sort.Strings(identities)

	manifest := submoduleRosterManifest{
		Version:    submoduleRosterVersion,
		Parent:     submoduleRosterParent{Source: r.source, Name: r.name},
		Submodules: make([]*submoduleRosterEntry, 0, len(identities)),
	}
	referenced := map[string]struct{}{}

	for _, identity := range identities {
		desired := r.desired[identity]
		entry := desired

		if snapshot, ok := r.fresh[identity]; ok {
			data := marshalSubmoduleSnapshot(snapshot)
			sum := sha256.Sum256(data)
			blobName := hex.EncodeToString(sum[:]) + ".bin"
			if _, err := os.Stat(filepath.Join(r.dir, blobName)); errors.Is(err, fs.ErrNotExist) {
				if err := writeRosterFileAtomic(r.dir, blobName, data, 0o600); err != nil {
					return err
				}
			} else if err != nil {
				return fmt.Errorf("stat snapshot blob %s: %w", blobName, err)
			}
			entry.Snapshot = blobName
			entry.Files = snapshotFileMetadata(snapshot)
		} else {
			// Reused identity: its content must come from the previous
			// commit. Missing or unreadable rescue state means the run was
			// not actually complete, so refuse to commit.
			old := r.prev[identity]
			if old == nil {
				return fmt.Errorf("internal error: submodule %q reused without saved content", identity)
			}
			if _, err := os.Stat(filepath.Join(r.dir, old.Snapshot)); err != nil {
				return fmt.Errorf("saved content for reused submodule %q is gone: %w", identity, err)
			}
			entry.Snapshot = old.Snapshot
			entry.Files = old.Files
		}

		manifest.Submodules = append(manifest.Submodules, &entry)
		referenced[entry.Snapshot] = struct{}{}
	}

	data, err := json.MarshalIndent(&manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal submodule roster manifest: %w", err)
	}
	data = append(data, '\n')
	if err := writeRosterFileAtomic(r.dir, submoduleRosterManifestName, data, 0o600); err != nil {
		return err
	}

	// Retire blobs that the new manifest no longer references. Failure here
	// only leaks disk space; the next load reconciles again, so do not fail
	// the commit.
	return r.garbageCollect(referenced)
}

// garbageCollect deletes staged temporary files and snapshot blobs that no
// manifest entry references.
func (r *submoduleRoster) garbageCollect(referenced map[string]struct{}) error {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return fmt.Errorf("read submodule roster directory %s: %w", r.dir, err)
	}
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		switch {
		case name == submoduleRosterManifestName:
			continue
		case isRosterTempFile(name):
			if err := os.Remove(filepath.Join(r.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		case isSnapshotBlob(name):
			if _, ok := referenced[name]; ok {
				continue
			}
			if err := os.Remove(filepath.Join(r.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func isSnapshotBlob(name string) bool {
	if len(name) != sha256.Size*2+len(".bin") || !strings.HasSuffix(name, ".bin") {
		return false
	}
	_, err := hex.DecodeString(name[:len(name)-len(".bin")])
	return err == nil
}

func isRosterTempFile(name string) bool {
	return strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp")
}

// writeRosterFileAtomic writes data to dir/name via a same-directory temporary
// file and an atomic rename. The temp name is hidden and suffixed .tmp so the
// reconciler can sweep it if the process dies before the rename.
func writeRosterFileAtomic(dir, name string, data []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(dir, "."+name+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", name, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("flush %s: %w", name, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("chmod %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		cleanup()
		return fmt.Errorf("publish %s: %w", name, err)
	}
	return nil
}

// snapshotFileMetadata builds the integrity records stored alongside a
// snapshot blob.
func snapshotFileMetadata(snapshot *submoduleSnapshot) []submoduleRosterFile {
	files := make([]submoduleRosterFile, 0, len(snapshot.files))
	for _, file := range snapshot.files {
		meta := submoduleRosterFile{
			Sub:     file.sub,
			Path:    file.path,
			Size:    int64(len(file.content)),
			Missing: file.missing,
		}
		if !file.missing {
			sum := sha256.Sum256(file.content)
			meta.Hash = hex.EncodeToString(sum[:])
		}
		files = append(files, meta)
	}
	return files
}

// captureSubmoduleSnapshot turns a collected submodule file map into a
// storable snapshot.
func captureSubmoduleSnapshot(files map[fileKey]BlobLocation) (*submoduleSnapshot, error) {
	keys := make([]fileKey, 0, len(files))
	for key := range files {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].SubRepoPath != keys[j].SubRepoPath {
			return keys[i].SubRepoPath < keys[j].SubRepoPath
		}
		if keys[i].Path != keys[j].Path {
			return keys[i].Path < keys[j].Path
		}
		return keys[i].ID.String() < keys[j].ID.String()
	})

	snapshot := &submoduleSnapshot{files: make([]submoduleSnapshotFile, 0, len(keys))}
	for _, key := range keys {
		location := files[key]
		content, missing, err := resolveBlobContent(location, key.ID)
		if err != nil {
			return nil, fmt.Errorf("read submodule file %s: %w", key.FullPath(), err)
		}
		snapshot.files = append(snapshot.files, submoduleSnapshotFile{
			sub:     key.SubRepoPath,
			path:    key.Path,
			content: content,
			missing: missing,
		})
	}
	return snapshot, nil
}

// resolveBlobContent reads a blob either from inline roster content or from
// the backing Git repository. A blob filtered out by the repository is
// reported as missing instead of an error, matching createDocument.
func resolveBlobContent(location BlobLocation, id plumbing.Hash) (content []byte, missing bool, err error) {
	if location.inlineMissing {
		return nil, true, nil
	}
	if location.inline != nil {
		return location.inline, false, nil
	}
	blob, err := location.GitRepo.BlobObject(id)
	if err != nil {
		if errors.Is(err, plumbing.ErrObjectNotFound) {
			return nil, true, nil
		}
		return nil, false, err
	}
	content, err = blobContents(blob)
	if err != nil {
		return nil, false, err
	}
	return content, false, nil
}

// injectSubmoduleSnapshot restores snapshot files into a collected file map,
// prefixing every file with the submodule's current path in the parent.
// Injecting repeatedly (once per indexed branch) accumulates branch
// membership on the restored documents.
func injectSubmoduleSnapshot(files map[fileKey]BlobLocation, parentPath string, fileURL *url.URL, branches []string, snapshot *submoduleSnapshot) {
	for _, file := range snapshot.files {
		key := fileKey{
			SubRepoPath: filepath.Join(parentPath, file.sub),
			Path:        file.path,
			ID:          submoduleBlobHash(file.content, file.missing),
		}
		if location, exists := files[key]; exists {
			for _, branch := range branches {
				if !containsString(location.Branches, branch) {
					location.Branches = append(location.Branches, branch)
					files[key] = location
				}
			}
			continue
		}
		files[key] = BlobLocation{
			URL:           fileURL,
			inline:        file.content,
			inlineMissing: file.missing,
			Branches:      append([]string(nil), branches...),
		}
	}
}

func submoduleBlobHash(content []byte, missing bool) plumbing.Hash {
	if missing {
		return plumbing.ZeroHash
	}
	return plumbing.ComputeHash(plumbing.BlobObject, content)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// marshalSubmoduleSnapshot serializes a snapshot using a compact, length
// prefixed binary format:
//
//	magic, u32 file count,
//	per file: u8 flags, u32 sub length, sub, u32 path length, path,
//	          (u64 content length, content) unless missing.
func marshalSubmoduleSnapshot(snapshot *submoduleSnapshot) []byte {
	buf := bytes.NewBuffer(nil)
	buf.WriteString(submoduleSnapshotMagic)
	writeUint32(buf, uint32(len(snapshot.files)))
	for _, file := range snapshot.files {
		var flags uint8
		if file.missing {
			flags |= submoduleSnapshotFlagMissing
		}
		buf.WriteByte(flags)
		writeBytes32(buf, []byte(file.sub))
		writeBytes32(buf, []byte(file.path))
		if !file.missing {
			_ = binary.Write(buf, binary.LittleEndian, uint64(len(file.content)))
			buf.Write(file.content)
		}
	}
	return buf.Bytes()
}

// unmarshalSubmoduleSnapshot parses the format produced by
// marshalSubmoduleSnapshot and rejects truncated or malformed input.
func unmarshalSubmoduleSnapshot(data []byte) (*submoduleSnapshot, error) {
	if !bytes.HasPrefix(data, []byte(submoduleSnapshotMagic)) {
		return nil, errors.New("unrecognized snapshot format")
	}
	reader := bytes.NewReader(data[len(submoduleSnapshotMagic):])

	var count32 uint32
	if err := binary.Read(reader, binary.LittleEndian, &count32); err != nil {
		return nil, fmt.Errorf("read file count: %w", err)
	}
	if uint64(count32) > uint64(reader.Len()) {
		return nil, errors.New("implausible file count")
	}

	snapshot := &submoduleSnapshot{files: make([]submoduleSnapshotFile, 0, count32)}
	for i := uint32(0); i < count32; i++ {
		flags, err := reader.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("read file %d flags: %w", i, err)
		}
		sub, err := readBytes32(reader)
		if err != nil {
			return nil, fmt.Errorf("read file %d subrepository path: %w", i, err)
		}
		path, err := readBytes32(reader)
		if err != nil {
			return nil, fmt.Errorf("read file %d path: %w", i, err)
		}
		file := submoduleSnapshotFile{
			sub:     string(sub),
			path:    string(path),
			missing: flags&submoduleSnapshotFlagMissing != 0,
		}
		if flags&^submoduleSnapshotFlagMissing != 0 {
			return nil, fmt.Errorf("file %d uses unknown snapshot flags: %#x", i, flags)
		}
		if !file.missing {
			var size64 uint64
			if err := binary.Read(reader, binary.LittleEndian, &size64); err != nil {
				return nil, fmt.Errorf("read file %d content length: %w", i, err)
			}
			if size64 > uint64(reader.Len()) {
				return nil, fmt.Errorf("file %d claims %d bytes but only %d remain", i, size64, reader.Len())
			}
			file.content = make([]byte, size64)
			if _, err := reader.Read(file.content); err != nil {
				return nil, fmt.Errorf("read file %d content: %w", i, err)
			}
		}
		snapshot.files = append(snapshot.files, file)
	}
	if reader.Len() != 0 {
		return nil, fmt.Errorf("%d trailing bytes in snapshot", reader.Len())
	}
	return snapshot, nil
}

func writeUint32(buf *bytes.Buffer, value uint32) {
	_ = binary.Write(buf, binary.LittleEndian, value)
}

func writeBytes32(buf *bytes.Buffer, value []byte) {
	writeUint32(buf, uint32(len(value)))
	buf.Write(value)
}

func readBytes32(reader *bytes.Reader) ([]byte, error) {
	var length uint32
	if err := binary.Read(reader, binary.LittleEndian, &length); err != nil {
		return nil, err
	}
	if uint64(length) > uint64(reader.Len()) {
		return nil, fmt.Errorf("length %d exceeds %d remaining bytes", length, reader.Len())
	}
	value := make([]byte, length)
	if _, err := reader.Read(value); err != nil {
		return nil, err
	}
	return value, nil
}
