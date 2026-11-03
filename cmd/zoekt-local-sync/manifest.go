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

// This file implements discovery driven by an Android 'repo' manifest
// (https://gerrit.googlesource.com/git-repo/+/master/docs/manifest-format.txt).
//
// Release branches of such a manifest frequently place the same project at a
// different checkout path, or drop a project entirely. Discovering
// repositories by walking the filesystem then leaves stale directories from
// older branches indexed under their old path names, or indexes a leftover
// checkout that the current manifest does not describe.
//
// When a manifest is supplied, projects are instead identified by the stable
// manifest "name" attribute: the same project moving to another path keeps the
// same Zoekt repository (and shard set) and merely points at the new source,
// and only projects that disappear from the manifest are pruned.

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// manifestProject is one resolved <project> entry: its stable Name (the
// manifest "name" attribute, identifying the upstream Git repository) and the
// checkout Path relative to the client root.
type manifestProject struct {
	Name string
	Path string
}

// manifestItemKind discriminates the ordered child elements of a manifest.
type manifestItemKind int

const (
	manifestProjectItem manifestItemKind = iota
	manifestIncludeItem
	manifestRemoveItem
	manifestExtendItem
)

type rawManifestItem struct {
	kind   manifestItemKind
	name   string
	path   string
	groups string
}

// rawManifest is the ordered contents of a single manifest XML file. Element
// order matters because <include>, <project>, <remove-project> and
// <extend-project> are processed in document order.
type rawManifest struct {
	items []rawManifestItem
}

// UnmarshalXML decodes only the elements that affect the project layout.
func (m *rawManifest) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	type attrProject struct {
		Name   string `xml:"name,attr"`
		Path   string `xml:"path,attr"`
		Groups string `xml:"groups,attr"`
	}
	type attrName struct {
		Name string `xml:"name,attr"`
	}

	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}

		switch se.Name.Local {
		case "project":
			var p attrProject
			if err := d.DecodeElement(&p, &se); err != nil {
				return err
			}
			m.items = append(m.items, rawManifestItem{
				kind:   manifestProjectItem,
				name:   strings.TrimSpace(p.Name),
				path:   strings.TrimSpace(p.Path),
				groups: p.Groups,
			})
		case "include":
			var inc attrName
			if err := d.DecodeElement(&inc, &se); err != nil {
				return err
			}
			m.items = append(m.items, rawManifestItem{
				kind: manifestIncludeItem,
				name: strings.TrimSpace(inc.Name),
			})
		case "remove-project":
			var rm attrName
			if err := d.DecodeElement(&rm, &se); err != nil {
				return err
			}
			m.items = append(m.items, rawManifestItem{
				kind: manifestRemoveItem,
				name: strings.TrimSpace(rm.Name),
			})
		case "extend-project":
			var ep attrProject
			if err := d.DecodeElement(&ep, &se); err != nil {
				return err
			}
			m.items = append(m.items, rawManifestItem{
				kind:   manifestExtendItem,
				name:   strings.TrimSpace(ep.Name),
				path:   strings.TrimSpace(ep.Path),
				groups: ep.Groups,
			})
		default:
			// Consume elements we do not model.
			if err := d.Skip(); err != nil {
				return err
			}
		}
	}
}

// parseManifestFile reads and decodes one manifest XML file, preserving the
// order of the layout-affecting elements.
func parseManifestFile(file string) (*rawManifest, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("open manifest %s: %w", file, err)
	}
	defer f.Close()

	var raw rawManifest
	dec := xml.NewDecoder(f)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", file, err)
	}
	return &raw, nil
}

// validateProjectPath ensures a project path stays inside the client root.
func validateProjectPath(name, projectPath string) (string, error) {
	cleaned := path.Clean(projectPath)
	if cleaned == "" || cleaned == "." {
		return "", fmt.Errorf("project %q has an empty checkout path", name)
	}
	if path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("project %q has a checkout path %q outside the client root", name, projectPath)
	}
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(cleaned))), nil
}

func parseManifestGroups(groups string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, group := range strings.Split(groups, ",") {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		set[group] = struct{}{}
	}
	if len(set) == 0 {
		set["default"] = struct{}{}
	}
	return set
}

// manifestResolver accumulates the effective project set across a root
// manifest and its transitive includes.
type manifestResolver struct {
	order  []string
	byName map[string]*manifestProject
	groups map[string]map[string]struct{}
}

func newManifestResolver() *manifestResolver {
	return &manifestResolver{
		byName: map[string]*manifestProject{},
		groups: map[string]map[string]struct{}{},
	}
}

// loadFile processes one manifest file. Included files are resolved relative
// to the directory of the file that names them (following symlinks, as repo's
// manifest.xml is one).
func (r *manifestResolver) loadFile(file string) error {
	raw, err := parseManifestFile(file)
	if err != nil {
		return err
	}

	dir := filepath.Dir(file)
	if resolved, err := filepath.EvalSymlinks(file); err == nil {
		dir = filepath.Dir(resolved)
	}

	for _, item := range raw.items {
		switch item.kind {
		case manifestProjectItem:
			if item.name == "" {
				return fmt.Errorf("manifest %s contains a project without a name", file)
			}
			if _, exists := r.byName[item.name]; exists {
				return fmt.Errorf("manifest %s lists duplicate project %q", file, item.name)
			}
			projectPath := item.path
			if projectPath == "" {
				projectPath = item.name
			}
			projectPath, err := validateProjectPath(item.name, projectPath)
			if err != nil {
				return err
			}
			r.byName[item.name] = &manifestProject{Name: item.name, Path: projectPath}
			r.order = append(r.order, item.name)
			r.groups[item.name] = parseManifestGroups(item.groups)

		case manifestIncludeItem:
			if item.name == "" {
				return fmt.Errorf("manifest %s contains an include without a name", file)
			}
			includePath := item.name
			if !filepath.IsAbs(includePath) {
				includePath = filepath.Join(dir, includePath)
			}
			if err := r.loadFile(includePath); err != nil {
				return err
			}

		case manifestRemoveItem:
			if _, exists := r.byName[item.name]; !exists {
				return fmt.Errorf("manifest %s removes project %q that was never declared", file, item.name)
			}
			delete(r.byName, item.name)
			delete(r.groups, item.name)

		case manifestExtendItem:
			project, exists := r.byName[item.name]
			if !exists {
				return fmt.Errorf("manifest %s extends project %q that was never declared", file, item.name)
			}
			if item.path != "" {
				projectPath, err := validateProjectPath(item.name, item.path)
				if err != nil {
					return err
				}
				project.Path = projectPath
			}
			if item.groups != "" {
				if r.groups[item.name] == nil {
					r.groups[item.name] = map[string]struct{}{}
				}
				for group := range parseManifestGroups(item.groups) {
					r.groups[item.name][group] = struct{}{}
				}
			}
		}
	}

	// order may contain names that were later removed; prune on output.
	return nil
}

func (r *manifestResolver) projects() []manifestProject {
	seen := map[string]struct{}{}
	projects := make([]manifestProject, 0, len(r.byName))
	for _, name := range r.order {
		project, ok := r.byName[name]
		if !ok {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		if _, excluded := r.groups[name]["notdefault"]; excluded {
			continue
		}
		projects = append(projects, *project)
	}
	return projects
}

// resolveManifestPath turns the -manifest argument into a manifest XML file
// path. A plain XML file is used directly; a directory containing
// manifest.xml, or a repo client root containing .repo/manifest.xml, is
// accepted as well.
func resolveManifestPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve manifest path %q: %w", p, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("stat manifest %s: %w", abs, err)
	}
	if !info.IsDir() {
		return abs, nil
	}

	candidates := []string{
		filepath.Join(abs, "manifest.xml"),
		filepath.Join(abs, ".repo", "manifest.xml"),
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate, nil
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("stat manifest %s: %w", candidate, err)
		}
	}
	return "", fmt.Errorf("no manifest.xml found under %s", abs)
}

// loadManifestProjects resolves the effective project set of the manifest at
// the given path, following includes and applying remove/extend directives.
func loadManifestProjects(p string) ([]manifestProject, error) {
	manifestFile, err := resolveManifestPath(p)
	if err != nil {
		return nil, err
	}
	resolver := newManifestResolver()
	if err := resolver.loadFile(manifestFile); err != nil {
		return nil, err
	}
	return resolver.projects(), nil
}

// locateProjectGitDir finds a manifest project's working directory under the
// resolved roots. It accepts both regular checkouts (.git directory) and git
// worktrees (.git file), mirroring filesystem discovery.
func locateProjectGitDir(roots []string, projectPath string) (string, error) {
	rel := filepath.FromSlash(projectPath)
	var failures []string
	for _, root := range roots {
		candidate := filepath.Join(root, rel)
		info, err := os.Stat(candidate)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				failures = append(failures, candidate)
				continue
			}
			return "", fmt.Errorf("stat project directory %s: %w", candidate, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("project path %s is not a directory", candidate)
		}

		dotGit := filepath.Join(candidate, ".git")
		dotGitInfo, err := os.Stat(dotGit)
		switch {
		case err == nil && (dotGitInfo.IsDir() || dotGitInfo.Mode().IsRegular()):
			return evalPath(candidate)
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return "", fmt.Errorf("stat %s: %w", dotGit, err)
		}

		if strings.HasSuffix(candidate, ".git") {
			objectsInfo, err := os.Stat(filepath.Join(candidate, "objects"))
			if err == nil && objectsInfo.IsDir() {
				return evalPath(candidate)
			}
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", fmt.Errorf("stat bare project %s: %w", candidate, err)
			}
		}
		failures = append(failures, candidate)
	}
	return "", fmt.Errorf("manifest project checkout %q not found; checked %s", projectPath, strings.Join(failures, ", "))
}

func evalPath(p string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved, nil
	}
	return p, nil
}

// discoverManifestRepositories returns the repositories described by an
// Android repo manifest, located beneath the given roots. Repository names are
// the stable manifest project names rather than the current checkout paths.
// All parsing and resolution happens before any index mutation, so a failure
// leaves the existing index untouched.
func discoverManifestRepositories(manifestPath string, rootPaths []string) ([]repositorySpec, error) {
	roots, err := resolveRoots(rootPaths)
	if err != nil {
		return nil, err
	}
	projects, err := loadManifestProjects(manifestPath)
	if err != nil {
		return nil, err
	}
	if len(projects) == 0 {
		return nil, fmt.Errorf("manifest %s does not list any projects", manifestPath)
	}

	byName := map[string]struct{}{}
	bySource := map[string]struct{}{}
	repositories := make([]repositorySpec, 0, len(projects))
	for _, project := range projects {
		if _, dup := byName[project.Name]; dup {
			return nil, fmt.Errorf("duplicate manifest project name %q", project.Name)
		}
		source, err := locateProjectGitDir(roots, project.Path)
		if err != nil {
			return nil, err
		}
		if _, dup := bySource[source]; dup {
			return nil, fmt.Errorf("manifest project %q shares checkout %s with another project", project.Name, source)
		}
		byName[project.Name] = struct{}{}
		bySource[source] = struct{}{}
		repositories = append(repositories, repositorySpec{
			Name:   filepath.ToSlash(project.Name),
			Source: source,
		})
	}

	sort.Slice(repositories, func(i, j int) bool {
		return repositories[i].Name < repositories[j].Name
	})
	return repositories, nil
}
