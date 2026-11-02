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

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/gitindex"
	"github.com/sourcegraph/zoekt/index"
)

// Staging layout:
//
//	<indexDir>/<name>_v<ver>.<n>.zoekt            live, searchable shards
//	<indexDir>/.zoekt-local-sync.staging/<txn>/    one replacement attempt
//	  txn.json                                    durable replacement record
//	  <name>_v<ver>.<n>.zoekt[.meta]              verified new generation
//	  old/<...>                                   retired live generation
//
// The staging directory is invisible to the shard inventory and to the
// zoekt shard watcher, which only globs *.zoekt directly inside the index
// directory. Live files are therefore either entirely the old generation
// or entirely the new one: the two never appear in search results together.

const (
	stagingDirName = ".zoekt-local-sync.staging"
	journalName    = "txn.json"
	journalVersion = 1
	retiredDirName = "old"
)

// replacementJournal is the durable record of a single cache replacement. It
// is written and fsynced before any live file is touched, so an interrupted
// sync can always decide whether to finish publishing the new generation or
// to restore the previous one.
type replacementJournal struct {
	Version int `json:"version"`

	Name      string    `json:"name"`
	ID        uint32    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	// NewSource is the normalized source of the staged generation.
	NewSource string `json:"new_source"`
	// OldSources are the normalized sources that were live for this
	// repository identity when the replacement started. There is normally
	// one; several entries mean the run was also healing a mixed state.
	OldSources []string `json:"old_sources"`

	// OldFiles are live basenames retired by this replacement.
	OldFiles []string `json:"old_files"`
	// NewFiles are staged basenames published by this replacement. Empty for
	// a retire-only replacement that adopts an already complete live group.
	NewFiles []string `json:"new_files"`
	// KeepFiles anchors a retire-only replacement: they must stay live and
	// verify against NewSource for the replacement to count as finished.
	KeepFiles []string `json:"keep_files"`
}

// replacement is one in-progress cache replacement.
type replacement struct {
	indexDir string
	dir      string
	oldDir   string
	journal  replacementJournal
}

func newReplacement(indexDir string) (*replacement, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, fmt.Errorf("generate replacement id: %w", err)
	}
	name := fmt.Sprintf("txn-%d-%s", time.Now().UnixNano(), hex.EncodeToString(random[:]))
	dir := filepath.Join(indexDir, stagingDirName, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create staging directory %s: %w", dir, err)
	}
	return &replacement{
		indexDir: indexDir,
		dir:      dir,
		oldDir:   filepath.Join(dir, retiredDirName),
	}, nil
}

// shardNumberRegexp matches shard basenames produced by index.shardName,
// e.g. "team%2Frepo_v17.00003.zoekt".
var shardNumberRegexp = regexp.MustCompile(`_v(\d+)\.(\d{5})\.zoekt$`)

type shardSetInfo struct {
	Name     string
	Source   string
	ID       uint32
	Shards   []string
	Branches []zoekt.RepositoryBranch
}

// verifyShardSet verifies a complete generation in dir and returns its
// identity and every artifact basename (shards plus .meta sidecars).
func verifyShardSet(dir, wantName, wantSource string) (*shardSetInfo, []string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("read staged index directory %s: %w", dir, err)
	}

	var shards, sidecars []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			if name == retiredDirName {
				continue
			}
			return nil, nil, fmt.Errorf("unexpected directory %s in staged index", filepath.Join(dir, name))
		}
		// The journal (and its temp files) lives alongside the staged shards.
		if name == journalName || strings.HasPrefix(name, ".txn-") {
			continue
		}
		switch {
		case strings.HasSuffix(name, ".zoekt"):
			shards = append(shards, name)
		case strings.HasSuffix(name, ".zoekt.meta"):
			sidecars = append(sidecars, name)
		default:
			// Leftover *.tmp files or anything else mean the build did not
			// finish cleanly: the staged cache is not complete.
			return nil, nil, fmt.Errorf("unexpected file %s in staged index", filepath.Join(dir, name))
		}
	}
	if len(shards) == 0 {
		return nil, nil, fmt.Errorf("staged index in %s contains no shards", dir)
	}
	sort.Strings(shards)
	sort.Strings(sidecars)

	type shardNumber struct {
		version int
		number  int
	}
	numbers := make(map[string]shardNumber, len(shards))
	version := -1
	for _, name := range shards {
		match := shardNumberRegexp.FindStringSubmatch(name)
		if match == nil {
			return nil, nil, fmt.Errorf("staged shard %s has an unexpected name", filepath.Join(dir, name))
		}
		var v, n int
		fmt.Sscanf(match[1], "%d", &v)
		fmt.Sscanf(match[2], "%d", &n)
		if version == -1 {
			version = v
		} else if version != v {
			return nil, nil, fmt.Errorf("staged shards mix index versions %d and %d", version, v)
		}
		numbers[name] = shardNumber{version: v, number: n}
	}
	for i, name := range shards {
		if numbers[name].number != i {
			return nil, nil, fmt.Errorf("staged shard set is not contiguous, expected shard number %d next", i)
		}
	}

	sidecarSet := map[string]struct{}{}
	for _, name := range sidecars {
		shard := strings.TrimSuffix(name, ".meta")
		if _, ok := numbers[shard]; !ok {
			return nil, nil, fmt.Errorf("metadata sidecar %s has no matching staged shard", name)
		}
		sidecarSet[shard] = struct{}{}
	}

	var info *shardSetInfo
	for _, name := range shards {
		path := filepath.Join(dir, name)
		repositories, _, err := index.ReadMetadataPathAlive(path)
		if err != nil {
			return nil, nil, fmt.Errorf("read staged shard %s: %w", path, err)
		}
		if len(repositories) != 1 {
			return nil, nil, fmt.Errorf("staged shard %s contains %d repositories, want 1", path, len(repositories))
		}
		repo := repositories[0]
		if repo.Name != wantName {
			return nil, nil, fmt.Errorf("staged shard %s names repository %q, want %q", path, repo.Name, wantName)
		}
		if got := normalizeSource(repo.Source); got != wantSource {
			return nil, nil, fmt.Errorf("staged shard %s sources repository at %s, want %s", path, got, wantSource)
		}
		if len(repo.Branches) == 0 {
			return nil, nil, fmt.Errorf("staged shard %s contains no indexed branches", path)
		}
		if info == nil {
			info = &shardSetInfo{
				Name:     repo.Name,
				Source:   normalizeSource(repo.Source),
				ID:       repo.ID,
				Shards:   append([]string(nil), shards...),
				Branches: append([]zoekt.RepositoryBranch(nil), repo.Branches...),
			}
		} else {
			if repo.ID != info.ID {
				return nil, nil, fmt.Errorf("staged shards disagree on repository ID: %d and %d", repo.ID, info.ID)
			}
			if !branchesEqual(repo.Branches, info.Branches) {
				return nil, nil, fmt.Errorf("staged shards disagree on indexed branches")
			}
		}
	}

	artifacts := make([]string, 0, len(shards)+len(sidecars))
	artifacts = append(artifacts, shards...)
	artifacts = append(artifacts, sidecars...)
	sort.Strings(artifacts)
	return info, artifacts, nil
}

func branchesEqual(a, b []zoekt.RepositoryBranch) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// build indexes spec.Source into the private staging directory. Nothing in
// the live index directory is touched.
func (r *replacement) build(spec repositorySpec, opts gitindex.Options) error {
	buildOpts := opts
	buildOpts.RepoDir = spec.Source
	buildOpts.DryRun = false
	// Staging always starts empty, so a full normal build is required; an
	// incremental build would incorrectly compare against missing shards.
	buildOpts.Incremental = false
	buildOpts.BuildOptions = opts.BuildOptions
	buildOpts.BuildOptions.IndexDir = r.dir
	buildOpts.BuildOptions.RepositoryDescription = zoekt.Repository{Name: spec.Name}

	updated, err := gitindex.IndexGitRepo(buildOpts)
	if err != nil {
		return fmt.Errorf("index %q from %s: %w", spec.Name, spec.Source, err)
	}
	if !updated {
		return fmt.Errorf("index %q from %s produced no shards", spec.Name, spec.Source)
	}

	info, artifacts, err := verifyShardSet(r.dir, spec.Name, normalizeSource(spec.Source))
	if err != nil {
		return err
	}
	r.journal.Name = spec.Name
	r.journal.ID = info.ID
	r.journal.NewSource = normalizeSource(spec.Source)
	r.journal.NewFiles = artifacts
	return nil
}

// writeJournal persists the replacement record durably.
func (r *replacement) writeJournal() error {
	r.journal.Version = journalVersion
	if r.journal.CreatedAt.IsZero() {
		r.journal.CreatedAt = time.Now()
	}
	data, err := json.MarshalIndent(&r.journal, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal replacement journal: %w", err)
	}
	tmp, err := os.CreateTemp(r.dir, ".txn-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create replacement journal: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write replacement journal: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync replacement journal: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close replacement journal: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(r.dir, journalName)); err != nil {
		return fmt.Errorf("publish replacement journal: %w", err)
	}
	fsyncDir(r.dir)
	return nil
}

func fsyncDir(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	_ = f.Sync()
	_ = f.Close()
}

// fileExists reports whether a file is currently present at path.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// checkLiveShard reads a live shard and confirms it carries this
// replacement's repository identity and an expected source. Old and new
// generations can share the same shard name, so the source is matched
// against the recorded old sources and, when the same source is being
// refreshed, the new source.
func (r *replacement) checkLiveShard(basename string, requireNew bool) error {
	repositories, _, err := index.ReadMetadataPathAlive(filepath.Join(r.indexDir, basename))
	if err != nil {
		return fmt.Errorf("read live shard %s: %w", basename, err)
	}
	if len(repositories) != 1 {
		return fmt.Errorf("conflict: live shard %s contains %d repositories, want 1", basename, len(repositories))
	}
	repo := repositories[0]
	if repo.Name != r.journal.Name {
		return fmt.Errorf("conflict: live shard %s belongs to %q, not %q", basename, repo.Name, r.journal.Name)
	}
	source := normalizeSource(repo.Source)
	if requireNew {
		if source != r.journal.NewSource {
			return fmt.Errorf("conflict: live shard %s sources %s, want %s", basename, source, r.journal.NewSource)
		}
		return nil
	}
	if source == r.journal.NewSource {
		return nil
	}
	for _, old := range r.journal.OldSources {
		if source == old {
			return nil
		}
	}
	return fmt.Errorf("conflict: live shard %s source %s is not part of replacement for %q", basename, source, r.journal.Name)
}

// fence makes sure the live generation has not been replaced by an
// unrelated process after planning. It must run before any live file moves.
func (r *replacement) fence() error {
	for _, basename := range r.journal.OldFiles {
		if !strings.HasSuffix(basename, ".zoekt") {
			continue
		}
		if !fileExists(filepath.Join(r.indexDir, basename)) {
			return fmt.Errorf("conflict: live shard %s disappeared before replacement of %q committed", basename, r.journal.Name)
		}
		if err := r.checkLiveShard(basename, false); err != nil {
			return err
		}
	}
	return nil
}

// commit retires the live generation, publishes the staged generation, and
// verifies the result. Any error rolls back to the previous live state.
func (r *replacement) commit(out io.Writer) error {
	if err := r.writeJournal(); err != nil {
		return err
	}
	if err := r.fence(); err != nil {
		_ = r.discard()
		return err
	}
	if err := r.publish(); err != nil {
		if rbErr := r.rollback(); rbErr != nil {
			return fmt.Errorf("replace %q failed (%v); rollback also failed: %w", r.journal.Name, err, rbErr)
		}
		fmt.Fprintf(out, "Replacement for %q failed; kept the previous searchable cache\n", r.journal.Name)
		return err
	}
	if err := r.discard(); err != nil {
		// The swap itself is complete and durable; a stale staging directory
		// only costs disk and is cleaned by the next recovery run.
		fmt.Fprintf(out, "Replacement for %q committed, but cleanup of %s failed: %v\n", r.journal.Name, r.dir, err)
	}
	return nil
}

func (r *replacement) livePath(basename string) string { return filepath.Join(r.indexDir, basename) }
func (r *replacement) stagedPath(b string) string      { return filepath.Join(r.dir, b) }
func (r *replacement) retiredPath(b string) string     { return filepath.Join(r.oldDir, b) }

// newFileSet reports which artifact basenames belong to the new generation.
func (r *replacement) newFileSet() map[string]struct{} {
	set := make(map[string]struct{}, len(r.journal.NewFiles))
	for _, b := range r.journal.NewFiles {
		set[b] = struct{}{}
	}
	return set
}

// publish performs the ordered rename swap. It is idempotent so recovery can
// resume an interrupted publish. Generation is decided by file position: a
// file still in staging has not been published yet, so any same-named live
// file is the old generation and must be retired.
func (r *replacement) publish() error {
	newFiles := r.newFileSet()
	if len(r.journal.OldFiles) > 0 {
		if err := os.MkdirAll(r.oldDir, 0o700); err != nil {
			return fmt.Errorf("create retirement directory: %w", err)
		}
	}

	// 1. Retire every live file of the old generation.
	for _, basename := range r.journal.OldFiles {
		livePath := r.livePath(basename)
		_, liveErr := os.Stat(livePath)
		if liveErr != nil {
			if !errors.Is(liveErr, fs.ErrNotExist) {
				return liveErr
			}
			if fileExists(r.retiredPath(basename)) {
				continue // already retired
			}
			if _, isNew := newFiles[basename]; isNew && !fileExists(r.stagedPath(basename)) {
				// The new generation already occupies this name.
				continue
			}
			return fmt.Errorf("live file %s vanished during replacement of %q", basename, r.journal.Name)
		}

		if _, isNew := newFiles[basename]; isNew && !fileExists(r.stagedPath(basename)) {
			// New file was already moved into this name; confirm the live
			// occupant really is the new generation before keeping it.
			if strings.HasSuffix(basename, ".zoekt") {
				if err := r.checkLiveShard(basename, true); err != nil {
					return err
				}
			}
			continue
		}
		if strings.HasSuffix(basename, ".zoekt") {
			if err := r.checkLiveShard(basename, false); err != nil {
				return err
			}
		}
		if fileExists(r.retiredPath(basename)) {
			return fmt.Errorf("conflict: %s exists both live and retired for %q", basename, r.journal.Name)
		}
		if err := os.Rename(livePath, r.retiredPath(basename)); err != nil {
			return fmt.Errorf("retire %s: %w", basename, err)
		}
	}

	// 2. Publish the new generation.
	for _, basename := range r.journal.NewFiles {
		livePath := r.livePath(basename)
		if fileExists(livePath) {
			// Already published on a resumed run.
			if strings.HasSuffix(basename, ".zoekt") {
				if err := r.checkLiveShard(basename, true); err != nil {
					return err
				}
			}
			continue
		}
		stagedPath := r.stagedPath(basename)
		if !fileExists(stagedPath) {
			return fmt.Errorf("staged file %s is missing during publish", basename)
		}
		if err := os.Rename(stagedPath, livePath); err != nil {
			return fmt.Errorf("publish %s: %w", basename, err)
		}
	}
	fsyncDir(r.indexDir)

	// 3. Verify the live directory now holds exactly the new generation.
	return r.verifyLive()
}

// verifyLive confirms the committed state in the live directory.
func (r *replacement) verifyLive() error {
	published := r.journal.NewFiles
	if len(published) == 0 {
		published = r.journal.KeepFiles
	}
	if len(published) == 0 {
		return fmt.Errorf("replacement for %q has no files to verify", r.journal.Name)
	}

	shards := map[string]struct{}{}
	for _, basename := range published {
		path := filepath.Join(r.indexDir, basename)
		if !strings.HasSuffix(basename, ".zoekt") {
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("committed sidecar %s is missing: %w", basename, err)
			}
			continue
		}
		repositories, _, err := index.ReadMetadataPathAlive(path)
		if err != nil {
			return fmt.Errorf("committed shard %s cannot be read: %w", path, err)
		}
		if len(repositories) != 1 || repositories[0].Name != r.journal.Name || normalizeSource(repositories[0].Source) != r.journal.NewSource {
			return fmt.Errorf("committed shard %s does not match repository %q at %s", path, r.journal.Name, r.journal.NewSource)
		}
		shards[basename] = struct{}{}
	}

	// No retired file may remain live, and the live directory must not carry
	// any extra shard for this identity (e.g. an older format version).
	for _, basename := range r.journal.OldFiles {
		if _, err := os.Stat(filepath.Join(r.indexDir, basename)); err == nil {
			return fmt.Errorf("old shard file %s remains live after replacement of %q", basename, r.journal.Name)
		}
	}
	prefix := url.QueryEscape(r.journal.Name)
	extra, err := filepath.Glob(filepath.Join(r.indexDir, prefix+"_v*.zoekt*"))
	if err != nil {
		return err
	}
	expected := map[string]struct{}{}
	for _, basename := range published {
		expected[basename] = struct{}{}
	}
	for _, path := range extra {
		if _, ok := expected[filepath.Base(path)]; !ok {
			return fmt.Errorf("unexpected shard %s remains live after replacement of %q", path, r.journal.Name)
		}
	}
	return nil
}

// rollback restores the previous live generation and discards the staged
// attempt. It is used when a replacement fails or cannot be finished. As in
// publish, whether a live file is a published new one is decided by position:
// its staged copy is gone exactly when it was published.
func (r *replacement) rollback() error {
	// 1. Move any already published new files back into staging.
	for _, basename := range r.journal.NewFiles {
		livePath := r.livePath(basename)
		if !fileExists(livePath) {
			continue
		}
		stagedPath := r.stagedPath(basename)
		if fileExists(stagedPath) {
			// Staged and live copies coexist: the live occupant is the
			// unpublishable old generation, leave it for the restore step.
			continue
		}
		if strings.HasSuffix(basename, ".zoekt") {
			if err := r.checkLiveShard(basename, true); err != nil {
				return err
			}
		}
		if err := os.Rename(livePath, stagedPath); err != nil {
			return fmt.Errorf("roll back published file %s: %w", basename, err)
		}
	}

	// 2. Restore the retired generation.
	var missing []string
	for _, basename := range r.journal.OldFiles {
		livePath := r.livePath(basename)
		if fileExists(livePath) {
			continue // never retired, or an old occupant left in place
		}
		if err := os.Rename(r.retiredPath(basename), livePath); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				missing = append(missing, basename)
				continue
			}
			return fmt.Errorf("restore %s: %w", basename, err)
		}
	}
	fsyncDir(r.indexDir)

	if len(missing) > 0 {
		return fmt.Errorf("cannot restore previous cache for %q, files lost: %s", r.journal.Name, strings.Join(missing, ", "))
	}
	// 3. Confirm every restored shard carries the recorded identity.
	for _, basename := range r.journal.OldFiles {
		if !strings.HasSuffix(basename, ".zoekt") {
			continue
		}
		repositories, _, err := index.ReadMetadataPathAlive(r.livePath(basename))
		if err != nil {
			return fmt.Errorf("restored shard %s cannot be read: %w", basename, err)
		}
		if len(repositories) != 1 || repositories[0].Name != r.journal.Name {
			return fmt.Errorf("restored shard %s does not belong to %q", basename, r.journal.Name)
		}
		source := normalizeSource(repositories[0].Source)
		matched := false
		for _, old := range r.journal.OldSources {
			if source == old {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("restored shard %s for %q carries unexpected source %s", basename, r.journal.Name, source)
		}
	}
	return r.discard()
}

// discard removes the staging directory of a finished/failed attempt.
func (r *replacement) discard() error {
	if err := os.RemoveAll(r.dir); err != nil {
		return err
	}
	fsyncDir(filepath.Dir(r.dir))
	return nil
}

// recoverTransactions resumes or rolls back replacements left behind by a
// killed or crashed sync process. It must be called while the index
// directory lock is held.
func recoverTransactions(indexDir string, out io.Writer) error {
	root := filepath.Join(indexDir, stagingDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read staging directory %s: %w", root, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, entry := range entries {
		dir := filepath.Join(root, entry.Name())
		if !entry.IsDir() {
			if err := os.Remove(dir); err != nil {
				return fmt.Errorf("remove stray staging file %s: %w", dir, err)
			}
			continue
		}
		journalPath := filepath.Join(dir, journalName)
		data, err := os.ReadFile(journalPath)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Created before the journal was persisted; no live file can
				// have been moved yet, so it is safe to delete.
				if err := os.RemoveAll(dir); err != nil {
					return fmt.Errorf("remove incomplete staging directory %s: %w", dir, err)
				}
				continue
			}
			return fmt.Errorf("read replacement journal %s: %w", journalPath, err)
		}
		var journal replacementJournal
		if err := json.Unmarshal(data, &journal); err != nil {
			return fmt.Errorf("parse replacement journal %s: %w", journalPath, err)
		}
		if journal.Version != journalVersion || journal.Name == "" {
			return fmt.Errorf("replacement journal %s is unsupported", journalPath)
		}
		r := &replacement{
			indexDir: indexDir,
			dir:      dir,
			oldDir:   filepath.Join(dir, retiredDirName),
			journal:  journal,
		}
		if err := r.recover(out); err != nil {
			return err
		}
	}

	// Remove the root when it is empty; a non-empty root (for example a
	// directory another tool created) is left for the next recovery run.
	if entries, _ := os.ReadDir(root); len(entries) == 0 {
		if err := os.Remove(root); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove staging directory %s: %w", root, err)
		}
	}
	return nil
}

// recover finishes or undoes one interrupted replacement.
func (r *replacement) recover(out io.Writer) error {
	newStaged := true
	if len(r.journal.NewFiles) > 0 {
		if info, artifacts, err := verifyShardSet(r.dir, r.journal.Name, r.journal.NewSource); err != nil {
			newStaged = false
		} else if info.ID != r.journal.ID {
			newStaged = false
		} else {
			staged := map[string]struct{}{}
			for _, a := range artifacts {
				staged[a] = struct{}{}
			}
			for _, expected := range r.journal.NewFiles {
				if _, ok := staged[expected]; !ok {
					newStaged = false
					break
				}
			}
		}
	}

	rollbackAndReport := func(reason error) error {
		if err := r.rollback(); err != nil {
			return fmt.Errorf("interrupted replacement for %q cannot be recovered (%v); rollback failed: %w", r.journal.Name, reason, err)
		}
		fmt.Fprintf(out, "Recovered interrupted replacement for %q by restoring the previous cache\n", r.journal.Name)
		return nil
	}

	if !newStaged {
		if len(r.journal.OldFiles) == 0 {
			// Nothing searchable existed before; just discard the partial try.
			if err := r.discard(); err != nil {
				return err
			}
			fmt.Fprintf(out, "Discarded incomplete initial index build for %q\n", r.journal.Name)
			return nil
		}
		return rollbackAndReport(fmt.Errorf("staged cache for %q is incomplete", r.journal.Name))
	}

	// Staged generation is complete. Verify whether publishing already
	// finished; otherwise resume the idempotent publish.
	if err := r.verifyLive(); err == nil {
		if err := r.discard(); err != nil {
			return err
		}
		fmt.Fprintf(out, "Recovered interrupted replacement for %q: new cache was already live\n", r.journal.Name)
		return nil
	}
	if err := r.publish(); err != nil {
		return rollbackAndReport(err)
	}
	if err := r.discard(); err != nil {
		return err
	}
	fmt.Fprintf(out, "Recovered interrupted replacement for %q by finishing the switch to %s\n", r.journal.Name, r.journal.NewSource)
	return nil
}
