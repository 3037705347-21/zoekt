// Copyright 2016 Google Inc. All rights reserved.
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

package gitindex

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/sourcegraph/zoekt/ignore"
)

// RepoWalker walks one or more commit trees, collecting the files to index in its Files map.
//
// It also recurses into submodules if Options.Submodules is enabled.
type RepoWalker struct {
	Files map[fileKey]BlobLocation

	repo    *git.Repository
	repoURL *url.URL

	// Path => SubmoduleEntry
	submodules map[string]*SubmoduleEntry
	repoCache  *RepoCache

	// roster, when set, persists last-known-good submodule content for the
	// top-level repository. A submodule that cannot be read is served from
	// its roster snapshot; submodules for which no snapshot exists, or whose
	// attribution cannot be confirmed, fail the whole walk. It is always nil
	// on walkers recursing into a submodule.
	roster *submoduleRoster
}

// subURL returns the URL for a submodule.
func (rw *RepoWalker) subURL(relURL string) (*url.URL, error) {
	if rw.repoURL == nil {
		return nil, fmt.Errorf("no URL for base repo")
	}
	if strings.HasPrefix(relURL, "../") {
		u := *rw.repoURL
		u.Path = path.Join(u.Path, relURL)
		return &u, nil
	}

	return url.Parse(relURL)
}

// NewRepoWalker creates a new RepoWalker.
func NewRepoWalker(r *git.Repository, repoURL string, repoCache *RepoCache) *RepoWalker {
	u, _ := url.Parse(repoURL)
	return &RepoWalker{
		repo:      r,
		repoURL:   u,
		Files:     map[fileKey]BlobLocation{},
		repoCache: repoCache,
	}
}

// parseModuleMap initializes rw.submodules.
func (rw *RepoWalker) parseModuleMap(t *object.Tree) error {
	if rw.repoCache == nil {
		return nil
	}
	modEntry, _ := t.File(".gitmodules")
	if modEntry != nil {
		c, err := blobContents(&modEntry.Blob)
		if err != nil {
			return fmt.Errorf("blobContents: %w", err)
		}
		mods, err := ParseGitModules(c)
		if err != nil {
			return fmt.Errorf("ParseGitModules: %w", err)
		}
		rw.submodules = map[string]*SubmoduleEntry{}
		for _, entry := range mods {
			rw.submodules[entry.Path] = entry
		}
	}
	return nil
}

// CollectFiles fetches the blob SHA1s for the tree. If repoCache is
// non-nil, recurse into submodules. In addition, it returns a mapping
// that indicates in which repo each SHA1 can be found.
//
// Files matched by the tree's own ignore file are skipped. Each submodule is
// filtered by its own ignore file, since its paths are relative to its root.
//
// The collected files are available through the RepoWalker.Files map.
func (rw *RepoWalker) CollectFiles(t *object.Tree, branch string) (map[string]plumbing.Hash, error) {
	if err := rw.parseModuleMap(t); err != nil {
		return nil, fmt.Errorf("parseModuleMap: %w", err)
	}

	ig, err := newIgnoreMatcher(t)
	if err != nil {
		return nil, fmt.Errorf("newIgnoreMatcher: %w", err)
	}

	tw := object.NewTreeWalker(t, true, make(map[plumbing.Hash]bool))
	defer tw.Close()

	// Path => commit SHA1
	subRepoVersions := make(map[string]plumbing.Hash)
	skippedTrees := 0
	for {
		name, entry, err := tw.Next()
		if err == io.EOF {
			if name == "" {
				break
			}
			// go-git returns io.EOF with the subtree's name when it cannot
			// read a subtree, after moving past it, so keep walking. Without
			// the ignore file we would index paths the repo excludes.
			if name == path.Dir(ignore.IgnoreFile) {
				return nil, fmt.Errorf("cannot read tree %s (%s)", name, entry.Hash)
			}
			skippedTrees++
			continue
		}
		if err == object.ErrMaxTreeDepth {
			return nil, fmt.Errorf("TreeWalker.Next: %w", err)
		}
		if err := rw.handleEntry(name, &entry, branch, subRepoVersions, ig); err != nil {
			return nil, fmt.Errorf("handleEntry: %w", err)
		}
	}
	if skippedTrees > 0 {
		log.Printf("skipped %d trees with missing Git objects on branch %s", skippedTrees, branch)
	}
	return subRepoVersions, nil
}

func (rw *RepoWalker) tryHandleSubmodule(p string, id *plumbing.Hash, branch string, subRepoVersions map[string]plumbing.Hash, ig *ignore.Matcher) error {
	if err := rw.handleSubmodule(p, id, branch, subRepoVersions, ig); err != nil {
		// With a roster in use, an unavailable submodule without a usable
		// snapshot, or one whose ownership cannot be confirmed, must abort
		// the update instead of silently dropping the submodule's files.
		if rw.roster != nil && (errors.Is(err, errSubmoduleUnavailable) || errors.Is(err, errSubmoduleUnattributable)) {
			return err
		}
		log.Printf("submodule %s: ignoring error %v", p, err)
	}
	return nil
}

func (rw *RepoWalker) handleSubmodule(p string, id *plumbing.Hash, branch string, subRepoVersions map[string]plumbing.Hash, ig *ignore.Matcher) error {
	submod := rw.submodules[p]
	if submod == nil {
		// The tree references a submodule but .gitmodules does not describe
		// it, so its content cannot be attributed to this repository.
		if rw.roster != nil {
			return fmt.Errorf("%w: gitlink at %q has no .gitmodules entry", errSubmoduleUnattributable, p)
		}
		return fmt.Errorf("no entry for submodule path %q", rw.repoURL)
	}

	subURL, err := rw.subURL(submod.URL)
	if err != nil {
		if rw.roster != nil {
			return fmt.Errorf("%w: submodule %q at %q has unusable URL %q: %v",
				errSubmoduleUnattributable, submod.Name, p, submod.URL, err)
		}
		return err
	}

	subRepo, err := rw.repoCache.Open(subURL)
	if err != nil {
		return rw.useRosterSnapshot(submod, p, subURL, id, branch, subRepoVersions, err)
	}

	obj, err := subRepo.CommitObject(*id)
	if err != nil {
		return rw.useRosterSnapshot(submod, p, subURL, id, branch, subRepoVersions, err)
	}
	tree, err := subRepo.TreeObject(obj.TreeHash)
	if err != nil {
		return rw.useRosterSnapshot(submod, p, subURL, id, branch, subRepoVersions, err)
	}

	subRepoVersions[p] = *id

	sw := NewRepoWalker(subRepo, subURL.String(), rw.repoCache)
	subVersions, err := sw.CollectFiles(tree, branch)
	if err != nil {
		return rw.useRosterSnapshot(submod, p, subURL, id, branch, subRepoVersions, err)
	}

	if rw.roster != nil {
		// The same submodule is visited once per indexed branch; capture
		// its snapshot on the first successful visit.
		if _, captured := rw.roster.fresh[submod.Name]; !captured {
			snapshot, err := captureSubmoduleSnapshot(sw.Files)
			if err != nil {
				return fmt.Errorf("capture content of submodule %q at %q: %w", submod.Name, p, err)
			}
			rw.roster.noteFresh(submod.Name, p, submod.URL, id.String(), snapshot)
		}
	}

	for k, repo := range sw.Files {
		rw.Files[fileKey{
			SubRepoPath: filepath.Join(p, k.SubRepoPath),
			Path:        k.Path,
			ID:          k.ID,
		}] = repo
	}
	for k, v := range subVersions {
		subRepoVersions[filepath.Join(p, k)] = v
	}
	return nil
}

// useRosterSnapshot is called when a submodule attributed at p cannot be read.
// Without a roster it returns cause unchanged (the caller logs and skips the
// submodule, the historical behavior). With a roster it restores the last
// successfully indexed content, or returns errSubmoduleUnavailable when no
// trusted snapshot exists so the whole index update is abandoned.
func (rw *RepoWalker) useRosterSnapshot(submod *SubmoduleEntry, p string, subURL *url.URL, id *plumbing.Hash, branch string, subRepoVersions map[string]plumbing.Hash, cause error) error {
	if rw.roster == nil {
		return cause
	}

	entry := rw.roster.previous(submod.Name)
	if entry == nil {
		return fmt.Errorf("%w: submodule %q at %q cannot be read (%v) and no last-known-good content is saved",
			errSubmoduleUnavailable, submod.Name, p, cause)
	}
	// A previously moved submodule must be restored at its CURRENT tree
	// path; the snapshot stores paths relative to the submodule root.
	if entry.Path != "" && entry.Path != p {
		log.Printf("submodule %q moved from %q to %q; restoring saved content at the new path", submod.Name, entry.Path, p)
	}
	snapshot, err := rw.roster.loadSnapshot(entry)
	if err != nil {
		return fmt.Errorf("%w: cannot restore submodule %q at %q from saved content: %v",
			errSubmoduleUnavailable, submod.Name, p, err)
	}

	injectSubmoduleSnapshot(rw.Files, p, subURL, []string{branch}, snapshot)
	subRepoVersions[p] = *id
	rw.roster.noteReused(submod.Name, p, submod.URL, id.String())
	log.Printf("submodule %q at %q is temporarily unreadable (%v); serving its last-known-good content",
		submod.Name, p, cause)
	return nil
}

func (rw *RepoWalker) handleEntry(p string, e *object.TreeEntry, branch string, subRepoVersions map[string]plumbing.Hash, ig *ignore.Matcher) error {
	if e.Mode == filemode.Submodule {
		if rw.repoCache != nil {
			// Index the submodule using repo cache
			if err := rw.tryHandleSubmodule(p, &e.Hash, branch, subRepoVersions, ig); err != nil {
				return fmt.Errorf("submodule %s: %v", p, err)
			}
		} else {
			// Record the commit ID for the submodule path
			// This will be the submodule's commit hash, not the parent's
			subRepoVersions[p] = e.Hash
		}
	}

	switch e.Mode {
	case filemode.Regular, filemode.Executable, filemode.Symlink:
	default:
		return nil
	}

	// Skip ignored files
	if ig.Match(p) {
		return nil
	}

	key := fileKey{Path: p, ID: e.Hash}
	if existing, ok := rw.Files[key]; ok {
		existing.Branches = append(existing.Branches, branch)
		rw.Files[key] = existing
	} else {
		rw.Files[key] = BlobLocation{GitRepo: rw.repo, URL: rw.repoURL, Branches: []string{branch}}
	}

	return nil
}

// fileKey describes a blob at a location in the final tree. We also
// record the subrepository from where it came.
type fileKey struct {
	SubRepoPath string
	Path        string
	ID          plumbing.Hash
}

func (k *fileKey) FullPath() string {
	return filepath.Join(k.SubRepoPath, k.Path)
}

// BlobLocation holds the repo where the blob can be found, plus other information
// needed for indexing like its branches.
type BlobLocation struct {
	GitRepo *git.Repository
	URL     *url.URL

	// Branches is the list of branches that contain the blob.
	Branches []string

	// inline carries the blob contents restored from the submodule roster
	// when the submodule itself is unreadable. When inline is non-nil the
	// blob is read from here instead of from GitRepo.
	inline []byte
	// inlineMissing reproduces a blob that the captured submodule could not
	// provide; the document is emitted as skipped instead of being dropped.
	inlineMissing bool
}

func (l *BlobLocation) Blob(id *plumbing.Hash) ([]byte, error) {
	blob, err := l.GitRepo.BlobObject(*id)
	if err != nil {
		return nil, err
	}
	return blobContents(blob)
}
