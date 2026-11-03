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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
	"github.com/sourcegraph/zoekt/query"
	"github.com/sourcegraph/zoekt/search"
)

func searchPattern(t *testing.T, indexDir, pattern string) []zoekt.FileMatch {
	t.Helper()
	searcher, err := search.NewDirectorySearcher(indexDir)
	if err != nil {
		t.Fatal("NewDirectorySearcher", err)
	}
	defer searcher.Close()

	results, err := searcher.Search(context.Background(),
		&query.Substring{Pattern: pattern},
		&zoekt.SearchOptions{})
	if err != nil {
		t.Fatal("Search", err)
	}
	return results.Files
}

// TestSubmoduleRosterRepoCacheKeepsContentWhenMirrorVanishes covers the
// repository-cache indexing path: when the cached bare submodule repository
// cannot be opened (for example after a failed mirror update over a flaky
// network), the rebuild must keep serving the submodule's last-known-good
// content instead of silently dropping it.
func TestSubmoduleRosterRepoCacheKeepsContentWhenMirrorVanishes(t *testing.T) {
	dir := t.TempDir()
	if err := createSubmoduleRepo(dir); err != nil {
		t.Fatalf("createSubmoduleRepo: %v", err)
	}
	indexDir := t.TempDir()
	rosterDir := t.TempDir()

	opts := Options{
		RepoDir:            filepath.Join(dir, "gerrit.googlesource.com", "adir.git"),
		BuildOptions:       index.Options{IndexDir: indexDir},
		BranchPrefix:       "refs/heads/",
		Branches:           []string{"master"},
		Submodules:         true,
		Incremental:        true,
		RepoCacheDir:       dir,
		SubmoduleRosterDir: rosterDir,
	}
	if _, err := IndexGitRepo(opts); err != nil {
		t.Fatalf("initial IndexGitRepo: %v", err)
	}
	if files := searchPattern(t, indexDir, "bcont"); len(files) != 1 {
		t.Fatalf("initial index: got %d matches for submodule content, want 1", len(files))
	}

	// Simulate the mirror of the submodule becoming unavailable.
	cachedSubmodule := filepath.Join(dir, "gerrit.googlesource.com", "bdir.git")
	if err := os.RemoveAll(cachedSubmodule); err != nil {
		t.Fatal(err)
	}

	// A forced rebuild must succeed using the saved content...
	rebuild := opts
	rebuild.Incremental = false
	if _, err := IndexGitRepo(rebuild); err != nil {
		t.Fatalf("rebuild with vanished submodule mirror: %v", err)
	}
	files := searchPattern(t, indexDir, "bcont")
	if len(files) != 1 {
		t.Fatalf("stale rebuild: got %d matches for submodule content, want 1", len(files))
	}
	if got := files[0].SubRepositoryPath; got != "bname" {
		t.Fatalf("restored content lost its attribution path: got %q, want %q", got, "bname")
	}

	// ...and the roster still attributes the content to this parent and
	// submodule identity.
	source, err := normalizeRosterSource(opts.RepoDir)
	if err != nil {
		t.Fatal(err)
	}
	roster, err := openSubmoduleRoster(rosterDir, source, "")
	if err != nil {
		t.Fatal(err)
	}
	entry := roster.previous("bname")
	if entry == nil {
		t.Fatal("roster does not record the submodule bname")
	}
	if entry.Path != "bname" || len(entry.Commit) != 40 {
		t.Fatalf("unexpected roster attribution: %+v", entry)
	}
}

// TestSubmoduleRosterRepoCacheFailsWithoutSavedContent ensures that a
// submodule referenced by the parent tree on a first index cannot be
// skipped silently when it is unavailable and no saved content exists: the
// whole update must fail without publishing a shard.
func TestSubmoduleRosterRepoCacheFailsWithoutSavedContent(t *testing.T) {
	dir := t.TempDir()
	if err := createSubmoduleRepo(dir); err != nil {
		t.Fatalf("createSubmoduleRepo: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "gerrit.googlesource.com", "bdir.git")); err != nil {
		t.Fatal(err)
	}
	indexDir := t.TempDir()
	rosterDir := t.TempDir()

	opts := Options{
		RepoDir:            filepath.Join(dir, "gerrit.googlesource.com", "adir.git"),
		BuildOptions:       index.Options{IndexDir: indexDir},
		BranchPrefix:       "refs/heads/",
		Branches:           []string{"master"},
		Submodules:         true,
		RepoCacheDir:       dir,
		SubmoduleRosterDir: rosterDir,
	}
	_, err := IndexGitRepo(opts)
	if err == nil {
		t.Fatal("indexing succeeded although the submodule was unavailable and no content was saved")
	}
	if !strings.Contains(err.Error(), "no last-known-good content") {
		t.Fatalf("error %v does not explain the missing saved content", err)
	}

	entries, err := os.ReadDir(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".zoekt") {
			t.Fatalf("failed update published a shard: %s", entry.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(rosterDir)); err != nil {
		t.Fatal(err)
	}
}
