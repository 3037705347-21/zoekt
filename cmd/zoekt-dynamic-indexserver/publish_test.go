package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func shardName(repoID uint32, n int) string {
	return fmt.Sprintf("%d_v17.%05d.zoekt", repoID, n)
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func assertNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected %s to not exist, got err=%v", path, err)
	}
}

// seedOldIndex writes one shard for repo 100 and an unrelated shard for repo
// 200 directly into indexDir, simulating the currently live index.
func seedOldIndex(t *testing.T, indexDir string, shardCount int) {
	t.Helper()
	for i := 0; i < shardCount; i++ {
		writeTestFile(t, filepath.Join(indexDir, shardName(100, i)), fmt.Sprintf("old-100-%d", i))
	}
	writeTestFile(t, filepath.Join(indexDir, shardName(100, 0)+".meta"), "old-meta-100")
	writeTestFile(t, filepath.Join(indexDir, shardName(200, 0)), "old-200")
}

func stageBuild(t *testing.T, indexDir string, repoID uint32, generation int64, files map[string]string) string {
	t.Helper()
	staging, err := newStagingDir(indexDir, repoID, generation)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		writeTestFile(t, filepath.Join(staging, name), content)
	}
	return staging
}

func assertOtherRepoUntouched(t *testing.T, indexDir string) {
	t.Helper()
	if got := readTestFile(t, filepath.Join(indexDir, shardName(200, 0))); got != "old-200" {
		t.Fatalf("unrelated repository 200 was modified: got %q", got)
	}
}

func assertHiddenDirsEmpty(t *testing.T, indexDir string) {
	t.Helper()
	for _, name := range []string{stagingDirName, trashDirName, journalDirName} {
		entries, err := os.ReadDir(filepath.Join(indexDir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("hidden directory %s should be empty, contains %v", name, entries)
		}
	}
}

func TestPublishNewIndexReplacesShards(t *testing.T) {
	indexDir := t.TempDir()
	seedOldIndex(t, indexDir, 1)

	staging := stageBuild(t, indexDir, 100, 1, map[string]string{
		shardName(100, 0):           "new-100",
		shardName(100, 0) + ".meta": "new-meta-100",
	})

	if err := publishNewIndex(indexDir, 100, 1, staging); err != nil {
		t.Fatal(err)
	}

	if got := readTestFile(t, filepath.Join(indexDir, shardName(100, 0))); got != "new-100" {
		t.Fatalf("live shard content = %q, want new-100", got)
	}
	if got := readTestFile(t, filepath.Join(indexDir, shardName(100, 0)+".meta")); got != "new-meta-100" {
		t.Fatalf("live meta content = %q, want new-meta-100", got)
	}
	assertOtherRepoUntouched(t, indexDir)
	assertHiddenDirsEmpty(t, indexDir)
}

func TestPublishNewIndexShrinksShardCount(t *testing.T) {
	indexDir := t.TempDir()
	seedOldIndex(t, indexDir, 2)

	staging := stageBuild(t, indexDir, 100, 1, map[string]string{
		shardName(100, 0): "new-100",
	})

	if err := publishNewIndex(indexDir, 100, 1, staging); err != nil {
		t.Fatal(err)
	}

	if got := readTestFile(t, filepath.Join(indexDir, shardName(100, 0))); got != "new-100" {
		t.Fatalf("live shard content = %q, want new-100", got)
	}
	assertNotExist(t, filepath.Join(indexDir, shardName(100, 1)))
	assertOtherRepoUntouched(t, indexDir)
	assertHiddenDirsEmpty(t, indexDir)
}

func TestPublishNewIndexGrowsShardCount(t *testing.T) {
	indexDir := t.TempDir()
	seedOldIndex(t, indexDir, 1)

	staging := stageBuild(t, indexDir, 100, 1, map[string]string{
		shardName(100, 0): "new-100-0",
		shardName(100, 1): "new-100-1",
	})

	if err := publishNewIndex(indexDir, 100, 1, staging); err != nil {
		t.Fatal(err)
	}

	if got := readTestFile(t, filepath.Join(indexDir, shardName(100, 1))); got != "new-100-1" {
		t.Fatalf("new shard content = %q, want new-100-1", got)
	}
	assertHiddenDirsEmpty(t, indexDir)
}

func TestPublishNewIndexEmptyStaging(t *testing.T) {
	indexDir := t.TempDir()
	seedOldIndex(t, indexDir, 1)

	staging, err := newStagingDir(indexDir, 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := publishNewIndex(indexDir, 100, 1, staging); err == nil {
		t.Fatal("expected error for empty staging directory")
	}

	if got := readTestFile(t, filepath.Join(indexDir, shardName(100, 0))); got != "old-100-0" {
		t.Fatalf("previous index must survive failed publish, got %q", got)
	}
	assertOtherRepoUntouched(t, indexDir)
}

// TestRecoverCrashBeforeRename: journal exists but no rename happened.
func TestRecoverCrashBeforeRename(t *testing.T) {
	indexDir := t.TempDir()
	seedOldIndex(t, indexDir, 1)

	staging := stageBuild(t, indexDir, 100, 1, map[string]string{
		shardName(100, 0): "new-100",
	})
	if _, err := planSwap(indexDir, 100, 1, staging); err != nil {
		t.Fatal(err)
	}

	if err := recoverInterruptedPublishes(indexDir); err != nil {
		t.Fatal(err)
	}

	if got := readTestFile(t, filepath.Join(indexDir, shardName(100, 0))); got != "old-100-0" {
		t.Fatalf("old index must be intact after rollback, got %q", got)
	}
	assertOtherRepoUntouched(t, indexDir)
	assertHiddenDirsEmpty(t, indexDir)
}

// TestRecoverCrashAfterBackups: live files were moved to backups.
func TestRecoverCrashAfterBackups(t *testing.T) {
	indexDir := t.TempDir()
	seedOldIndex(t, indexDir, 1)

	staging := stageBuild(t, indexDir, 100, 1, map[string]string{
		shardName(100, 0):           "new-100",
		shardName(100, 0) + ".meta": "new-meta-100",
	})
	journal, err := planSwap(indexDir, 100, 1, staging)
	if err != nil {
		t.Fatal(err)
	}
	if err := moveLiveToBackups(indexDir, journal); err != nil {
		t.Fatal(err)
	}

	// Live shards are temporarily gone while backups hold them.
	assertNotExist(t, filepath.Join(indexDir, shardName(100, 0)))

	if err := recoverInterruptedPublishes(indexDir); err != nil {
		t.Fatal(err)
	}

	if got := readTestFile(t, filepath.Join(indexDir, shardName(100, 0))); got != "old-100-0" {
		t.Fatalf("old index must be restored, got %q", got)
	}
	if got := readTestFile(t, filepath.Join(indexDir, shardName(100, 0)+".meta")); got != "old-meta-100" {
		t.Fatalf("old meta must be restored, got %q", got)
	}
	assertOtherRepoUntouched(t, indexDir)
	assertHiddenDirsEmpty(t, indexDir)
}

// TestRecoverCrashDuringInstall: some new files reached the live directory.
func TestRecoverCrashDuringInstall(t *testing.T) {
	indexDir := t.TempDir()
	seedOldIndex(t, indexDir, 1)

	staging := stageBuild(t, indexDir, 100, 1, map[string]string{
		shardName(100, 0):           "new-100",
		shardName(100, 0) + ".meta": "new-meta-100",
	})
	journal, err := planSwap(indexDir, 100, 1, staging)
	if err != nil {
		t.Fatal(err)
	}
	if err := moveLiveToBackups(indexDir, journal); err != nil {
		t.Fatal(err)
	}

	// Install only the first file, simulating a crash mid-phase-2.
	first := journal.Files[0]
	if err := os.Rename(filepath.Join(staging, first.Final),
		filepath.Join(indexDir, first.Final)); err != nil {
		t.Fatal(err)
	}

	if err := recoverInterruptedPublishes(indexDir); err != nil {
		t.Fatal(err)
	}

	if got := readTestFile(t, filepath.Join(indexDir, shardName(100, 0))); got != "old-100-0" {
		t.Fatalf("old index must be restored over partial new files, got %q", got)
	}
	if got := readTestFile(t, filepath.Join(indexDir, shardName(100, 0)+".meta")); got != "old-meta-100" {
		t.Fatalf("old meta must be restored, got %q", got)
	}
	assertOtherRepoUntouched(t, indexDir)
	assertHiddenDirsEmpty(t, indexDir)
}

// TestRecoverCrashAfterCommit: marker exists; the new generation must win.
func TestRecoverCrashAfterCommit(t *testing.T) {
	indexDir := t.TempDir()
	seedOldIndex(t, indexDir, 1)

	staging := stageBuild(t, indexDir, 100, 1, map[string]string{
		shardName(100, 0):           "new-100",
		shardName(100, 0) + ".meta": "new-meta-100",
	})
	journal, err := planSwap(indexDir, 100, 1, staging)
	if err != nil {
		t.Fatal(err)
	}
	journalFile := journalPath(indexDir, 100, 1)
	if err := moveLiveToBackups(indexDir, journal); err != nil {
		t.Fatal(err)
	}
	if err := installStaged(indexDir, journal); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(journalFile+commitSuffix, nil); err != nil {
		t.Fatal(err)
	}

	if err := recoverInterruptedPublishes(indexDir); err != nil {
		t.Fatal(err)
	}

	if got := readTestFile(t, filepath.Join(indexDir, shardName(100, 0))); got != "new-100" {
		t.Fatalf("new generation must be finalized, got %q", got)
	}
	if got := readTestFile(t, filepath.Join(indexDir, shardName(100, 0)+".meta")); got != "new-meta-100" {
		t.Fatalf("new meta must be finalized, got %q", got)
	}
	assertOtherRepoUntouched(t, indexDir)
	assertHiddenDirsEmpty(t, indexDir)

	// Recovery must be idempotent.
	if err := recoverInterruptedPublishes(indexDir); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, filepath.Join(indexDir, shardName(100, 0))); got != "new-100" {
		t.Fatalf("new index changed on second recovery, got %q", got)
	}
}

// TestRecoverOrphanCommitMarker: the swap was committed and the journal
// forgotten, but the process died before removing the marker. The live shards
// are the new generation and must be kept; the marker is just garbage.
func TestRecoverOrphanCommitMarker(t *testing.T) {
	indexDir := t.TempDir()
	seedOldIndex(t, indexDir, 1)

	name := shardName(100, 0)
	writeTestFile(t, filepath.Join(indexDir, name), "new-100")
	writeTestFile(t, filepath.Join(indexDir, name+".meta"), "new-meta")
	marker := filepath.Join(indexDir, journalDirName,
		fmt.Sprintf("%s%d-%d%s", journalPrefix, 100, 1, commitSuffix))
	writeTestFile(t, marker, "")

	if err := recoverInterruptedPublishes(indexDir); err != nil {
		t.Fatal(err)
	}

	if got := readTestFile(t, filepath.Join(indexDir, name)); got != "new-100" {
		t.Fatalf("committed shards must be kept, got %q", got)
	}
	assertNotExist(t, marker)
	assertOtherRepoUntouched(t, indexDir)
}

func TestRecoverGarbageCollectsOrphans(t *testing.T) {
	indexDir := t.TempDir()
	seedOldIndex(t, indexDir, 1)

	writeTestFile(t, filepath.Join(indexDir, stagingDirName, "orphan", shardName(100, 0)), "stale")
	writeTestFile(t, filepath.Join(indexDir, trashDirName, "garbage.bak"), "garbage")

	if err := recoverInterruptedPublishes(indexDir); err != nil {
		t.Fatal(err)
	}

	assertNotExist(t, filepath.Join(indexDir, stagingDirName, "orphan"))
	assertNotExist(t, filepath.Join(indexDir, trashDirName, "garbage.bak"))
	if got := readTestFile(t, filepath.Join(indexDir, shardName(100, 0))); got != "old-100-0" {
		t.Fatalf("live index must be untouched, got %q", got)
	}
}
