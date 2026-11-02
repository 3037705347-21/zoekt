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
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/gitindex"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/internal/testutil"
	"github.com/sourcegraph/zoekt/query"
	"github.com/sourcegraph/zoekt/search"
)

func syncArgs(indexDir, root string, extra ...string) []string {
	args := []string{"-index", indexDir, "-disable_ctags", "-submodules=false"}
	args = append(args, extra...)
	args = append(args, root)
	return args
}

func liveShardFiles(t *testing.T, indexDir string) []string {
	t.Helper()
	shards, err := readInventory(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, shard := range shards {
		paths = append(paths, shard.Path)
	}
	sort.Strings(paths)
	return paths
}

// liveSources maps repository name to the distinct normalized sources still
// searchable in the live directory.
func liveSources(t *testing.T, indexDir string) map[string][]string {
	t.Helper()
	shards, err := readInventory(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]map[string]struct{}{}
	for _, shard := range shards {
		set := sources[shard.Repository.Name]
		if set == nil {
			set = map[string]struct{}{}
			sources[shard.Repository.Name] = set
		}
		set[normalizeSource(shard.Repository.Source)] = struct{}{}
	}
	out := map[string][]string{}
	for name, set := range sources {
		for source := range set {
			out[name] = append(out[name], source)
		}
		sort.Strings(out[name])
	}
	return out
}

func assertStagingEmpty(t *testing.T, indexDir string) {
	t.Helper()
	path := filepath.Join(indexDir, stagingDirName)
	if entries, err := os.ReadDir(path); err == nil && len(entries) > 0 {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("staging directory %s still contains %s", path, strings.Join(names, ", "))
	}
}

func initEmptyGitRepository(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, path, "init", "-b", "main")
}

func addCommit(t *testing.T, path, file, content, message string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(path, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, path, "add", ".")
	testutil.RunGit(t, path, "commit", "-m", message)
}

func testIndexOptions(indexDir string) gitindex.Options {
	build := index.Options{IndexDir: indexDir, DisableCTags: true}
	build.SetDefaults()
	return gitindex.Options{
		BuildOptions: build,
		Branches:     []string{"HEAD"},
		BranchPrefix: "refs/heads/",
		Submodules:   false,
	}
}

// buildShardInto indexes a real git repository into the given index
// directory and returns the produced artifact basenames.
func buildShardInto(t *testing.T, indexDir, name, source string) []string {
	t.Helper()
	opts := testIndexOptions(indexDir)
	opts.BuildOptions.RepositoryDescription = zoekt.Repository{Name: name}
	opts.RepoDir = source
	if _, err := gitindex.IndexGitRepo(opts); err != nil {
		t.Fatal(err)
	}
	shards, err := readInventory(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, shard := range shards {
		paths, err := index.IndexFilePaths(shard.Path)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			files = append(files, filepath.Base(path))
		}
	}
	sort.Strings(files)
	return files
}

func TestSyncMovesRepositoryToNewSource(t *testing.T) {
	oldRoot := t.TempDir()
	oldRepo := filepath.Join(oldRoot, "team", "repo")
	createGitRepository(t, oldRepo)
	indexDir := t.TempDir()

	if err := execute(syncArgs(indexDir, oldRoot, "-f"), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	resolvedOld, err := filepath.EvalSymlinks(oldRepo)
	if err != nil {
		t.Fatal(err)
	}
	wantOld := normalizeSource(resolvedOld)
	if got := liveSources(t, indexDir)["team/repo"]; len(got) != 1 || got[0] != wantOld {
		t.Fatalf("initial source = %v, want %s", got, wantOld)
	}
	oldFiles := liveShardFiles(t, indexDir)
	if len(oldFiles) != 1 {
		t.Fatalf("got %d live shards, want 1", len(oldFiles))
	}
	oldContent, err := os.ReadFile(oldFiles[0])
	if err != nil {
		t.Fatal(err)
	}

	// Move the mirror to a new address; search name and identity stay the
	// same because the relative path under the root is unchanged.
	newRoot := t.TempDir()
	newRepo := filepath.Join(newRoot, "team", "repo")
	if err := os.Rename(oldRepo, newRepo); err != nil {
		t.Fatal(err)
	}
	// The old address must not remain as a leftover directory entry.
	if _, err := os.Stat(oldRepo); !os.IsNotExist(err) {
		t.Fatalf("old repository still present: %v", err)
	}

	// Preview reports the replacement but changes nothing.
	var out bytes.Buffer
	if err := execute(syncArgs(indexDir, newRoot), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Would replace cached source") || !strings.Contains(out.String(), `Would index "team/repo"`) {
		t.Fatalf("preview did not announce the source replacement:\n%s", out.String())
	}
	if got := liveSources(t, indexDir)["team/repo"]; len(got) != 1 || got[0] != wantOld {
		t.Fatalf("preview changed live source: %v", got)
	}
	afterPreview, err := os.ReadFile(oldFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(oldContent, afterPreview) {
		t.Fatal("preview modified the live shard")
	}

	// The actual switch.
	out.Reset()
	if err := execute(syncArgs(indexDir, newRoot, "-f"), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Replacing cached source") || !strings.Contains(out.String(), `Indexed "team/repo"`) {
		t.Fatalf("force sync output did not describe the replacement:\n%s", out.String())
	}

	wantNew, err := filepath.EvalSymlinks(newRepo)
	if err != nil {
		t.Fatal(err)
	}
	wantNew = normalizeSource(wantNew)
	if got := liveSources(t, indexDir)["team/repo"]; len(got) != 1 || got[0] != wantNew {
		t.Fatalf("after move source = %v, want %s", got, wantNew)
	}
	if files := liveShardFiles(t, indexDir); len(files) != 1 {
		t.Fatalf("got %d live shards after move, want exactly 1", len(files))
	}
	assertStagingEmpty(t, indexDir)

	// The repository list reports the new source exactly once.
	out.Reset()
	if err := execute([]string{"list", "-index", indexDir}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "team/repo") != 1 {
		t.Fatalf("repository list contains the repository more than once:\n%s", out.String())
	}
	if !strings.Contains(out.String(), wantNew) || strings.Contains(out.String(), wantOld) {
		t.Fatalf("repository list does not point at the new source only:\n%s", out.String())
	}

	// Search itself loads exactly the new generation.
	searcher, err := search.NewDirectorySearcher(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	defer searcher.Close()
	q, err := query.Parse("local")
	if err != nil {
		t.Fatal(err)
	}
	result, err := searcher.Search(context.Background(), q, &zoekt.SearchOptions{MaxDocDisplayCount: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || result.Files[0].Repository != "team/repo" {
		t.Fatalf("search matched %v, want one file in team/repo", result.Files)
	}
	// The searcher only loads top-level *.zoekt files, whose sources were
	// asserted above, so one match here proves the old generation is absent.
	_ = wantNew
}

func TestSyncFailedMoveKeepsPreviousCache(t *testing.T) {
	oldRoot := t.TempDir()
	oldRepo := filepath.Join(oldRoot, "team", "repo")
	createGitRepository(t, oldRepo)
	indexDir := t.TempDir()

	if err := execute(syncArgs(indexDir, oldRoot, "-f"), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	oldFiles := liveShardFiles(t, indexDir)
	if len(oldFiles) != 1 {
		t.Fatalf("got %d old shards, want 1", len(oldFiles))
	}
	resolvedOld, err := filepath.EvalSymlinks(oldRepo)
	if err != nil {
		t.Fatal(err)
	}
	wantOld := normalizeSource(resolvedOld)

	// New address hosts an empty repository that cannot be indexed yet.
	newRoot := t.TempDir()
	newRepo := filepath.Join(newRoot, "team", "repo")
	initEmptyGitRepository(t, newRepo)

	err = execute(syncArgs(indexDir, newRoot, "-f"), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected indexing the empty repository to fail")
	}
	if !strings.Contains(err.Error(), `index "team/repo"`) {
		t.Fatalf("error does not identify the failed index: %v", err)
	}

	// The previous generation is still fully searchable.
	if got := liveSources(t, indexDir)["team/repo"]; len(got) != 1 || got[0] != wantOld {
		t.Fatalf("after failed move source = %v, want %s", got, wantOld)
	}
	if files := liveShardFiles(t, indexDir); len(files) != 1 || files[0] != oldFiles[0] {
		t.Fatalf("live shard set changed after failed move: %v", files)
	}
	if info, err := os.Stat(oldFiles[0]); err != nil || info.Size() == 0 {
		t.Fatalf("old shard was damaged: %v", err)
	}
	assertStagingEmpty(t, indexDir)

	// Once the mirror is usable, a retry converges on the new source.
	addCommit(t, newRepo, "README.md", "new mirror content\n", "mirror moved")
	if err := execute(syncArgs(indexDir, newRoot, "-f"), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	wantNew := normalizeSource(newRepo)
	if got := liveSources(t, indexDir)["team/repo"]; len(got) != 1 || got[0] != wantNew {
		t.Fatalf("after retry source = %v, want %s", got, wantNew)
	}
	assertStagingEmpty(t, indexDir)
}

func TestSyncIDConflictKeepsPreviousCache(t *testing.T) {
	indexDir := t.TempDir()
	oldSource := filepath.Join(t.TempDir(), "old", "repo")
	createTestShard(t, indexDir, "team/repo", oldSource)
	// Mark the existing shard with repository ID 42.
	shards, err := readInventory(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	oldShard := shards[0].Path

	root := t.TempDir()
	repo := filepath.Join(root, "team", "repo")
	createGitRepository(t, repo)
	testutil.RunGit(t, repo, "config", "zoekt.repoid", "43")

	err = execute(syncArgs(indexDir, root, "-f"), &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "changed ID from 42 to 43") {
		t.Fatalf("expected ID conflict error, got %v", err)
	}
	if got := liveSources(t, indexDir)["team/repo"]; len(got) != 1 || got[0] != normalizeSource(oldSource) {
		t.Fatalf("ID conflict changed live cache: %v", got)
	}
	if info, err := os.Stat(oldShard); err != nil || info.Size() == 0 {
		t.Fatalf("old shard was damaged by the conflict: %v", err)
	}
	assertStagingEmpty(t, indexDir)
}

func TestRecoveryFinishesInterruptedReplacement(t *testing.T) {
	for _, interruptAt := range []string{"journal", "retired", "mid-publish"} {
		t.Run(interruptAt, func(t *testing.T) {
			oldRoot := t.TempDir()
			oldRepo := filepath.Join(oldRoot, "team", "repo")
			createGitRepository(t, oldRepo)
			indexDir := t.TempDir()
			if err := execute(syncArgs(indexDir, oldRoot, "-f"), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			shards, err := readInventory(indexDir)
			if err != nil {
				t.Fatal(err)
			}
			oldGroup := groupShards(shards)
			if len(oldGroup) != 1 {
				t.Fatalf("got %d groups, want 1", len(oldGroup))
			}

			newRoot := t.TempDir()
			newRepo := filepath.Join(newRoot, "team", "repo")
			createGitRepository(t, newRepo)
			addCommit(t, newRepo, "NEWS.md", "new mirror commit\n", "post move")
			resolvedNew, err := filepath.EvalSymlinks(newRepo)
			if err != nil {
				t.Fatal(err)
			}

			r, err := newReplacement(indexDir)
			if err != nil {
				t.Fatal(err)
			}
			oldFiles, oldSources, _, err := groupArtifacts(oldGroup)
			if err != nil {
				t.Fatal(err)
			}
			r.journal.Name = "team/repo"
			r.journal.OldFiles = oldFiles
			r.journal.OldSources = oldSources
			spec := repositorySpec{Name: "team/repo", Source: resolvedNew}
			if err := r.build(spec, testIndexOptions(r.dir)); err != nil {
				t.Fatal(err)
			}
			if err := r.writeJournal(); err != nil {
				t.Fatal(err)
			}

			switch interruptAt {
			case "journal":
				// Crash after persisting the journal, before any live move.
			case "retired":
				// Crash after moving the old generation aside, before publish.
				if err := os.MkdirAll(r.oldDir, 0o700); err != nil {
					t.Fatal(err)
				}
				for _, file := range oldFiles {
					if err := os.Rename(filepath.Join(indexDir, file), filepath.Join(r.oldDir, file)); err != nil {
						t.Fatal(err)
					}
				}
			case "mid-publish":
				// Retire everything, then publish only the first staged file.
				if err := os.MkdirAll(r.oldDir, 0o700); err != nil {
					t.Fatal(err)
				}
				for _, file := range oldFiles {
					if err := os.Rename(filepath.Join(indexDir, file), filepath.Join(r.oldDir, file)); err != nil {
						t.Fatal(err)
					}
				}
				first := r.journal.NewFiles[0]
				if err := os.Rename(filepath.Join(r.dir, first), filepath.Join(indexDir, first)); err != nil {
					t.Fatal(err)
				}
			}

			var out bytes.Buffer
			if err := recoverTransactions(indexDir, &out); err != nil {
				t.Fatalf("recovery failed: %v", err)
			}
			if !strings.Contains(out.String(), "Recovered interrupted replacement") {
				t.Fatalf("recovery did not report progress:\n%s", out.String())
			}

			wantNew := normalizeSource(resolvedNew)
			if got := liveSources(t, indexDir)["team/repo"]; len(got) != 1 || got[0] != wantNew {
				t.Fatalf("after recovery source = %v, want %s", got, wantNew)
			}
			if files := liveShardFiles(t, indexDir); len(files) != len(r.journal.NewFiles) {
				t.Fatalf("got %d live shards after recovery, want %d", len(files), len(r.journal.NewFiles))
			}
			assertStagingEmpty(t, indexDir)

			// Search must serve the recovered new generation, including the
			// commit that only exists at the new address.
			searcher, err := search.NewDirectorySearcher(indexDir)
			if err != nil {
				t.Fatal(err)
			}
			defer searcher.Close()
			q, _ := query.Parse("commit")
			result, err := searcher.Search(context.Background(), q, &zoekt.SearchOptions{MaxDocDisplayCount: 10})
			if err != nil {
				t.Fatal(err)
			}
			if result.FileCount != 1 || result.Files[0].Repository != "team/repo" {
				t.Fatalf("search matched %v, want one file in the recovered repository", result.Files)
			}
		})
	}
}

func TestRecoveryRestoresPreviousCacheWhenStagingBroken(t *testing.T) {
	oldRoot := t.TempDir()
	oldRepo := filepath.Join(oldRoot, "team", "repo")
	createGitRepository(t, oldRepo)
	indexDir := t.TempDir()
	if err := execute(syncArgs(indexDir, oldRoot, "-f"), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	shards, _ := readInventory(indexDir)
	oldGroup := groupShards(shards)

	newRoot := t.TempDir()
	newRepo := filepath.Join(newRoot, "team", "repo")
	createGitRepository(t, newRepo)

	r, err := newReplacement(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	oldFiles, oldSources, _, err := groupArtifacts(oldGroup)
	if err != nil {
		t.Fatal(err)
	}
	r.journal.Name = "team/repo"
	r.journal.OldFiles = oldFiles
	r.journal.OldSources = oldSources
	spec := repositorySpec{Name: "team/repo", Source: newRepo}
	if err := r.build(spec, testIndexOptions(r.dir)); err != nil {
		t.Fatal(err)
	}
	if err := r.writeJournal(); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash after retiring live files, with the staged shard
	// corrupted on disk.
	if err := os.MkdirAll(r.oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, file := range oldFiles {
		if err := os.Rename(filepath.Join(indexDir, file), filepath.Join(r.oldDir, file)); err != nil {
			t.Fatal(err)
		}
	}
	stagedShard := filepath.Join(r.dir, r.journal.NewFiles[0])
	if err := os.WriteFile(stagedShard, []byte("not a zoekt shard"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := recoverTransactions(indexDir, &out); err != nil {
		t.Fatalf("recovery returned an error instead of rolling back: %v", err)
	}
	if !strings.Contains(out.String(), "restoring the previous cache") {
		t.Fatalf("recovery did not roll back:\n%s", out.String())
	}
	resolvedOld, err := filepath.EvalSymlinks(oldRepo)
	if err != nil {
		t.Fatal(err)
	}
	wantOld := normalizeSource(resolvedOld)
	if got := liveSources(t, indexDir)["team/repo"]; len(got) != 1 || got[0] != wantOld {
		t.Fatalf("after rollback source = %v, want %s", got, wantOld)
	}
	if files := liveShardFiles(t, indexDir); len(files) != len(oldFiles) {
		t.Fatalf("got %d restored shards, want %d", len(files), len(oldFiles))
	}
	assertStagingEmpty(t, indexDir)
}

func TestRecoveryRemovesOrphanedStagingWithoutJournal(t *testing.T) {
	indexDir := t.TempDir()
	orphan := filepath.Join(indexDir, stagingDirName, "txn-orphan")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "partial.zoekt.tmp"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := recoverTransactions(indexDir, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphaned staging directory remains: %v", err)
	}
}

func TestSyncConsolidatesMixedGenerations(t *testing.T) {
	oldRoot := t.TempDir()
	oldRepo := filepath.Join(oldRoot, "team", "repo")
	createGitRepository(t, oldRepo)
	newRoot := t.TempDir()
	newRepo := filepath.Join(newRoot, "team", "repo")
	createGitRepository(t, newRepo)
	indexDir := t.TempDir()

	// Publish the old generation live.
	if err := execute(syncArgs(indexDir, oldRoot, "-f"), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	liveShards, err := filepath.Glob(filepath.Join(indexDir, "*.zoekt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(liveShards) != 1 {
		t.Fatalf("got %d live shards, want 1", len(liveShards))
	}

	// Reproduce the mixed state an interrupted in-place rebuild can leave:
	// shard 0 is the new generation, shard 1 still carries the old source.
	resolvedNew, err := filepath.EvalSymlinks(newRepo)
	if err != nil {
		t.Fatal(err)
	}
	stagingNew := t.TempDir()
	newFiles := buildShardInto(t, stagingNew, "team/repo", resolvedNew)
	if len(newFiles) != 1 {
		t.Fatalf("unexpected new artifacts: %v", newFiles)
	}
	base := newFiles[0]
	extraShard := strings.TrimSuffix(base, ".00000.zoekt") + ".00001.zoekt"
	if err := os.Link(filepath.Join(indexDir, base), filepath.Join(indexDir, extraShard)); err != nil {
		t.Fatal(err)
	}
	// Replace shard 0 with the new source; shard 1 stays old.
	if err := os.Rename(filepath.Join(stagingNew, base), filepath.Join(indexDir, base)); err != nil {
		t.Fatal(err)
	}

	sources := liveSources(t, indexDir)["team/repo"]
	if len(sources) != 2 {
		t.Fatalf("setup should leave two generations, got %v", sources)
	}

	var out bytes.Buffer
	if err := execute(syncArgs(indexDir, newRoot, "-f"), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Consolidated") {
		t.Fatalf("sync did not consolidate the mixed state:\n%s", out.String())
	}
	wantNew := normalizeSource(resolvedNew)
	if got := liveSources(t, indexDir)["team/repo"]; len(got) != 1 || got[0] != wantNew {
		t.Fatalf("after consolidation source = %v, want %s", got, wantNew)
	}
	globbed, err := filepath.Glob(filepath.Join(indexDir, "*.zoekt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(globbed) != 1 {
		t.Fatalf("got %d shard files after consolidation, want 1: %v", len(globbed), globbed)
	}
	assertStagingEmpty(t, indexDir)
}

func TestCanConsolidate(t *testing.T) {
	group := func(paths ...string) shardGroup {
		var shards []shardInfo
		for _, path := range paths {
			shards = append(shards, shardInfo{Path: path})
		}
		return shardGroup{Shards: shards}
	}
	shard := func(n int) string {
		return filepath.Join("/idx", fmt.Sprintf("repo_v17.%05d.zoekt", n))
	}
	for _, tc := range []struct {
		name   string
		match  []string
		others []string
		wantOK bool
	}{
		{name: "contiguous new prefix with higher-numbered stale shard", match: []string{shard(0)}, others: []string{shard(1)}, wantOK: true},
		{name: "complete two-shard match", match: []string{shard(0), shard(1)}, wantOK: true},
		{name: "match missing shard zero", match: []string{shard(1)}, wantOK: false},
		{name: "gap in match", match: []string{shard(0), shard(2)}, wantOK: false},
		{name: "stale shard overlaps match numbers", match: []string{shard(0), shard(1)}, others: []string{shard(1)}, wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var otherGroups []shardGroup
			if len(tc.others) > 0 {
				otherGroups = []shardGroup{group(tc.others...)}
			}
			if got := canConsolidate(group(tc.match...), otherGroups); got != tc.wantOK {
				t.Fatalf("canConsolidate = %v, want %v", got, tc.wantOK)
			}
		})
	}
}

func TestSameSourceRepeatedSyncReusesCache(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "team", "repo")
	createGitRepository(t, repo)
	indexDir := t.TempDir()

	if err := execute(syncArgs(indexDir, root, "-f"), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}

	// A new commit at the same source is replaced transactionally, but
	// repeated no-op runs reuse the existing cache.
	addCommit(t, repo, "CHANGELOG.md", "changelog entry\n", "second commit")
	var out bytes.Buffer
	if err := execute(syncArgs(indexDir, root, "-f"), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `Indexed "team/repo"`) {
		t.Fatalf("changed repository was not reindexed:\n%s", out.String())
	}
	if got := liveSources(t, indexDir)["team/repo"]; len(got) != 1 {
		t.Fatalf("unexpected sources after update: %v", got)
	}
	assertStagingEmpty(t, indexDir)
	updated := liveShardFiles(t, indexDir)
	infoAfterUpdate, err := os.Stat(updated[0])
	if err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := execute(syncArgs(indexDir, root, "-f"), &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `Up to date "team/repo"`) {
		t.Fatalf("repeat sync did not reuse the cache:\n%s", out.String())
	}
	reused := liveShardFiles(t, indexDir)
	if len(reused) != 1 || reused[0] != updated[0] {
		t.Fatalf("no-op sync changed the cache files: before %v after %v", updated, reused)
	}
	infoAfterNoOp, err := os.Stat(reused[0])
	if err != nil {
		t.Fatal(err)
	}
	if !infoAfterUpdate.ModTime().Equal(infoAfterNoOp.ModTime()) {
		t.Fatal("no-op sync rewrote the reused cache")
	}
}
