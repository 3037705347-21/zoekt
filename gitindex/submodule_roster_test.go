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
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestSubmoduleSnapshotRoundTrip(t *testing.T) {
	t.Parallel()

	original := &submoduleSnapshot{files: []submoduleSnapshotFile{
		{sub: "", path: "bfile", content: []byte("bcont\n")},
		{sub: "", path: "empty", content: []byte{}},
		{sub: "", path: "binary", content: []byte{0x00, 0x01, 0x02, 0xff}},
		{sub: "nested", path: "n.txt", content: []byte("nested content\n")},
		{sub: "nested", path: "missing.txt", missing: true},
	}}

	data := marshalSubmoduleSnapshot(original)
	restored, err := unmarshalSubmoduleSnapshot(data)
	if err != nil {
		t.Fatalf("unmarshalSubmoduleSnapshot: %v", err)
	}
	if len(restored.files) != len(original.files) {
		t.Fatalf("got %d files, want %d", len(restored.files), len(original.files))
	}
	for i, want := range original.files {
		got := restored.files[i]
		if got.sub != want.sub || got.path != want.path || got.missing != want.missing {
			t.Fatalf("file %d metadata mismatch: got %+v, want %+v", i, got, want)
		}
		if want.missing {
			if got.content != nil {
				t.Fatalf("missing file %d carried content", i)
			}
			continue
		}
		if string(got.content) != string(want.content) {
			t.Fatalf("file %d content mismatch: got %q, want %q", i, got.content, want.content)
		}
	}
}

func TestSubmoduleSnapshotRejectsCorruptInput(t *testing.T) {
	t.Parallel()

	good := marshalSubmoduleSnapshot(&submoduleSnapshot{files: []submoduleSnapshotFile{
		{path: "a", content: []byte("abc")},
	}})

	cases := map[string][]byte{
		"bad magic":         []byte("XXXXXXXXXX\n"),
		"truncated header":  good[:len(good)-4],
		"truncated content": good[:len(good)-1],
		"trailing garbage":  append(append([]byte{}, good...), 0),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := unmarshalSubmoduleSnapshot(data); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func testSnapshot(contents map[string]string, missing ...string) *submoduleSnapshot {
	paths := make([]string, 0, len(contents))
	for path := range contents {
		paths = append(paths, path)
	}
	paths = append(paths, missing...)
	sort.Strings(paths)

	missingSet := map[string]struct{}{}
	for _, path := range missing {
		missingSet[path] = struct{}{}
	}

	snapshot := &submoduleSnapshot{}
	for _, path := range paths {
		file := submoduleSnapshotFile{path: path}
		if _, isMissing := missingSet[path]; !isMissing {
			file.content = []byte(contents[path])
		} else {
			file.missing = true
		}
		snapshot.files = append(snapshot.files, file)
	}
	return snapshot
}

func rosterEntryForTest(t *testing.T, r *submoduleRoster, identity string) *submoduleRosterEntry {
	t.Helper()
	entry := r.previous(identity)
	if entry == nil {
		t.Fatalf("roster has no entry for %q", identity)
	}
	return entry
}

func TestSubmoduleRosterCommitsAndReopens(t *testing.T) {
	baseDir := t.TempDir()
	source := filepath.Join(t.TempDir(), "parent")

	r, err := openSubmoduleRoster(baseDir, source, "parent")
	if err != nil {
		t.Fatal(err)
	}
	r.noteFresh("lib", "vendor/lib", "../up", "aaa111", testSnapshot(map[string]string{
		"bfile":     "bcont\n",
		"sub/n.txt": "nested\n",
	}))
	if err := r.commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	manifestPath := filepath.Join(r.dir, submoduleRosterManifestName)
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifestData), `"vendor/lib"`) || !strings.Contains(string(manifestData), "aaa111") {
		t.Fatalf("manifest does not record submodule attribution:\n%s", manifestData)
	}
	blobs := rosterBlobFiles(t, r.dir)
	if len(blobs) != 1 {
		t.Fatalf("got %d snapshot blobs, want 1: %v", len(blobs), blobs)
	}

	// The persisted snapshot verifies and restores byte-for-byte.
	reopened, err := openSubmoduleRoster(baseDir, source, "parent")
	if err != nil {
		t.Fatal(err)
	}
	entry := rosterEntryForTest(t, reopened, "lib")
	snapshot, err := reopened.loadSnapshot(entry)
	if err != nil {
		t.Fatalf("loadSnapshot: %v", err)
	}
	if got := snapshotFileByName(snapshot, "bfile"); got != "bcont\n" {
		t.Fatalf("got bfile=%q, want bcont", got)
	}
}

func TestSubmoduleRosterReuseKeepsContentAndMovesPath(t *testing.T) {
	baseDir := t.TempDir()
	source := filepath.Join(t.TempDir(), "parent")

	first, err := openSubmoduleRoster(baseDir, source, "parent")
	if err != nil {
		t.Fatal(err)
	}
	first.noteFresh("lib", "vendor/lib", "../up", "aaa111", testSnapshot(map[string]string{"bfile": "bcont\n"}))
	if err := first.commit(); err != nil {
		t.Fatal(err)
	}
	blobsAfterFirst := rosterBlobFiles(t, first.dir)
	if len(blobsAfterFirst) != 1 {
		t.Fatalf("got %d blobs after first commit", len(blobsAfterFirst))
	}

	// The submodule moved to a new path but is temporarily unreadable. A
	// reused snapshot must be re-prefixed to the new path without leaving a
	// duplicate blob behind.
	second, err := openSubmoduleRoster(baseDir, source, "parent")
	if err != nil {
		t.Fatal(err)
	}
	second.noteReused("lib", "deps/lib", "../up", "aaa111")
	if err := second.commit(); err != nil {
		t.Fatalf("commit after move: %v", err)
	}
	if blobs := rosterBlobFiles(t, second.dir); len(blobs) != 1 || blobs[0] != blobsAfterFirst[0] {
		t.Fatalf("move left duplicate snapshots: %v (was %v)", blobs, blobsAfterFirst)
	}

	reopened, err := openSubmoduleRoster(baseDir, source, "parent")
	if err != nil {
		t.Fatal(err)
	}
	entry := rosterEntryForTest(t, reopened, "lib")
	if entry.Path != "deps/lib" || entry.Name != "lib" || entry.Snapshot != blobsAfterFirst[0] {
		t.Fatalf("entry did not track the move: %+v", entry)
	}
}

func TestSubmoduleRosterRetiresOnlyAfterConfirmedRemoval(t *testing.T) {
	baseDir := t.TempDir()
	source := filepath.Join(t.TempDir(), "parent")

	r, err := openSubmoduleRoster(baseDir, source, "parent")
	if err != nil {
		t.Fatal(err)
	}
	r.noteFresh("lib", "vendor/lib", "../up", "aaa111", testSnapshot(map[string]string{"bfile": "bcont\n"}))
	if err := r.commit(); err != nil {
		t.Fatal(err)
	}

	// A run that cannot read the submodule keeps it on the roster: the
	// content is still in use (served as stale content during that build).
	stale, err := openSubmoduleRoster(baseDir, source, "parent")
	if err != nil {
		t.Fatal(err)
	}
	stale.noteReused("lib", "vendor/lib", "../up", "aaa111")
	if err := stale.commit(); err != nil {
		t.Fatal(err)
	}
	if blobs := rosterBlobFiles(t, stale.dir); len(blobs) != 1 {
		t.Fatalf("stale reuse retired the snapshot too early: %v", blobs)
	}

	// A complete run that no longer lists the submodule retires it.
	confirmed, err := openSubmoduleRoster(baseDir, source, "parent")
	if err != nil {
		t.Fatal(err)
	}
	if err := confirmed.commit(); err != nil {
		t.Fatal(err)
	}
	if blobs := rosterBlobFiles(t, confirmed.dir); len(blobs) != 0 {
		t.Fatalf("retired submodule left blobs behind: %v", blobs)
	}
	reopened, err := openSubmoduleRoster(baseDir, source, "parent")
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.prev) != 0 {
		t.Fatalf("retired submodule still listed: %+v", reopened.prev)
	}
}

func TestSubmoduleRosterRejectsReuseWithoutSavedBlob(t *testing.T) {
	baseDir := t.TempDir()
	source := filepath.Join(t.TempDir(), "parent")

	r, err := openSubmoduleRoster(baseDir, source, "parent")
	if err != nil {
		t.Fatal(err)
	}
	r.noteFresh("lib", "vendor/lib", "../up", "aaa111", testSnapshot(map[string]string{"bfile": "bcont\n"}))
	if err := r.commit(); err != nil {
		t.Fatal(err)
	}
	for _, path := range rosterBlobFiles(t, r.dir) {
		if err := os.Remove(filepath.Join(r.dir, path)); err != nil {
			t.Fatal(err)
		}
	}

	next, err := openSubmoduleRoster(baseDir, source, "parent")
	if err != nil {
		t.Fatal(err)
	}
	next.noteReused("lib", "vendor/lib", "../up", "aaa111")
	if err := next.commit(); err == nil {
		t.Fatal("commit succeeded although the reused snapshot blob is gone")
	}
}

func TestSubmoduleRosterDetectsCorruptSavedContent(t *testing.T) {
	baseDir := t.TempDir()
	source := filepath.Join(t.TempDir(), "parent")

	r, err := openSubmoduleRoster(baseDir, source, "parent")
	if err != nil {
		t.Fatal(err)
	}
	r.noteFresh("lib", "vendor/lib", "../up", "aaa111", testSnapshot(map[string]string{"bfile": "bcont\n"}))
	if err := r.commit(); err != nil {
		t.Fatal(err)
	}

	blobs := rosterBlobFiles(t, r.dir)
	corrupt := filepath.Join(r.dir, blobs[0])
	data, err := os.ReadFile(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(corrupt, data, 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := openSubmoduleRoster(baseDir, source, "parent")
	if err != nil {
		t.Fatal(err)
	}
	entry := rosterEntryForTest(t, reopened, "lib")
	if _, err := reopened.loadSnapshot(entry); err == nil {
		t.Fatal("corrupt snapshot was accepted")
	}
}

func TestSubmoduleRosterRejectsForeignAttribution(t *testing.T) {
	baseDir := t.TempDir()
	source := filepath.Join(t.TempDir(), "parent")

	r, err := openSubmoduleRoster(baseDir, source, "parent")
	if err != nil {
		t.Fatal(err)
	}
	r.noteFresh("lib", "vendor/lib", "../up", "aaa111", testSnapshot(map[string]string{"bfile": "bcont\n"}))
	if err := r.commit(); err != nil {
		t.Fatal(err)
	}

	// Copy the committed manifest into another parent's roster directory,
	// as could happen when a roster directory is copied or reused out of
	// band. The new parent must reject the foreign manifest instead of
	// adopting the saved attribution.
	other, err := normalizeRosterSource(filepath.Join(t.TempDir(), "other"))
	if err != nil {
		t.Fatal(err)
	}
	foreignDir := filepath.Join(baseDir, rosterParentID(other))
	if err := os.MkdirAll(foreignDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(r.dir, submoduleRosterManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreignDir, submoduleRosterManifestName), manifest, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := openSubmoduleRoster(baseDir, other, "other"); !errors.Is(err, errSubmoduleUnattributable) {
		t.Fatalf("got %v, want errSubmoduleUnattributable", err)
	}
}

func TestSubmoduleRosterReconcilesInterruptedWrites(t *testing.T) {
	baseDir := t.TempDir()
	source := filepath.Join(t.TempDir(), "parent")

	r, err := openSubmoduleRoster(baseDir, source, "parent")
	if err != nil {
		t.Fatal(err)
	}
	r.noteFresh("lib", "vendor/lib", "../up", "aaa111", testSnapshot(map[string]string{"bfile": "bcont\n"}))
	if err := r.commit(); err != nil {
		t.Fatal(err)
	}
	referenced := rosterBlobFiles(t, r.dir)

	// Simulate leftovers from a process that died mid-publish: a staged
	// manifest, a staged blob and an orphaned, previously-referenced blob.
	if err := os.WriteFile(filepath.Join(r.dir, ".manifest.json.123.tmp"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	orphan := strings.Repeat("a", sha256HexLen) + ".bin"
	if err := os.WriteFile(filepath.Join(r.dir, orphan), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "."+orphan+".456.tmp"), []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := openSubmoduleRoster(baseDir, source, "parent")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(reopened.dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	for _, leftover := range []string{".manifest.json.123.tmp", orphan, "." + orphan + ".456.tmp"} {
		if containsString(names, leftover) {
			t.Fatalf("interrupted-write leftover %q was not swept: %v", leftover, names)
		}
	}
	if blobs := rosterBlobFiles(t, reopened.dir); len(blobs) != 1 || blobs[0] != referenced[0] {
		t.Fatalf("referenced blob was swept: got %v, want %v", blobs, referenced)
	}
	rosterEntryForTest(t, reopened, "lib") // previous state survived
}

func TestDeleteSubmoduleRoster(t *testing.T) {
	baseDir := t.TempDir()
	source := filepath.Join(t.TempDir(), "parent")

	r, err := openSubmoduleRoster(baseDir, source, "parent")
	if err != nil {
		t.Fatal(err)
	}
	r.noteFresh("lib", "vendor/lib", "../up", "aaa111", testSnapshot(map[string]string{"bfile": "bcont\n"}))
	if err := r.commit(); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSubmoduleRoster(baseDir, source); err != nil {
		t.Fatalf("DeleteSubmoduleRoster: %v", err)
	}
	if _, err := os.Stat(r.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("roster directory still exists: %v", err)
	}
	if err := DeleteSubmoduleRoster(baseDir, source); err != nil {
		t.Fatalf("deleting a missing roster should be idempotent: %v", err)
	}
}

func rosterBlobFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var blobs []string
	for _, entry := range entries {
		if isSnapshotBlob(entry.Name()) {
			blobs = append(blobs, entry.Name())
		}
	}
	sort.Strings(blobs)
	return blobs
}

func snapshotFileByName(snapshot *submoduleSnapshot, name string) string {
	for _, file := range snapshot.files {
		if file.sub == "" && file.path == name {
			return string(file.content)
		}
	}
	return ""
}

const sha256HexLen = 64
