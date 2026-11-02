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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/gitindex"
	"github.com/sourcegraph/zoekt/index"
)

type shardInfo struct {
	Path       string
	Repository *zoekt.Repository
}

type repositoryKey struct {
	Name   string
	Source string
}

type repositoryRecord struct {
	Name   string
	Source string
	Shards []string
}

type pruneAction struct {
	Shard  string
	Name   string
	Source string
	Reason string
}

// shardGroup is the set of live shard files carrying one (repository name,
// source) identity. A moved repository leaves one group behind and requires
// another to be published.
type shardGroup struct {
	Name   string
	Source string
	ID     uint32
	Shards []shardInfo
}

func readInventory(indexDir string) ([]shardInfo, error) {
	entries, err := os.ReadDir(indexDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read index directory %s: %w", indexDir, err)
	}

	var shards []shardInfo
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".zoekt" {
			continue
		}
		path := filepath.Join(indexDir, entry.Name())
		repositories, _, err := index.ReadMetadataPathAlive(path)
		if err != nil {
			return nil, fmt.Errorf("read metadata from %s: %w", path, err)
		}
		if len(repositories) != 1 {
			return nil, fmt.Errorf("index shard %s contains %d repositories, want 1", path, len(repositories))
		}
		shards = append(shards, shardInfo{Path: path, Repository: repositories[0]})
	}
	return shards, nil
}

func normalizeSource(source string) string {
	if source == "" {
		return ""
	}
	if abs, err := filepath.Abs(source); err == nil {
		source = abs
	} else {
		source = filepath.Clean(source)
	}
	if filepath.Base(source) == ".git" {
		source = filepath.Dir(source)
	}
	return source
}

func groupShards(shards []shardInfo) []shardGroup {
	type groupKey struct {
		name   string
		source string
	}
	byKey := map[groupKey]*shardGroup{}
	for _, shard := range shards {
		key := groupKey{
			name:   shard.Repository.Name,
			source: normalizeSource(shard.Repository.Source),
		}
		group := byKey[key]
		if group == nil {
			group = &shardGroup{
				Name:   key.name,
				Source: key.source,
				ID:     shard.Repository.ID,
			}
			byKey[key] = group
		}
		group.Shards = append(group.Shards, shard)
	}

	groups := make([]shardGroup, 0, len(byKey))
	for _, group := range byKey {
		sort.Slice(group.Shards, func(i, j int) bool { return group.Shards[i].Path < group.Shards[j].Path })
		groups = append(groups, *group)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Name != groups[j].Name {
			return groups[i].Name < groups[j].Name
		}
		return groups[i].Source < groups[j].Source
	})
	return groups
}

type repoAction int

const (
	// actionProbe needs an incremental check against the live same-source
	// shards before deciding whether the cache is reused.
	actionProbe repoAction = iota
	// actionIndex builds the first cache for a repository.
	actionIndex
	// actionReplace stages a new generation and retires every live group
	// carrying the repository name.
	actionReplace
)

type repoTask struct {
	spec     repositorySpec
	action   repoAction
	matching *shardGroup
	others   []shardGroup
}

// planSync decides what to do with every desired repository and every live
// shard group. Groups whose repository name is no longer desired are pruned;
// a moved repository (same name, new source) is never pruned ahead of the
// rebuild, but scheduled for an atomic replacement.
func planSync(desired []repositorySpec, groups []shardGroup) ([]pruneAction, []repoTask) {
	desiredByName := map[string]repositorySpec{}
	desiredBySource := map[string]repositorySpec{}
	for _, repo := range desired {
		desiredByName[repo.Name] = repo
		desiredBySource[normalizeSource(repo.Source)] = repo
	}

	groupsByName := map[string][]shardGroup{}
	for _, group := range groups {
		groupsByName[group.Name] = append(groupsByName[group.Name], group)
	}

	var removals []pruneAction
	for _, group := range groups {
		if _, ok := desiredByName[group.Name]; ok {
			continue
		}
		reason := "repository is no longer selected"
		if renamed, ok := desiredBySource[group.Source]; ok {
			reason = fmt.Sprintf("repository is now named %q", renamed.Name)
		}
		for _, shard := range group.Shards {
			removals = append(removals, pruneAction{
				Shard:  shard.Path,
				Name:   group.Name,
				Source: group.Source,
				Reason: reason,
			})
		}
	}
	sort.Slice(removals, func(i, j int) bool { return removals[i].Shard < removals[j].Shard })

	tasks := make([]repoTask, 0, len(desired))
	for _, repo := range desired {
		live := groupsByName[repo.Name]
		declaredSource := normalizeSource(repo.Source)

		var matching *shardGroup
		var others []shardGroup
		for i := range live {
			if live[i].Source == declaredSource {
				group := live[i]
				matching = &group
			} else {
				others = append(others, live[i])
			}
		}

		action := actionReplace
		if matching == nil && len(live) == 0 {
			action = actionIndex
		} else if matching != nil {
			action = actionProbe
		}
		tasks = append(tasks, repoTask{
			spec:     repo,
			action:   action,
			matching: matching,
			others:   others,
		})
	}
	return removals, tasks
}

func removeShard(path string) error {
	paths, err := index.IndexFilePaths(path)
	if err != nil {
		return err
	}
	var errs []error
	// Remove the optional metadata sidecar first. If deletion then fails, the
	// shard remains self-consistent and can be retried on the next sync.
	for _, path := range slices.Backward(paths) {

		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func applyRemovals(actions []pruneAction, dryRun bool, out io.Writer) error {
	var errs []error
	for _, action := range actions {
		if dryRun {
			fmt.Fprintf(out, "Would remove %s (repository %q, source %s: %s)\n", action.Shard, action.Name, action.Source, action.Reason)
			continue
		}
		fmt.Fprintf(out, "Removing %s (repository %q, source %s: %s)\n", action.Shard, action.Name, action.Source, action.Reason)
		if err := removeShard(action.Shard); err != nil {
			errs = append(errs, fmt.Errorf("remove shard %s: %w", action.Shard, err))
		}
	}
	return errors.Join(errs...)
}

// probeRepository reports whether the live same-source cache for spec is
// stale. It never writes to the index.
func probeRepository(spec repositorySpec, opts gitindex.Options, indexDir string) (bool, error) {
	probeOpts := opts
	probeOpts.RepoDir = spec.Source
	probeOpts.DryRun = true
	probeOpts.Incremental = true
	probeOpts.BuildOptions = opts.BuildOptions
	probeOpts.BuildOptions.IndexDir = indexDir
	probeOpts.BuildOptions.RepositoryDescription = zoekt.Repository{Name: spec.Name}
	return gitindex.IndexGitRepo(probeOpts)
}

// parseShardNumber extracts the format version and shard number from a shard
// basename.
func parseShardNumber(basename string) (version, number int, ok bool) {
	match := shardNumberRegexp.FindStringSubmatch(basename)
	if match == nil {
		return 0, 0, false
	}
	fmt.Sscanf(match[1], "%d", &version)
	fmt.Sscanf(match[2], "%d", &number)
	return version, number, true
}

// canConsolidate reports whether the matching same-source group is a complete
// shard set by itself, so the other-source groups can be retired without a
// rebuild. The matching shards must be one format version numbered
// contiguously from zero; stale extras may only carry higher numbers.
func canConsolidate(matching shardGroup, others []shardGroup) bool {
	if len(matching.Shards) == 0 {
		return false
	}
	version := -1
	var numbers []int
	for _, shard := range matching.Shards {
		v, n, ok := parseShardNumber(filepath.Base(shard.Path))
		if !ok {
			return false
		}
		if version == -1 {
			version = v
		} else if version != v {
			return false
		}
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)
	for i, n := range numbers {
		if n != i {
			return false
		}
	}
	for _, group := range others {
		for _, shard := range group.Shards {
			_, n, ok := parseShardNumber(filepath.Base(shard.Path))
			if !ok || n < len(numbers) {
				return false
			}
		}
	}
	return true
}

func groupArtifacts(groups []shardGroup) (files, sources []string, ids map[uint32]struct{}, err error) {
	fileSet := map[string]struct{}{}
	sourceSet := map[string]struct{}{}
	ids = map[uint32]struct{}{}
	for _, group := range groups {
		sourceSet[group.Source] = struct{}{}
		if group.ID != 0 {
			ids[group.ID] = struct{}{}
		}
		for _, shard := range group.Shards {
			paths, err := index.IndexFilePaths(shard.Path)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("resolve artifacts of %s: %w", shard.Path, err)
			}
			for _, path := range paths {
				fileSet[filepath.Base(path)] = struct{}{}
			}
		}
	}
	for file := range fileSet {
		files = append(files, file)
	}
	for source := range sourceSet {
		sources = append(sources, source)
	}
	sort.Strings(files)
	sort.Strings(sources)
	return files, sources, ids, nil
}

func formatSources(sources []string) string {
	return strings.Join(sources, ", ")
}

// executeReplacement builds the new generation in a private staging
// directory and only switches after it has been verified. The previous
// generation remains searchable until the switch and is restored if anything
// goes wrong.
func executeReplacement(indexDir string, task repoTask, oldGroups []shardGroup, opts gitindex.Options, out io.Writer) error {
	r, err := newReplacement(indexDir)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = r.discard()
		}
	}()

	oldFiles, oldSources, oldIDs, err := groupArtifacts(oldGroups)
	if err != nil {
		return err
	}
	r.journal.OldFiles = oldFiles
	r.journal.OldSources = oldSources

	fmt.Fprintf(out, "Indexing %q from %s\n", task.spec.Name, task.spec.Source)
	if err := r.build(task.spec, opts); err != nil {
		return err
	}
	if r.journal.ID != 0 {
		for oldID := range oldIDs {
			if oldID != r.journal.ID {
				return fmt.Errorf("conflict: repository %q changed ID from %d to %d; keeping the previous searchable cache", task.spec.Name, oldID, r.journal.ID)
			}
		}
	}
	if err := r.commit(out); err != nil {
		return err
	}
	committed = true
	fmt.Fprintf(out, "Indexed %q from %s\n", task.spec.Name, task.spec.Source)
	return nil
}

// executeConsolidation retires stale other-source groups while adopting the
// complete same-source group already live.
func executeConsolidation(indexDir string, task repoTask, opts gitindex.Options, out io.Writer) error {
	r, err := newReplacement(indexDir)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = r.discard()
		}
	}()

	keepFiles, _, keepIDs, err := groupArtifacts([]shardGroup{*task.matching})
	if err != nil {
		return err
	}
	oldFiles, oldSources, _, err := groupArtifacts(task.others)
	if err != nil {
		return err
	}
	r.journal.Name = task.spec.Name
	r.journal.ID = task.matching.ID
	r.journal.NewSource = normalizeSource(task.spec.Source)
	r.journal.KeepFiles = keepFiles
	r.journal.OldFiles = oldFiles
	r.journal.OldSources = oldSources
	for id := range keepIDs {
		r.journal.ID = id
	}
	if err := r.commit(out); err != nil {
		return err
	}
	committed = true
	fmt.Fprintf(out, "Consolidated %q onto %s; retired stale cache from %s\n", task.spec.Name, r.journal.NewSource, formatSources(oldSources))
	return nil
}

func synchronizeOne(indexDir string, task repoTask, opts gitindex.Options, dryRun bool, out io.Writer) error {
	declared := task.spec
	declaredSource := normalizeSource(declared.Source)
	liveGroups := task.others
	if task.matching != nil {
		liveGroups = append(liveGroups, *task.matching)
	}

	switch task.action {
	case actionIndex:
		if dryRun {
			fmt.Fprintf(out, "Would index %q from %s\n", declared.Name, declared.Source)
			return nil
		}
		return executeReplacement(indexDir, task, nil, opts, out)

	case actionReplace:
		oldSources := sourcesOf(liveGroups)
		if dryRun {
			fmt.Fprintf(out, "Would replace cached source %s for %q with %s\n", formatSources(oldSources), declared.Name, declaredSource)
			fmt.Fprintf(out, "Would index %q from %s\n", declared.Name, declared.Source)
			return nil
		}
		return executeReplacement(indexDir, task, liveGroups, opts, out)
	}

	// actionProbe: a same-source group is live. Check whether it is current.
	updated, err := probeRepository(declared, opts, indexDir)
	if err != nil {
		return fmt.Errorf("check %q from %s: %w", declared.Name, declared.Source, err)
	}
	if !updated {
		if len(task.others) == 0 {
			fmt.Fprintf(out, "Up to date %q from %s\n", declared.Name, declared.Source)
			return nil
		}
		if canConsolidate(*task.matching, task.others) {
			oldSources := sourcesOf(task.others)
			if dryRun {
				fmt.Fprintf(out, "Would remove stale cache for %q from %s\n", declared.Name, formatSources(oldSources))
				return nil
			}
			return executeConsolidation(indexDir, task, opts, out)
		}
	}

	if dryRun {
		fmt.Fprintf(out, "Would index %q from %s\n", declared.Name, declared.Source)
		return nil
	}
	return executeReplacement(indexDir, task, liveGroups, opts, out)
}

func sourcesOf(groups []shardGroup) []string {
	seen := map[string]struct{}{}
	var sources []string
	for _, group := range groups {
		if _, ok := seen[group.Source]; !ok {
			seen[group.Source] = struct{}{}
			sources = append(sources, group.Source)
		}
	}
	sort.Strings(sources)
	return sources
}

// synchronizeRepositories prunes repositories no longer selected and brings
// every desired repository's cache through a recoverable replacement.
func synchronizeRepositories(indexDir string, repositories []repositorySpec, opts gitindex.Options, dryRun bool, out io.Writer) error {
	shards, err := readInventory(indexDir)
	if err != nil {
		return err
	}
	removals, tasks := planSync(repositories, groupShards(shards))
	if err := applyRemovals(removals, dryRun, out); err != nil {
		return err
	}

	var errs []error
	for _, task := range tasks {
		if err := synchronizeOne(indexDir, task, opts, dryRun, out); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func recordsFromShards(shards []shardInfo) []repositoryRecord {
	records := map[repositoryKey]*repositoryRecord{}
	for _, shard := range shards {
		repo := shard.Repository
		key := repositoryKey{Name: repo.Name, Source: normalizeSource(repo.Source)}
		record := records[key]
		if record == nil {
			record = &repositoryRecord{Name: key.Name, Source: key.Source}
			records[key] = record
		}
		record.Shards = append(record.Shards, shard.Path)
	}

	result := make([]repositoryRecord, 0, len(records))
	for _, record := range records {
		sort.Strings(record.Shards)
		result = append(result, *record)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name == result[j].Name {
			return result[i].Source < result[j].Source
		}
		return result[i].Name < result[j].Name
	})
	return result
}

func listRepositories(indexDir string, out io.Writer) error {
	shards, err := readInventory(indexDir)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(out, 8, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSOURCE\tSHARDS")
	for _, record := range recordsFromShards(shards) {
		fmt.Fprintf(w, "%s\t%s\t%s\n", record.Name, record.Source, strings.Join(record.Shards, ","))
	}
	return w.Flush()
}

func selectRecords(records []repositoryRecord, selectors []string) ([]repositoryRecord, error) {
	selected := map[repositoryKey]repositoryRecord{}
	for _, selector := range selectors {
		var matches []repositoryRecord
		for _, record := range records {
			if record.Name == selector {
				matches = append(matches, record)
			}
		}
		if len(matches) == 0 {
			source := normalizeSource(selector)
			for _, record := range records {
				if record.Source != "" && record.Source == source {
					matches = append(matches, record)
				}
			}
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("repository %q not found", selector)
		}
		if len(matches) > 1 {
			return nil, fmt.Errorf("repository selector %q is ambiguous; use an exact source path", selector)
		}
		match := matches[0]
		selected[repositoryKey{Name: match.Name, Source: match.Source}] = match
	}

	result := make([]repositoryRecord, 0, len(selected))
	for _, record := range selected {
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name == result[j].Name {
			return result[i].Source < result[j].Source
		}
		return result[i].Name < result[j].Name
	})
	return result, nil
}

func removeRepositories(indexDir string, selectors []string, dryRun bool, out io.Writer) error {
	shards, err := readInventory(indexDir)
	if err != nil {
		return err
	}
	records, err := selectRecords(recordsFromShards(shards), selectors)
	if err != nil {
		return err
	}

	var actions []pruneAction
	for _, record := range records {
		for _, shardPath := range record.Shards {
			actions = append(actions, pruneAction{
				Shard:  shardPath,
				Name:   record.Name,
				Source: record.Source,
				Reason: "explicitly selected",
			})
		}
	}

	sort.Slice(actions, func(i, j int) bool { return actions[i].Shard < actions[j].Shard })
	return applyRemovals(actions, dryRun, out)
}
