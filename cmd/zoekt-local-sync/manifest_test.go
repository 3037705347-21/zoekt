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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/internal/testutil"
	"github.com/sourcegraph/zoekt/query"
	"github.com/sourcegraph/zoekt/search"
)

func writeManifest(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func manifestXML(projects ...string) string {
	return "<manifest>\n" + strings.Join(projects, "\n") + "\n</manifest>\n"
}

func listRecords(t *testing.T, indexDir string) []repositoryRecord {
	t.Helper()
	shards, err := readInventory(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	return recordsFromShards(shards)
}

func recordNames(records []repositoryRecord) []string {
	names := make([]string, 0, len(records))
	for _, record := range records {
		names = append(names, record.Name)
	}
	return names
}

func runManifestSync(t *testing.T, indexDir, manifestPath, root string, extra ...string) (string, error) {
	t.Helper()
	args := append([]string{"-index", indexDir, "-manifest", manifestPath,
		"-disable_ctags", "-submodules=false"}, extra...)
	args = append(args, root)
	var out bytes.Buffer
	err := execute(args, &out, &bytes.Buffer{})
	return out.String(), err
}

func searchIndex(t *testing.T, indexDir, queryString string, wantRepo string) {
	t.Helper()
	ss, err := search.NewDirectorySearcher(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()

	q, err := query.Parse(queryString)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ss.Search(context.Background(), q, &zoekt.SearchOptions{})
	if err != nil {
		t.Fatalf("search %q: %v", queryString, err)
	}
	if len(result.Files) == 0 {
		t.Fatalf("search %q returned no files", queryString)
	}
	for _, file := range result.Files {
		if file.Repository == wantRepo {
			return
		}
	}
	t.Fatalf("search %q did not match repository %q (matched %v)", queryString, wantRepo, result.Files)
}

func assertIndexAbsent(t *testing.T, indexDir, queryString string) {
	t.Helper()
	ss, err := search.NewDirectorySearcher(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()

	q, err := query.Parse(queryString)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ss.Search(context.Background(), q, &zoekt.SearchOptions{})
	if err != nil {
		t.Fatalf("search %q: %v", queryString, err)
	}
	if len(result.Files) != 0 {
		t.Fatalf("search %q returned %d files, want none: %v", queryString, len(result.Files), result.Files)
	}
}

func TestLoadManifestProjects(t *testing.T) {
	dir := t.TempDir()
	includePath := filepath.Join(dir, "included.xml")
	writeManifest(t, includePath, manifestXML(
		`<project name="platform/included" path="included"/>`,
		`<project name="platform/notdefault" groups="notdefault"/>`,
	))
	mainPath := filepath.Join(dir, "default.xml")
	writeManifest(t, mainPath, `
<manifest>
  <project name="platform/build"/>
  <project name="platform/relocated" path="build/moved"/>
  <include name="included.xml"/>
  <project name="platform/temporary"/>
  <remove-project name="platform/temporary"/>
  <project name="platform/extended"/>
  <extend-project name="platform/extended" path="custom/extended"/>
</manifest>`)

	projects, err := loadManifestProjects(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	var names []string
	for _, project := range projects {
		got[project.Name] = project.Path
		names = append(names, project.Name)
	}
	want := map[string]string{
		"platform/build":     "platform/build",
		"platform/relocated": "build/moved",
		"platform/included":  "included",
		"platform/extended":  "custom/extended",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("unexpected manifest projects (-want +got):\n%s", diff)
	}
	if _, ok := got["platform/notdefault"]; ok {
		t.Fatalf("notdefault project was included: %v", names)
	}
	if _, ok := got["platform/temporary"]; ok {
		t.Fatalf("removed project was included: %v", names)
	}
}

func TestLoadManifestProjectsErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{name: "malformed", content: "<manifest><project"},
		{name: "duplicate project", content: manifestXML(
			`<project name="p"/>`, `<project name="p"/>`)},
		{name: "remove missing", content: manifestXML(`<remove-project name="p"/>`)},
		{name: "extend missing", content: manifestXML(`<extend-project name="p"/>`)},
		{name: "project without name", content: manifestXML(`<project path="p"/>`)},
		{name: "escaping path", content: manifestXML(`<project name="p" path="../p"/>`)},
		{name: "absolute path", content: manifestXML(`<project name="p" path="/tmp/p"/>`)},
		{name: "missing include", content: manifestXML(`<include name="absent.xml"/>`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "default.xml")
			writeManifest(t, path, tc.content)
			if _, err := loadManifestProjects(path); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestResolveManifestPath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "default.xml")
	writeManifest(t, file, manifestXML())

	resolved, err := resolveManifestPath(file)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != file {
		t.Fatalf("got %q, want %q", resolved, file)
	}

	manifestDir := filepath.Join(dir, "repo")
	writeManifest(t, filepath.Join(manifestDir, "manifest.xml"), manifestXML())
	resolved, err = resolveManifestPath(manifestDir)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(manifestDir, "manifest.xml"); resolved != want {
		t.Fatalf("got %q, want %q", resolved, want)
	}

	clientRoot := filepath.Join(dir, "client")
	writeManifest(t, filepath.Join(clientRoot, ".repo", "manifest.xml"), manifestXML())
	resolved, err = resolveManifestPath(clientRoot)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(clientRoot, ".repo", "manifest.xml"); resolved != want {
		t.Fatalf("got %q, want %q", resolved, want)
	}

	if _, err := resolveManifestPath(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("expected error for missing manifest")
	}
}

func TestDiscoverManifestRepositoriesUsesStableProjectNames(t *testing.T) {
	root := t.TempDir()
	createGitRepository(t, filepath.Join(root, "build", "moved"))
	// A leftover checkout from an older release branch: present on disk but
	// absent from the manifest. It must never be selected.
	createGitRepository(t, filepath.Join(root, "stale", "leftover"))

	manifestPath := filepath.Join(root, "manifest.xml")
	writeManifest(t, manifestPath, manifestXML(
		`<project name="platform/build" path="build/moved"/>`,
	))

	repositories, err := discoverManifestRepositories(manifestPath, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"platform/build"}, func() []string {
		var names []string
		for _, repo := range repositories {
			names = append(names, repo.Name)
		}
		return names
	}()); diff != "" {
		t.Fatalf("unexpected repositories (-want +got):\n%s", diff)
	}

	wantSource, err := filepath.EvalSymlinks(filepath.Join(root, "build", "moved"))
	if err != nil {
		t.Fatal(err)
	}
	if got := repositories[0].Source; got != wantSource {
		t.Fatalf("got source %q, want %q", got, wantSource)
	}
}

func TestDiscoverManifestRepositoriesMissingProject(t *testing.T) {
	root := t.TempDir()
	manifestPath := filepath.Join(root, "manifest.xml")
	writeManifest(t, manifestPath, manifestXML(
		`<project name="platform/missing"/>`,
	))
	if _, err := discoverManifestRepositories(manifestPath, []string{root}); err == nil {
		t.Fatal("expected error when a manifest project is not checked out")
	}
}

func TestPlanManifestPruneKeepsMovedProjects(t *testing.T) {
	desired := []repositorySpec{{Name: "platform/build", Source: "/new/path"}}
	shards := []shardInfo{
		{Path: "/idx/build.zoekt", Repository: &zoekt.Repository{Name: "platform/build", Source: "/old/path"}},
		{Path: "/idx/gone.zoekt", Repository: &zoekt.Repository{Name: "platform/gone", Source: "/gone"}},
	}

	actions := planManifestPrune(desired, shards)
	if diff := cmp.Diff([]string{"/idx/gone.zoekt"}, func() []string {
		var paths []string
		for _, action := range actions {
			paths = append(paths, action.Shard)
		}
		return paths
	}()); diff != "" {
		t.Fatalf("unexpected prune actions (-want +got):\n%s", diff)
	}

	moves := movedRepositories(desired, shards)
	if len(moves) != 1 || moves[0].Name != "platform/build" ||
		moves[0].OldSource != "/old/path" || moves[0].NewSource != "/new/path" {
		t.Fatalf("unexpected moves: %+v", moves)
	}
}

func TestSyncManifestIndexesStableNames(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "build", "moved")
	createGitRepository(t, projectPath)
	indexDir := t.TempDir()
	manifestPath := filepath.Join(root, "manifest.xml")
	writeManifest(t, manifestPath, manifestXML(
		`<project name="platform/build" path="build/moved"/>`,
	))

	out, err := runManifestSync(t, indexDir, manifestPath, root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `Would index "platform/build"`) {
		t.Fatalf("preview did not name the stable project:\n%s", out)
	}

	if _, err := runManifestSync(t, indexDir, manifestPath, root, "-f"); err != nil {
		t.Fatal(err)
	}
	records := listRecords(t, indexDir)
	if diff := cmp.Diff([]string{"platform/build"}, recordNames(records)); diff != "" {
		t.Fatalf("unexpected repositories (-want +got):\n%s", diff)
	}
	wantSource, _ := filepath.EvalSymlinks(projectPath)
	if records[0].Source != wantSource {
		t.Fatalf("got source %q, want %q", records[0].Source, wantSource)
	}
	searchIndex(t, indexDir, "local repository", "platform/build")
}

func TestSyncManifestMovesProjectAcrossBranches(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "old", "location")
	createGitRepository(t, oldPath)
	indexDir := t.TempDir()

	manifestV1 := filepath.Join(root, "manifest-v1.xml")
	writeManifest(t, manifestV1, manifestXML(
		`<project name="platform/moving" path="old/location"/>`,
	))
	if _, err := runManifestSync(t, indexDir, manifestV1, root, "-f"); err != nil {
		t.Fatal(err)
	}

	// A new release branch places the project elsewhere. Repo leaves the old
	// working directory behind, and an unrelated stale checkout exists too.
	newPath := filepath.Join(root, "new", "location")
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	createGitRepository(t, filepath.Join(root, "stale", "leftover"))

	manifestV2 := filepath.Join(root, "manifest-v2.xml")
	writeManifest(t, manifestV2, manifestXML(
		`<project name="platform/moving" path="new/location"/>`,
	))

	out, err := runManifestSync(t, indexDir, manifestV2, root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `Would update source for "platform/moving"`) {
		t.Fatalf("preview did not report the path move:\n%s", out)
	}
	if !strings.Contains(out, "Up to date \"platform/moving\"") {
		t.Fatalf("preview should not reindex unchanged content:\n%s", out)
	}

	if _, err := runManifestSync(t, indexDir, manifestV2, root, "-f"); err != nil {
		t.Fatal(err)
	}

	records := listRecords(t, indexDir)
	if diff := cmp.Diff([]string{"platform/moving"}, recordNames(records)); diff != "" {
		t.Fatalf("old checkout path remained indexed (-want +got):\n%s", diff)
	}
	wantSource, _ := filepath.EvalSymlinks(newPath)
	if records[0].Source != wantSource {
		t.Fatalf("got source %q, want %q", records[0].Source, wantSource)
	}
	metaFiles, _ := filepath.Glob(filepath.Join(indexDir, "*.meta"))
	if len(metaFiles) != 1 {
		t.Fatalf("got %d metadata sidecars, want 1", len(metaFiles))
	}
	searchIndex(t, indexDir, "branch:HEAD local repository", "platform/moving")

	// The move survives a repeat run without rebuilding or duplicating the
	// repository.
	out, err = runManifestSync(t, indexDir, manifestV2, root, "-f")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `Up to date "platform/moving"`) {
		t.Fatalf("repeat sync did not report the project up to date:\n%s", out)
	}
	if strings.Contains(out, "Updated source") {
		t.Fatalf("repeat sync rewrote the source again:\n%s", out)
	}
	if diff := cmp.Diff([]string{"platform/moving"}, recordNames(listRecords(t, indexDir))); diff != "" {
		t.Fatalf("repository list changed on repeat sync (-want +got):\n%s", diff)
	}
}

func TestSyncManifestMovesProjectWithChangedContent(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "old", "location")
	createGitRepository(t, oldPath)
	indexDir := t.TempDir()

	manifestV1 := filepath.Join(root, "manifest-v1.xml")
	writeManifest(t, manifestV1, manifestXML(
		`<project name="platform/moving" path="old/location"/>`,
	))
	if _, err := runManifestSync(t, indexDir, manifestV1, root, "-f"); err != nil {
		t.Fatal(err)
	}

	newPath := filepath.Join(root, "new", "location")
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newPath, "FEATURE.md"), []byte("shiny new marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, newPath, "add", ".")
	testutil.RunGit(t, newPath, "commit", "-m", "new release content")

	manifestV2 := filepath.Join(root, "manifest-v2.xml")
	writeManifest(t, manifestV2, manifestXML(
		`<project name="platform/moving" path="new/location"/>`,
	))

	if _, err := runManifestSync(t, indexDir, manifestV2, root, "-f"); err != nil {
		t.Fatal(err)
	}
	records := listRecords(t, indexDir)
	if diff := cmp.Diff([]string{"platform/moving"}, recordNames(records)); diff != "" {
		t.Fatalf("unexpected repositories after moved rebuild (-want +got):\n%s", diff)
	}
	wantSource, _ := filepath.EvalSymlinks(newPath)
	if records[0].Source != wantSource {
		t.Fatalf("got source %q, want %q", records[0].Source, wantSource)
	}
	searchIndex(t, indexDir, "shiny new marker", "platform/moving")
	searchIndex(t, indexDir, "branch:HEAD local repository", "platform/moving")
}

func TestSyncManifestPrunesRemovedProjects(t *testing.T) {
	root := t.TempDir()
	createGitRepository(t, filepath.Join(root, "kept"))
	indexDir := t.TempDir()
	removedShard := createTestShard(t, indexDir, "platform/gone", filepath.Join(root, "gone"))

	manifestPath := filepath.Join(root, "manifest.xml")
	writeManifest(t, manifestPath, manifestXML(
		`<project name="platform/kept" path="kept"/>`,
	))
	out, err := runManifestSync(t, indexDir, manifestPath, root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Would remove "+removedShard) {
		t.Fatalf("preview did not report removal of dropped project:\n%s", out)
	}

	if _, err := runManifestSync(t, indexDir, manifestPath, root, "-f"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(removedShard); !os.IsNotExist(err) {
		t.Fatalf("dropped project shard survived the sync: %v", err)
	}
	assertIndexAbsent(t, indexDir, "test repository")
	searchIndex(t, indexDir, "local repository", "platform/kept")
}

func TestSyncManifestParseFailurePreservesIndex(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "platform", "build")
	createGitRepository(t, projectPath)
	indexDir := t.TempDir()
	manifestPath := filepath.Join(root, "manifest.xml")
	writeManifest(t, manifestPath, manifestXML(
		`<project name="platform/build" path="platform/build"/>`,
	))
	if _, err := runManifestSync(t, indexDir, manifestPath, root, "-f"); err != nil {
		t.Fatal(err)
	}
	before := listRecords(t, indexDir)
	beforeShards, err := readInventory(indexDir)
	if err != nil {
		t.Fatal(err)
	}

	// A shard for a project the broken manifest would have dropped must
	// survive the failed sync.
	writeManifest(t, manifestPath, "<manifest><project name=")
	if _, err := runManifestSync(t, indexDir, manifestPath, root, "-f"); err == nil {
		t.Fatal("expected malformed manifest to fail the sync")
	}
	if diff := cmp.Diff(recordNames(before), recordNames(listRecords(t, indexDir))); diff != "" {
		t.Fatalf("index changed after parse failure (-want +got):\n%s", diff)
	}
	afterShards, err := readInventory(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterShards) != len(beforeShards) {
		t.Fatalf("got %d shards after parse failure, want %d", len(afterShards), len(beforeShards))
	}

	// A manifest that references a checkout that does not exist likewise
	// aborts before pruning.
	writeManifest(t, filepath.Join(root, "missing.xml"), manifestXML(
		`<project name="platform/absent"/>`,
	))
	if _, err := runManifestSync(t, indexDir, filepath.Join(root, "missing.xml"), root, "-f"); err == nil {
		t.Fatal("expected missing project checkout to fail the sync")
	}
	if diff := cmp.Diff(recordNames(before), recordNames(listRecords(t, indexDir))); diff != "" {
		t.Fatalf("index changed after discovery failure (-want +got):\n%s", diff)
	}
}

func TestSyncManifestIndexFailurePreservesPreviousIndex(t *testing.T) {
	root := t.TempDir()
	indexDir := t.TempDir()

	// Existing index for a project that the new manifest drops. Its shard
	// must survive a failed sync.
	removedShard := createTestShard(t, indexDir, "platform/gone", filepath.Join(root, "gone"))

	// The manifest also names a project whose checkout cannot be opened.
	broken := filepath.Join(root, "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, ".git"), []byte("gitdir: does-not-exist\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, "manifest.xml")
	writeManifest(t, manifestPath, manifestXML(
		`<project name="platform/broken" path="broken"/>`,
	))

	if _, err := runManifestSync(t, indexDir, manifestPath, root, "-f"); err == nil {
		t.Fatal("expected unreadable project to fail the sync")
	}
	if _, err := os.Stat(removedShard); err != nil {
		t.Fatalf("previous index was pruned despite sync failure: %v", err)
	}
	if records := listRecords(t, indexDir); len(records) != 1 || records[0].Name != "platform/gone" {
		t.Fatalf("unexpected repositories after failed sync: %+v", records)
	}

	// Repairing the checkout lets the next sync converge.
	if err := os.RemoveAll(broken); err != nil {
		t.Fatal(err)
	}
	createGitRepository(t, broken)
	if _, err := runManifestSync(t, indexDir, manifestPath, root, "-f"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(removedShard); !os.IsNotExist(err) {
		t.Fatalf("dropped project survived the repaired sync: %v", err)
	}
	if diff := cmp.Diff([]string{"platform/broken"}, recordNames(listRecords(t, indexDir))); diff != "" {
		t.Fatalf("unexpected repositories after repaired sync (-want +got):\n%s", diff)
	}
}

func TestSyncManifestRecoversAfterInterruptedRemoval(t *testing.T) {
	root := t.TempDir()
	createGitRepository(t, filepath.Join(root, "kept"))
	indexDir := t.TempDir()

	manifestPath := filepath.Join(root, "manifest.xml")
	writeManifest(t, manifestPath, manifestXML(
		`<project name="platform/kept" path="kept"/>`,
	))
	if _, err := runManifestSync(t, indexDir, manifestPath, root, "-f"); err != nil {
		t.Fatal(err)
	}

	// State on disk after a sync interrupted between successful indexing and
	// the prune: the kept project is current, the dropped project lingers.
	_ = createTestShard(t, indexDir, "platform/gone", filepath.Join(root, "gone"))

	if _, err := runManifestSync(t, indexDir, manifestPath, root, "-f"); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"platform/kept"}, recordNames(listRecords(t, indexDir))); diff != "" {
		t.Fatalf("index did not converge after interrupted removal (-want +got):\n%s", diff)
	}

	// One more run stays stable.
	if _, err := runManifestSync(t, indexDir, manifestPath, root, "-f"); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"platform/kept"}, recordNames(listRecords(t, indexDir))); diff != "" {
		t.Fatalf("index changed after converging (-want +got):\n%s", diff)
	}
}
