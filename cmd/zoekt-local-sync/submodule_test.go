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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/internal/testutil"
	"github.com/sourcegraph/zoekt/query"
	"github.com/sourcegraph/zoekt/search"
)

// runShellScript runs a shell script with the controlled git test
// environment. Scripts receive paths through %q so they stay safe under any
// temp directory.
func runShellScript(t *testing.T, script string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-euc", script)
	cmd.Env = testutil.GitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("shell script failed: %v\n%s", err, out)
	}
}

// createSubmoduleFixture creates a root containing a "parent" repository
// with submodule "lib" checked out at vendor/lib, plus a standalone "other"
// repository. The submodule's upstream lives outside the discovery root so
// only the parent is discovered there.
func createSubmoduleFixture(t *testing.T) (root, parent, upstream string) {
	t.Helper()
	workspace := t.TempDir()
	root = filepath.Join(workspace, "roots")
	upstream = filepath.Join(workspace, "up")
	parent = filepath.Join(root, "parent")
	other := filepath.Join(root, "other")

	runShellScript(t, fmt.Sprintf(`
set -eu
mkdir -p %[1]q
cd %[1]q
git init -q -b main
printf 'upstream unique bcont\n' > bfile
mkdir sub
printf 'upstream nested content\n' > sub/n
git add .
git commit -qm up

mkdir -p %[2]q
cd %[2]q
git init -q -b main
printf 'root unique rootcont-v1\n' > rootfile
git add .
git commit -qm root
git -c protocol.file.allow=always submodule add -q --name lib %[1]q vendor/lib
git commit -qm 'add submodule'

mkdir -p %[3]q
cd %[3]q
git init -q -b main
printf 'standalone other-marker\n' > other.go
git add .
git commit -qm other
`, upstream, parent, other))
	return root, parent, upstream
}

func syncForce(t *testing.T, indexDir, root string, extra ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	args := []string{"-index", indexDir, "-disable_ctags", "-f"}
	args = append(args, extra...)
	args = append(args, root)
	err := execute(args, &out, &bytes.Buffer{})
	return out.String(), err
}

func searchFiles(t *testing.T, indexDir, pattern string) []zoekt.FileMatch {
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
		t.Fatalf("Search(%q): %v", pattern, err)
	}
	return results.Files
}

func countAtSubPath(files []zoekt.FileMatch, subPath string) int {
	count := 0
	for _, file := range files {
		if file.SubRepositoryPath == subPath {
			count++
		}
	}
	return count
}

// rosterDirForParent returns the on-disk roster directory for parentSource
// under indexDir.
func rosterDirForParent(t *testing.T, indexDir, parentSource string) string {
	t.Helper()
	absolute, err := filepath.EvalSymlinks(parentSource)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(absolute))
	return filepath.Join(indexDir, defaultSubmoduleRosterDir, hex.EncodeToString(sum[:]))
}

func rosterManifestBytes(t *testing.T, indexDir, parentSource string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(rosterDirForParent(t, indexDir, parentSource), "manifest.json"))
	if err != nil {
		t.Fatalf("read roster manifest: %v", err)
	}
	return data
}

func rosterBlobNames(t *testing.T, indexDir, parentSource string) []string {
	t.Helper()
	entries, err := os.ReadDir(rosterDirForParent(t, indexDir, parentSource))
	if err != nil {
		t.Fatal(err)
	}
	var blobs []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".bin") {
			blobs = append(blobs, entry.Name())
		}
	}
	return blobs
}

// TestSyncSubmoduleServedFromRosterWhenUnreachable is the core scenario:
// after one good sync, "deleting" the checked-out submodule (as a network or
// mirror failure would) must not make its content vanish from search. The
// parent repository's own update and an unrelated repository must keep
// working, and an identical rerun must be a no-op.
func TestSyncSubmoduleServedFromRosterWhenUnreachable(t *testing.T) {
	root, parent, upstream := createSubmoduleFixture(t)
	indexDir := t.TempDir()

	if out, err := syncForce(t, indexDir, root); err != nil {
		t.Fatalf("initial sync: %v\n%s", err, out)
	}
	if files := searchFiles(t, indexDir, "bcont"); countAtSubPath(files, "vendor/lib") != 1 {
		t.Fatalf("initial sync missing submodule content: %+v", files)
	}
	if files := searchFiles(t, indexDir, "nested content"); countAtSubPath(files, "vendor/lib") != 1 {
		t.Fatalf("nested submodule file missing: %+v", files)
	}
	if files := searchFiles(t, indexDir, "rootcont-v1"); countAtSubPath(files, "") != 1 {
		t.Fatalf("main repository content missing: %+v", files)
	}
	if files := searchFiles(t, indexDir, "other-marker"); len(files) != 1 {
		t.Fatalf("standalone repository missing: %+v", files)
	}

	manifestBefore := rosterManifestBytes(t, indexDir, parent)
	blobsBefore := rosterBlobNames(t, indexDir, parent)
	if len(blobsBefore) != 1 {
		t.Fatalf("got %d submodule snapshots, want 1", len(blobsBefore))
	}

	// An identical rerun changes nothing.
	if out, err := syncForce(t, indexDir, root); err != nil {
		t.Fatalf("rerun: %v\n%s", err, out)
	} else if !strings.Contains(out, "Up to date") {
		t.Fatalf("identical rerun was not a no-op:\n%s", out)
	}
	if !bytes.Equal(rosterManifestBytes(t, indexDir, parent), manifestBefore) {
		t.Fatal("identical rerun modified the roster manifest")
	}

	// Simulate the submodule becoming unreachable: deinit clears its
	// worktree and registration. Advance the parent repository at the same
	// time so a rebuild is genuinely required, and publish new content
	// upstream that an unreachable, stale submodule must not pick up.
	runShellScript(t, fmt.Sprintf(`
set -eu
cd %q
git submodule deinit -f vendor/lib >/dev/null 2>&1
cd %q
printf 'upstream unique upstream-v2\n' > v2
git add v2
git commit -qm 'upstream v2'
cd %q
printf 'root unique rootcont-v2\n' > rootfile
git add rootfile
git commit -qm 'parent update while submodule unreachable'
`, parent, upstream, parent))

	out, err := syncForce(t, indexDir, root)
	if err != nil {
		t.Fatalf("sync with unreachable submodule failed: %v\n%s", err, out)
	}

	// Stale submodule content remains searchable at its attributed path...
	if files := searchFiles(t, indexDir, "bcont"); countAtSubPath(files, "vendor/lib") != 1 {
		t.Fatalf("submodule content disappeared while unreachable: %+v", files)
	}
	if files := searchFiles(t, indexDir, "nested content"); countAtSubPath(files, "vendor/lib") != 1 {
		t.Fatalf("nested submodule content disappeared: %+v", files)
	}
	// ...it is genuinely stale: upstream content published after the last
	// successful read does not appear...
	if files := searchFiles(t, indexDir, "upstream-v2"); len(files) != 0 {
		t.Fatalf("stale submodule unexpectedly picked up new upstream content: %+v", files)
	}
	// ...the parent rebuild landed its new content...
	if files := searchFiles(t, indexDir, "rootcont-v2"); countAtSubPath(files, "") != 1 {
		t.Fatalf("parent update did not land: %+v", files)
	}
	if files := searchFiles(t, indexDir, "rootcont-v1"); len(files) != 0 {
		t.Fatalf("old parent content still searchable: %+v", files)
	}
	// ...and the unrelated repository was left intact.
	if files := searchFiles(t, indexDir, "other-marker"); len(files) != 1 {
		t.Fatalf("unrelated repository was affected: %+v", files)
	}

	// The roster still attributes the content to the same identity, path
	// and snapshot (no duplicate copy).
	manifest := string(rosterManifestBytes(t, indexDir, parent))
	if !strings.Contains(manifest, `"name": "lib"`) ||
		!strings.Contains(manifest, `"path": "vendor/lib"`) {
		t.Fatalf("roster lost submodule attribution:\n%s", manifest)
	}
	if blobs := rosterBlobNames(t, indexDir, parent); len(blobs) != 1 || blobs[0] != blobsBefore[0] {
		t.Fatalf("fallback duplicated the snapshot: got %v, want %v", blobs, blobsBefore)
	}
}

// TestSyncSubmoduleRetiredAfterConfirmedRemoval ensures the submodule's old
// content disappears from both the index and the roster only after a full
// successful sync confirms the submodule was removed.
func TestSyncSubmoduleRetiredAfterConfirmedRemoval(t *testing.T) {
	root, parent, _ := createSubmoduleFixture(t)
	indexDir := t.TempDir()

	if out, err := syncForce(t, indexDir, root); err != nil {
		t.Fatalf("initial sync: %v\n%s", err, out)
	}
	if files := searchFiles(t, indexDir, "bcont"); len(files) != 1 {
		t.Fatalf("initial sync missing submodule content: %+v", files)
	}

	runShellScript(t, fmt.Sprintf(`
set -eu
cd %q
git rm -q -f vendor/lib
git commit -qm 'remove submodule'
`, parent))

	if out, err := syncForce(t, indexDir, root); err != nil {
		t.Fatalf("sync after removal: %v\n%s", err, out)
	}
	if files := searchFiles(t, indexDir, "bcont"); len(files) != 0 {
		t.Fatalf("removed submodule content stayed searchable: %+v", files)
	}
	if files := searchFiles(t, indexDir, "rootcont-v1"); countAtSubPath(files, "") != 1 {
		t.Fatalf("main repository content was removed together with the submodule: %+v", files)
	}

	manifest := string(rosterManifestBytes(t, indexDir, parent))
	if strings.Contains(manifest, `"name": "lib"`) {
		t.Fatalf("removed submodule stayed on the roster:\n%s", manifest)
	}
	if blobs := rosterBlobNames(t, indexDir, parent); len(blobs) != 0 {
		t.Fatalf("removed submodule left saved content behind: %v", blobs)
	}
}

// TestSyncSubmoduleMoveLeavesNoDuplicate moves a submodule to another path
// and checks that only the new path is indexed and stored.
func TestSyncSubmoduleMoveLeavesNoDuplicate(t *testing.T) {
	root, parent, _ := createSubmoduleFixture(t)
	indexDir := t.TempDir()

	if out, err := syncForce(t, indexDir, root); err != nil {
		t.Fatalf("initial sync: %v\n%s", err, out)
	}

	runShellScript(t, fmt.Sprintf(`
set -eu
cd %q
mkdir -p deps
git mv vendor/lib deps/lib
git commit -qm 'move submodule'
`, parent))

	if out, err := syncForce(t, indexDir, root); err != nil {
		t.Fatalf("sync after move: %v\n%s", err, out)
	}
	files := searchFiles(t, indexDir, "bcont")
	if countAtSubPath(files, "deps/lib") != 1 {
		t.Fatalf("moved submodule content not indexed at new path: %+v", files)
	}
	if countAtSubPath(files, "vendor/lib") != 0 {
		t.Fatalf("old path kept a duplicate copy: %+v", files)
	}
	if len(files) != 1 {
		t.Fatalf("move produced %d copies of the submodule file, want 1", len(files))
	}

	manifest := string(rosterManifestBytes(t, indexDir, parent))
	if !strings.Contains(manifest, `"path": "deps/lib"`) ||
		strings.Contains(manifest, `"path": "vendor/lib"`) {
		t.Fatalf("roster did not track the path move:\n%s", manifest)
	}
	if strings.Count(manifest, `"name": "lib"`) != 1 {
		t.Fatalf("roster lists the moved submodule more than once:\n%s", manifest)
	}
	if blobs := rosterBlobNames(t, indexDir, parent); len(blobs) != 1 {
		t.Fatalf("move left %d snapshot copies, want 1: %v", len(blobs), blobs)
	}
}

// TestSyncUnattributableGitlinkDoesNotLand removes the .gitmodules entry but
// keeps the gitlink. Neither a new shard nor roster changes may land because
// the submodule content's ownership cannot be confirmed.
func TestSyncUnattributableGitlinkDoesNotLand(t *testing.T) {
	root, parent, _ := createSubmoduleFixture(t)
	indexDir := t.TempDir()

	if out, err := syncForce(t, indexDir, root); err != nil {
		t.Fatalf("initial sync: %v\n%s", err, out)
	}
	manifestBefore := rosterManifestBytes(t, indexDir, parent)
	shardsBefore, err := readInventory(indexDir)
	if err != nil {
		t.Fatal(err)
	}

	runShellScript(t, fmt.Sprintf(`
set -eu
cd %q
git config -f .gitmodules --remove-section submodule.lib
git add .gitmodules
git commit -qm 'drop submodule attribution'
`, parent))

	if _, err := syncForce(t, indexDir, root); err == nil {
		t.Fatal("sync succeeded although the gitlink has no .gitmodules attribution")
	} else if !strings.Contains(err.Error(), "attribution cannot be confirmed") {
		t.Fatalf("error %v does not explain the attribution problem", err)
	}

	// The old shard is untouched: both parent and submodule content remain
	// searchable as they were.
	if files := searchFiles(t, indexDir, "bcont"); countAtSubPath(files, "vendor/lib") != 1 {
		t.Fatalf("failed update changed search results: %+v", files)
	}
	if files := searchFiles(t, indexDir, "rootcont-v1"); countAtSubPath(files, "") != 1 {
		t.Fatalf("main repository content changed after failed update: %+v", files)
	}
	if !bytes.Equal(rosterManifestBytes(t, indexDir, parent), manifestBefore) {
		t.Fatal("failed update modified the roster manifest")
	}
	shardsAfter, err := readInventory(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(shardsAfter) != len(shardsBefore) {
		t.Fatalf("failed update changed the shard set: before %d, after %d", len(shardsBefore), len(shardsAfter))
	}
}

// TestSyncUnavailableSubmoduleWithoutRosterDoesNotLand checks the first-run
// case: an unreachable submodule with no saved content must fail the parent
// update without writing a shard, while other repositories still sync.
func TestSyncUnavailableSubmoduleWithoutRosterDoesNotLand(t *testing.T) {
	root, parent, _ := createSubmoduleFixture(t)
	indexDir := t.TempDir()

	runShellScript(t, fmt.Sprintf(`
set -eu
cd %q
git submodule deinit -f vendor/lib >/dev/null 2>&1
`, parent))

	out, err := syncForce(t, indexDir, root)
	if err == nil {
		t.Fatal("sync succeeded for a parent whose submodule was never successfully indexed")
	}
	if !strings.Contains(err.Error(), "no last-known-good content") {
		t.Fatalf("error %v does not explain the missing saved content", err)
	}

	// The standalone repository was unaffected by the parent failure.
	if files := searchFiles(t, indexDir, "other-marker"); len(files) != 1 {
		t.Fatalf("unrelated repository did not sync:\n%s%+v", out, files)
	}
	shards, err := readInventory(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, shard := range shards {
		if shard.Repository.Name == "parent" {
			t.Fatal("the failed parent update published a shard")
		}
	}
	// The roster directory for the failed parent may have been prepared,
	// but no manifest was committed.
	if _, err := os.Stat(filepath.Join(rosterDirForParent(t, indexDir, parent), "manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("failed update committed a roster manifest: %v", err)
	}
}

// TestSyncRosterReconcilesStagedLeftovers ensures files left by an
// interrupted run neither block a rerun nor survive it.
func TestSyncRosterReconcilesStagedLeftovers(t *testing.T) {
	root, parent, _ := createSubmoduleFixture(t)
	indexDir := t.TempDir()

	if out, err := syncForce(t, indexDir, root); err != nil {
		t.Fatalf("initial sync: %v\n%s", err, out)
	}
	dir := rosterDirForParent(t, indexDir, parent)
	for _, leftover := range []string{
		".manifest.json.123.tmp",
		strings.Repeat("c", 64) + ".bin",
	} {
		if err := os.WriteFile(filepath.Join(dir, leftover), []byte("staged"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	runShellScript(t, fmt.Sprintf(`
set -eu
cd %q
printf 'root unique rootcont-v3\n' > rootfile
git add rootfile
git commit -qm 'another parent update'
`, parent))

	if out, err := syncForce(t, indexDir, root); err != nil {
		t.Fatalf("sync with staged leftovers: %v\n%s", err, out)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".tmp") {
			t.Fatalf("staged temporary file survived the rerun: %s", name)
		}
		if name == strings.Repeat("c", 64)+".bin" {
			t.Fatalf("orphan snapshot blob survived the rerun")
		}
	}
	if files := searchFiles(t, indexDir, "bcont"); countAtSubPath(files, "vendor/lib") != 1 {
		t.Fatalf("submodule content missing after reconciliation: %+v", files)
	}
}
