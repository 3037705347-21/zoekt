// Copyright 2026 Sourcegraph, Inc. All rights reserved.
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

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// All mutable state of the dynamic indexserver besides the cloned git
// repositories lives in hidden, per-purpose subdirectories of indexDir.
// They share the filesystem with the live shards, which makes publishing a
// new generation a metadata-only rename(2). The shard directory watcher only
// reads *.zoekt files placed directly inside indexDir, so partial builds,
// journals and backups below are never searched.
const (
	stagingDirName = ".staging"
	trashDirName   = ".trash"
	journalDirName = ".journal"
	stateDirName   = ".state"

	journalPrefix = "publish-"
	journalSuffix = ".json"
	commitSuffix  = ".commit"
)

// swapFile describes one file participating in a generation swap.
type swapFile struct {
	// Final is the basename of the live shard (or shard sidecar) in indexDir.
	Final string `json:"final"`
	// Backup is the absolute path the previous live file was moved to, empty
	// when there was nothing to back up.
	Backup string `json:"backup,omitempty"`
	// OldOnly reports a shard that has no counterpart in the new generation
	// (e.g. the new build produced fewer shards). It is backed up and removed
	// on commit, restored on rollback.
	OldOnly bool `json:"old_only,omitempty"`
}

// publishJournal is the on-disk record of an in-progress shard swap. The swap
// only becomes durable once the sibling commit marker (<journal>.commit)
// exists; until then a crash rolls the swap back.
type publishJournal struct {
	RepoID     uint32     `json:"repo_id"`
	Generation int64      `json:"generation"`
	StagingDir string     `json:"staging_dir"`
	Files      []swapFile `json:"files"`
}

func journalPath(indexDir string, repoID uint32, generation int64) string {
	return filepath.Join(indexDir, journalDirName,
		fmt.Sprintf("%s%d-%d%s", journalPrefix, repoID, generation, journalSuffix))
}

// newStagingDir creates an empty, unique staging directory for one indexing
// attempt. Staging directories are per attempt, so concurrent builds for
// different repositories (and superseded generations of the same repository)
// can never touch each other's output.
func newStagingDir(indexDir string, repoID uint32, generation int64) (string, error) {
	parent := filepath.Join(indexDir, stagingDirName)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	return os.MkdirTemp(parent, fmt.Sprintf("%d-%d-*", repoID, generation))
}

// planSwap inventories the staged and live files, plans backup paths and
// writes the swap journal. It performs no renames.
func planSwap(indexDir string, repoID uint32, generation int64, stagingDir string) (*publishJournal, error) {
	trashDir := filepath.Join(indexDir, trashDirName)
	journalDir := filepath.Join(indexDir, journalDirName)
	if err := os.MkdirAll(trashDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(journalDir, 0o755); err != nil {
		return nil, err
	}

	staged, err := listRegularFiles(stagingDir)
	if err != nil {
		return nil, fmt.Errorf("list staging directory %s: %w", stagingDir, err)
	}
	if len(staged) == 0 {
		return nil, fmt.Errorf("staging directory %s contains no index shards", stagingDir)
	}

	oldShards, err := listLiveShards(indexDir, repoID)
	if err != nil {
		return nil, err
	}

	journal := &publishJournal{
		RepoID:     repoID,
		Generation: generation,
		StagingDir: stagingDir,
	}

	remaining := make(map[string]struct{}, len(oldShards))
	for _, name := range oldShards {
		remaining[name] = struct{}{}
	}

	usedBackups := map[string]struct{}{}
	backupPath := func(final string) (string, error) {
		for i := 0; ; i++ {
			candidate := filepath.Join(trashDir,
				fmt.Sprintf("%s.%d.%d.bak", final, generation, i))
			if _, ok := usedBackups[candidate]; ok {
				continue
			}
			if _, err := os.Lstat(candidate); errors.Is(err, fs.ErrNotExist) {
				usedBackups[candidate] = struct{}{}
				return candidate, nil
			} else if err != nil {
				return "", err
			}
		}
	}

	for _, name := range staged {
		f := swapFile{Final: name}
		if _, ok := remaining[name]; ok {
			backup, err := backupPath(name)
			if err != nil {
				return nil, err
			}
			f.Backup = backup
			delete(remaining, name)
		}
		journal.Files = append(journal.Files, f)
	}
	for _, name := range oldShards {
		if _, ok := remaining[name]; !ok {
			continue
		}
		backup, err := backupPath(name)
		if err != nil {
			return nil, err
		}
		journal.Files = append(journal.Files, swapFile{Final: name, Backup: backup, OldOnly: true})
	}

	if err := writeJSONAtomic(journalPath(indexDir, repoID, generation), journal); err != nil {
		return nil, err
	}
	return journal, nil
}

// publishNewIndex atomically replaces the live shards of repoID with the
// freshly built files in stagingDir.
//
// The swap is journaled:
//  1. All previously live shard files are moved into .trash.
//  2. The staged files are renamed into place.
//  3. A commit marker is written (the commit point).
//  4. Backups and the journal are removed.
//
// A crash or failure before step 3 is rolled back, so the previous searchable
// index stays intact. A crash afterwards is finalized on the next start.
//
// The caller must have verified that generation is still the newest request
// for this repository before calling this function.
func publishNewIndex(indexDir string, repoID uint32, generation int64, stagingDir string) error {
	journal, err := planSwap(indexDir, repoID, generation, stagingDir)
	if err != nil {
		return err
	}
	journalFile := journalPath(indexDir, repoID, generation)

	committed := false
	defer func() {
		if !committed {
			// Failed or interrupted before the commit point: make sure the
			// previous index is back in place immediately. Any leftover state
			// is reconciled again at startup.
			if rbErr := rollbackSwap(indexDir, journal); rbErr != nil {
				log.Printf("dynamic-indexserver: rollback after failed publish for repo %d generation %d: %v",
					repoID, generation, rbErr)
			}
		}
	}()

	// Phase 1: move the current live set out of the way. Nothing new is
	// visible until the staged files have been moved into place below.
	if err := moveLiveToBackups(indexDir, journal); err != nil {
		return err
	}

	// Phase 2: place the new generation.
	if err := installStaged(indexDir, journal); err != nil {
		return err
	}

	// Phase 3: commit point.
	marker := journalFile + commitSuffix
	if err := writeAtomic(marker, nil); err != nil {
		return err
	}
	syncDir(indexDir)
	committed = true

	// Phase 4: the new generation is live; drop the previous one. Leftovers
	// are harmless and garbage collected at the next startup.
	for _, f := range journal.Files {
		if f.Backup == "" {
			continue
		}
		if err := os.Remove(f.Backup); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("dynamic-indexserver: failed to remove backup %s: %v", f.Backup, err)
		}
	}
	if err := os.RemoveAll(stagingDir); err != nil {
		log.Printf("dynamic-indexserver: failed to remove staging dir %s: %v", stagingDir, err)
	}
	// Forget the journal before the marker: a crash here leaves at worst an
	// orphan marker and leftover backups, both garbage collected at startup;
	// a present journal+marker always means "finalize", never "roll back".
	if err := os.Remove(journalFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("dynamic-indexserver: failed to remove journal %s: %v", journalFile, err)
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("dynamic-indexserver: failed to remove commit marker %s: %v", marker, err)
	}
	return nil
}

// moveLiveToBackups moves every currently live file recorded in the journal
// to its backup location (swap phase 1).
func moveLiveToBackups(indexDir string, journal *publishJournal) error {
	for _, f := range journal.Files {
		if f.Backup == "" {
			continue
		}
		if err := os.Rename(filepath.Join(indexDir, f.Final), f.Backup); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("back up shard %s: %w", f.Final, err)
		}
	}
	return nil
}

// installStaged renames the staged files into their live locations (swap
// phase 2). Old-only entries have no staged counterpart.
func installStaged(indexDir string, journal *publishJournal) error {
	for _, f := range journal.Files {
		if f.OldOnly {
			continue
		}
		if err := os.Rename(filepath.Join(journal.StagingDir, f.Final),
			filepath.Join(indexDir, f.Final)); err != nil {
			return fmt.Errorf("install shard %s: %w", f.Final, err)
		}
	}
	return nil
}

// rollbackSwap restores the shard set described by an uncommitted journal and
// removes every partial artifact of the attempt.
func rollbackSwap(indexDir string, journal *publishJournal) error {
	var errs []error

	for _, f := range journal.Files {
		finalPath := filepath.Join(indexDir, f.Final)

		if f.Backup == "" {
			// Nothing was moved away for this file, so anything at its final
			// location is a partially installed new file.
			if err := os.Remove(finalPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, fmt.Errorf("remove partial shard %s: %w", f.Final, err))
			}
			continue
		}

		if _, err := os.Stat(f.Backup); errors.Is(err, fs.ErrNotExist) {
			// The live file was never moved into the backup (or the backup
			// was discarded after commit, which never calls rollback), so the
			// current final location still holds the previous generation.
			continue
		} else if err != nil {
			errs = append(errs, fmt.Errorf("stat backup %s: %w", f.Backup, err))
			continue
		}

		// The previous generation is safely in the backup: discard any file
		// already installed at the final location and restore the backup.
		if err := os.Remove(finalPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove partial shard %s: %w", f.Final, err))
		}
		if err := os.Rename(f.Backup, finalPath); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			errs = append(errs, fmt.Errorf("restore shard %s: %w", f.Final, err))
		}
	}

	if journal.StagingDir != "" {
		if err := os.RemoveAll(journal.StagingDir); err != nil {
			errs = append(errs, fmt.Errorf("remove staging dir: %w", err))
		}
	}

	journalFile := journalPath(indexDir, journal.RepoID, journal.Generation)
	for _, p := range []string{journalFile, journalFile + commitSuffix} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove journal %s: %w", p, err))
		}
	}

	syncDir(indexDir)
	return errors.Join(errs...)
}

// finalizeSwap finishes a swap whose commit marker survived a process
// interruption: the new generation is live, so backups and staging data are
// discarded.
func finalizeSwap(journal *publishJournal) {
	for _, f := range journal.Files {
		if f.Backup == "" {
			continue
		}
		if err := os.Remove(f.Backup); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Printf("dynamic-indexserver: failed to remove backup %s: %v", f.Backup, err)
		}
	}
	if journal.StagingDir != "" {
		if err := os.RemoveAll(journal.StagingDir); err != nil {
			log.Printf("dynamic-indexserver: failed to remove staging dir %s: %v", journal.StagingDir, err)
		}
	}
}

// recoverInterruptedPublishes reconciles journals, staging directories and
// backups left behind by a crashed or killed process. It must run once at
// startup before any indexing worker is started.
func recoverInterruptedPublishes(indexDir string) error {
	journalDir := filepath.Join(indexDir, journalDirName)
	entries, err := os.ReadDir(journalDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read journal directory: %w", err)
	}

	var journals []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, journalPrefix) && strings.HasSuffix(name, journalSuffix) {
			journals = append(journals, filepath.Join(journalDir, name))
		}
	}
	sort.Strings(journals)

	var errs []error
	for _, journalFile := range journals {
		journal := &publishJournal{}
		data, err := os.ReadFile(journalFile)
		if err != nil {
			errs = append(errs, fmt.Errorf("read journal %s: %w", journalFile, err))
			continue
		}
		if err := json.Unmarshal(data, journal); err != nil {
			errs = append(errs, fmt.Errorf("parse journal %s: %w", journalFile, err))
			continue
		}

		if _, err := os.Stat(journalFile + commitSuffix); err == nil {
			log.Printf("dynamic-indexserver: finalizing interrupted publish for repo %d generation %d",
				journal.RepoID, journal.Generation)
			finalizeSwap(journal)
			if err := os.Remove(journalFile + commitSuffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			if err := os.Remove(journalFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		} else {
			log.Printf("dynamic-indexserver: rolling back interrupted publish for repo %d generation %d",
				journal.RepoID, journal.Generation)
			if err := rollbackSwap(indexDir, journal); err != nil {
				errs = append(errs, fmt.Errorf("rollback journal %s: %w", journalFile, err))
			}
		}
	}

	// Markers without a journal belong to swaps that were committed and then
	// interrupted while forgetting the journal; their shards are live, so the
	// markers themselves are garbage.
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasSuffix(name, commitSuffix) {
			if err := os.Remove(filepath.Join(journalDir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}

	// With all journals reconciled, anything left in the staging or trash
	// directories is orphaned (e.g. a process killed before journaling).
	if err := removeChildren(filepath.Join(indexDir, stagingDirName)); err != nil {
		errs = append(errs, err)
	}
	if err := removeChildren(filepath.Join(indexDir, trashDirName)); err != nil {
		errs = append(errs, err)
	}
	syncDir(indexDir)
	return errors.Join(errs...)
}

// listLiveShards returns the basenames of shards and shard sidecars currently
// live in indexDir for the repository with the given numeric ID. Shard names
// produced by this server are "<repoID>_v<format>.<n>.zoekt[.meta]", so the
// glob is scoped per repository and other repositories can never match.
func listLiveShards(indexDir string, repoID uint32) ([]string, error) {
	patterns := []string{
		fmt.Sprintf("%d_v*.zoekt", repoID),
		fmt.Sprintf("%d_v*.zoekt.meta", repoID),
	}

	var shards []string
	seen := map[string]struct{}{}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(filepath.Join(indexDir, pattern))
		if err != nil {
			return nil, fmt.Errorf("glob live shards %s: %w", pattern, err)
		}
		for _, match := range matches {
			info, err := os.Lstat(match)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return nil, err
			}
			if !info.Mode().IsRegular() {
				continue
			}
			name := filepath.Base(match)
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			shards = append(shards, name)
		}
	}
	sort.Strings(shards)
	return shards, nil
}

func listRegularFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)
	return files, nil
}

func removeChildren(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// writeAtomic writes data to path via a temp file in the same directory and
// an atomic rename, so readers never observe a partially written file.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeAtomic(path, data)
}

// syncDir best-effort fsyncs a directory so that renames within it are
// durable. Not all filesystems support this; errors are ignored.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}
