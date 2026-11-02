package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Archive interface {
	Next() (*File, error)
	Close() error
}

type File struct {
	io.ReadCloser
	Name    string
	Size    int64
	ModTime time.Time
}

type tarArchive struct {
	io.Closer
	tr *tar.Reader
	// gz is set for gzip-compressed tar archives so its checksum trailer
	// can be verified once the whole archive has been read.
	gz       *gzip.Reader
	verified bool
}

func (a *tarArchive) Next() (*File, error) {
	for {
		hdr, err := a.tr.Next()
		if err == io.EOF && a.gz != nil && !a.verified {
			// Read the gzip stream to its end so its CRC32 and size trailer
			// are validated. A truncated download fails here instead of
			// silently producing a partial index.
			if _, verr := io.Copy(io.Discard, a.gz); verr != nil {
				return nil, fmt.Errorf("verifying compressed archive: %w", verr)
			}
			a.verified = true
		}
		if err != nil {
			return nil, err
		}

		// We only care about files. tar.Reader reports legacy TypeRegA
		// entries as TypeReg.
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		return &File{
			ReadCloser: io.NopCloser(a.tr),
			Name:       hdr.Name,
			Size:       hdr.Size,
			ModTime:    hdr.ModTime,
		}, nil
	}
}

type zipArchive struct {
	io.Closer
	files []*zip.File
}

func (a *zipArchive) Next() (*File, error) {
	if len(a.files) == 0 {
		return nil, io.EOF
	}

	f := a.files[0]
	a.files = a.files[1:]

	r, err := f.Open()
	if err != nil {
		return nil, err
	}

	return &File{
		ReadCloser: r,
		Name:       f.Name,
		Size:       int64(f.UncompressedSize64),
		ModTime:    f.Modified,
	}, nil
}

func newZipArchive(r io.Reader, closer io.Closer) (*zipArchive, error) {
	f, ok := r.(interface {
		io.ReaderAt
		Stat() (os.FileInfo, error)
	})
	if !ok {
		return nil, fmt.Errorf("streaming zip files not supported")
	}

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}

	zr, err := zip.NewReader(f, fi.Size())
	if err != nil {
		return nil, err
	}

	// Filter out non files
	files := zr.File[:0]
	for _, f := range zr.File {
		if f.Mode().IsRegular() {
			files = append(files, f)
		}
	}

	return &zipArchive{
		Closer: closer,
		files:  files,
	}, nil
}

func detectContentType(r io.Reader) (string, io.Reader, error) {
	var buf [512]byte
	n, err := io.ReadFull(r, buf[:])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", nil, err
	}

	ct := http.DetectContentType(buf[:n])

	// If we are a seeker, we can just undo our read
	if s, ok := r.(io.Seeker); ok {
		_, err := s.Seek(int64(-n), io.SeekCurrent)
		return ct, r, err
	}

	// Otherwise return a new reader which merges in the read bytes
	return ct, io.MultiReader(bytes.NewReader(buf[:n]), r), nil
}

// OpenReader returns a reader for the archive at the URL u.
func OpenReader(u string) (io.ReadCloser, error) {
	if strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
		resp, err := http.Get(u)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
			_ = resp.Body.Close()
			if err != nil {
				return nil, err
			}
			return nil, &url.Error{
				Op:  "Get",
				URL: u,
				Err: fmt.Errorf("%s: %s", resp.Status, string(b)),
			}
		}
		return resp.Body, nil
	} else if u == "-" {
		return io.NopCloser(os.Stdin), nil
	}

	return os.Open(u)
}

// openArchive opens the tar at the URL or filepath u. Also supported is tgz
// files over http.
func openArchive(u string) (ar Archive, err error) {
	readCloser, err := OpenReader(u)
	if err != nil {
		return nil, err
	}
	return archiveFromReader(readCloser, readCloser)
}

// openArchiveFile opens the tar, tgz or zip archive stored at the local path.
func openArchiveFile(path string) (Archive, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return archiveFromReader(f, f)
}

// archiveFromReader detects the archive format on r and returns an Archive.
// closer is closed when the returned Archive is closed.
func archiveFromReader(r io.Reader, closer io.Closer) (ar Archive, err error) {
	defer func() {
		if err != nil {
			_ = closer.Close()
		}
	}()

	ct, r, err := detectContentType(r)
	if err != nil {
		return nil, err
	}
	switch ct {
	case "application/x-gzip":
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, err
		}

		return &tarArchive{
			Closer: closer,
			tr:     tar.NewReader(gz),
			gz:     gz,
		}, nil

	case "application/zip":
		return newZipArchive(r, closer)
	}

	return &tarArchive{
		Closer: closer,
		tr:     tar.NewReader(r),
	}, nil
}
