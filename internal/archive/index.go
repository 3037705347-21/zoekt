// package archive provides indexing of archives from remote URLs.
package archive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/index"
)

// Options specify the archive specific indexing options.
type Options struct {
	Incremental bool

	Archive string
	Name    string
	RepoURL string
	Branch  string
	Commit  string
	Strip   int

	// DownloadDir caches partially downloaded remote archives so an
	// interrupted run resumes from the last verified byte instead of
	// restarting. It defaults to a ".archive" subdirectory of the index
	// directory. Files in it never have a ".zoekt" name and are never
	// loaded as search indices.
	DownloadDir string
}

func (o *Options) SetDefaults() {
	// We guess based on the archive URL.
	u, _ := url.Parse(o.Archive)
	if u == nil {
		return
	}

	setRef := func(ref string) {
		if isGitOID(ref) && o.Commit == "" {
			o.Commit = ref
		}
		if !isGitOID(ref) && o.Branch == "" {
			o.Branch = ref
		}
	}

	switch u.Host {
	case "github.com", "codeload.github.com":
		// https://github.com/octokit/octokit.rb/commit/3d21ec53a331a6f037a91c368710b99387d012c1
		// https://github.com/octokit/octokit.rb/blob/master/README.md
		// https://github.com/octokit/octokit.rb/tree/master/lib
		// https://codeload.github.com/octokit/octokit.rb/legacy.tar.gz/master
		parts := strings.Split(u.Path, "/")
		if len(parts) > 2 && o.Name == "" {
			o.Name = fmt.Sprintf("github.com/%s/%s", parts[1], parts[2])
			o.RepoURL = fmt.Sprintf("https://github.com/%s/%s", parts[1], parts[2])
		}
		if len(parts) > 4 {
			setRef(parts[4])
			if u.Host == "github.com" {
				o.Archive = fmt.Sprintf("https://codeload.github.com/%s/%s/legacy.tar.gz/%s", parts[1], parts[2], parts[4])
			}
		}
		o.Strip = 1
	case "api.github.com":
		// https://api.github.com/repos/octokit/octokit.rb/tarball/master
		parts := strings.Split(u.Path, "/")
		if len(parts) > 2 && o.Name == "" {
			o.Name = fmt.Sprintf("github.com/%s/%s", parts[1], parts[2])
			o.RepoURL = fmt.Sprintf("https://github.com/%s/%s", parts[1], parts[2])
		}
		if len(parts) > 5 {
			setRef(parts[5])
		}
		o.Strip = 1
	}
}

// Index archive specified in opts using bopts.
func Index(opts Options, bopts index.Options) error {
	opts.SetDefaults()

	if opts.Name == "" && opts.RepoURL == "" {
		return errors.New("-name or -url required")
	}
	if opts.Branch == "" {
		return errors.New("-branch required")
	}

	if opts.Name != "" {
		bopts.RepositoryDescription.Name = opts.Name
	}
	// We do not use this functionality to avoid pulling in the transitive deps of gitindex
	/*
		if opts.RepoURL != "" {
			u, err := url.Parse(opts.RepoURL)
			if err != nil {
				return err
			}
			if err := gitindex.SetTemplatesFromOrigin(&bopts.RepositoryDescription, u); err != nil {
				return err
			}
		}
	*/
	bopts.SetDefaults()
	bopts.RepositoryDescription.Branches = []zoekt.RepositoryBranch{{Name: opts.Branch, Version: opts.Commit}}
	brs := []string{opts.Branch}

	if opts.Incremental && bopts.IncrementalSkipIndexing() {
		return nil
	}

	// Remote archives are fully downloaded into a private, identity-checked
	// cache before anything is indexed: interrupted runs resume from the
	// last verified offset, and replaced or inconsistent source content
	// forces a complete re-download. Local archives are opened directly.
	var download *archiveDownload
	archivePath := opts.Archive
	if isHTTPURL(opts.Archive) {
		d, err := downloadArchive(context.Background(), opts.Archive, opts.Commit, opts.downloadDir(bopts))
		if err != nil {
			return err
		}
		download = d
		archivePath = d.partPath
	}

	a, err := openArchiveSource(archivePath, download != nil)
	if err != nil {
		if download != nil {
			download.release(false)
		}
		return err
	}

	bopts.RepositoryDescription.Source = opts.Archive
	var builder *index.Builder

	once := sync.Once{}
	var onceErr error
	add := func(f *File) error {
		defer f.Close()

		once.Do(func() {
			// We use the ModTime of the first file as a proxy for the latest commit date.
			bopts.RepositoryDescription.LatestCommitDate = f.ModTime
			builder, onceErr = index.NewBuilder(bopts)
		})
		if onceErr != nil {
			return onceErr
		}

		contents, err := io.ReadAll(f)
		if err != nil {
			return err
		}

		name := stripComponents(f.Name, opts.Strip)
		if name == "" {
			return nil
		}

		return builder.Add(index.Document{
			Name:     name,
			Content:  contents,
			Branches: brs,
		})
	}

	var loopErr error
loop:
	for {
		f, err := a.Next()
		if err == io.EOF {
			break loop
		}
		if err != nil {
			loopErr = err
			break loop
		}

		if err := add(f); err != nil {
			loopErr = err
			break loop
		}
	}

	if builder != nil {
		if loopErr != nil {
			// Source read failed (network residue, checksum mismatch,
			// truncated archive): drop every temporary shard so an
			// interrupted build cannot replace the existing index.
			_ = builder.Abort()
		} else if err := builder.Finish(); err != nil {
			loopErr = err
		}
	}
	if err := a.Close(); loopErr == nil {
		loopErr = err
	}

	// The download cache is only removed after the complete archive has
	// been read, verified and produced the replacement index. Keep it on
	// failure so the next run can resume.
	if download != nil {
		download.release(loopErr == nil)
	}
	return loopErr
}

// openArchiveSource opens either a fully downloaded local file or the
// original local/stdin source.
func openArchiveSource(path string, downloaded bool) (Archive, error) {
	if downloaded {
		return openArchiveFile(path)
	}
	return openArchive(path)
}

func isHTTPURL(u string) bool {
	return strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")
}

// downloadDir resolves where partial downloads are stored.
func (o Options) downloadDir(bopts index.Options) string {
	if o.DownloadDir != "" {
		return o.DownloadDir
	}
	if bopts.IndexDir != "" {
		return filepath.Join(bopts.IndexDir, ".archive")
	}
	return filepath.Join(os.TempDir(), "zoekt-archive")
}

// stripComponents removes the specified number of leading path
// elements. Pathnames with fewer elements will return the empty string.
func stripComponents(path string, count int) string {
	for i := 0; path != "" && i < count; i++ {
		i := strings.Index(path, "/")
		if i < 0 {
			return ""
		}
		path = path[i+1:]
	}
	return path
}

// isGitOID checks if the revision is a git OID SHA string.
//
// Note: This doesn't mean the SHA exists in a repository, nor does it mean it
// isn't a ref. Git allows 40-char hexadecimal strings to be references.
func isGitOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') &&
			(r < 'a' || r > 'f') &&
			(r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}
