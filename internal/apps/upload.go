package apps

// Uploading code: an app whose files come from a .zip or .tar.gz the user
// picked in the browser, instead of a scaffold, a folder already on the host,
// or a repository xdev clones.
//
// The two formats are handled together because the user does not think of them
// as different things — they think "here is my code" — and because everything
// interesting (which wrapper directory to drop, which entries to refuse) is the
// same either way.

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ErrNotAnArchive is returned when an upload is neither gzip nor zip. The
// format is decided by the file's first bytes rather than its name: a browser
// will happily send anything with any extension, and "site.zip" that is
// actually a RAR should fail with a sentence the user can act on.
var ErrNotAnArchive = errors.New("that file is not a .zip or .tar.gz archive")

// ErrEmptyArchive is returned when an upload unpacked to nothing. Almost always
// an archive of an empty folder, and worth saying so — the app would otherwise
// be created, start, and serve a blank page with no explanation.
var ErrEmptyArchive = errors.New("the uploaded archive has no files in it")

// extractUpload unpacks an uploaded .zip or .tar.gz into dest.
func extractUpload(r io.Reader, dest string) error {
	src, size, cleanup, err := positioned(r)
	if err != nil {
		return err
	}
	defer cleanup()

	// Sniff rather than trust the extension. gzip is 1f 8b; every zip member
	// header starts "PK", including the end-of-archive record an empty zip is
	// made entirely of.
	var magic [2]byte
	if n, _ := src.ReadAt(magic[:], 0); n < 2 {
		return ErrNotAnArchive
	}
	switch {
	case magic[0] == 0x1f && magic[1] == 0x8b:
		return untarGzUpload(src, size, dest)
	case magic[0] == 'P' && magic[1] == 'K':
		return unzipUpload(src, size, dest)
	}
	return ErrNotAnArchive
}

// positioned turns an upload into something that can be read at an offset and
// whose length is known — what archive/zip requires, and what lets the tar path
// walk the archive twice (once to find the wrapper directory, once to extract).
//
// Both real callers already hand over something seekable: a multipart.File for
// a native submit, and the spooled temp file for a background create. The copy
// is the fallback for anything else, so this function is total rather than
// leaving a trap for the next caller.
func positioned(r io.Reader) (io.ReaderAt, int64, func(), error) {
	noop := func() {}
	if ra, ok := r.(io.ReaderAt); ok {
		if s, ok := r.(io.Seeker); ok {
			if size, err := s.Seek(0, io.SeekEnd); err == nil {
				if _, err := s.Seek(0, io.SeekStart); err == nil {
					return ra, size, noop, nil
				}
			}
		}
	}
	f, err := os.CreateTemp("", "xdev-code-*")
	if err != nil {
		return nil, 0, noop, err
	}
	cleanup := func() { f.Close(); os.Remove(f.Name()) }
	size, err := io.Copy(f, r)
	if err != nil {
		cleanup()
		return nil, 0, noop, err
	}
	return f, size, cleanup, nil
}

// unzipUpload extracts a zip into dest.
func unzipUpload(src io.ReaderAt, size int64, dest string) error {
	zr, err := zip.NewReader(src, size)
	if err != nil {
		return fmt.Errorf("read the zip: %w", err)
	}
	var files []string
	for _, f := range zr.File {
		if junk(f.Name) || f.FileInfo().IsDir() {
			continue
		}
		files = append(files, path.Clean(f.Name))
	}
	root := wrapperDir(files)

	written := 0
	for _, f := range zr.File {
		if junk(f.Name) {
			continue
		}
		rel, err := entryPath(f.Name, root)
		if err != nil {
			return err
		}
		if rel == "" {
			continue
		}
		dst := filepath.Join(dest, rel)
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
			continue
		}
		// Only regular files. A zip can also carry symlinks and devices, and an
		// archive from a stranger's laptop is not a thing to recreate those from.
		if !f.Mode().IsRegular() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeEntry(dst, rc, f.Mode())
		rc.Close()
		if err != nil {
			return err
		}
		written++
	}
	if written == 0 {
		return ErrEmptyArchive
	}
	return nil
}

// untarGzUpload extracts a .tar.gz into dest. It walks the archive twice
// because the wrapper directory can only be recognised once every entry has
// been seen, and a tar has no index to consult.
func untarGzUpload(src io.ReaderAt, size int64, dest string) error {
	var files []string
	err := walkTarGz(src, size, func(hdr *tar.Header, _ io.Reader) error {
		if hdr.Typeflag == tar.TypeReg && !junk(hdr.Name) {
			files = append(files, path.Clean(hdr.Name))
		}
		return nil
	})
	if err != nil {
		return err
	}
	root := wrapperDir(files)

	written := 0
	err = walkTarGz(src, size, func(hdr *tar.Header, body io.Reader) error {
		if junk(hdr.Name) {
			return nil
		}
		rel, err := entryPath(hdr.Name, root)
		if err != nil || rel == "" {
			return err
		}
		dst := filepath.Join(dest, rel)
		switch hdr.Typeflag {
		case tar.TypeDir:
			return os.MkdirAll(dst, 0o755)
		case tar.TypeReg:
			if err := writeEntry(dst, body, fs.FileMode(hdr.Mode)); err != nil {
				return err
			}
			written++
		}
		return nil
	})
	if err != nil {
		return err
	}
	if written == 0 {
		return ErrEmptyArchive
	}
	return nil
}

// walkTarGz calls fn for every entry of a gzipped tar. body is only valid for
// the duration of the call — it reads from the shared tar stream.
func walkTarGz(src io.ReaderAt, size int64, fn func(*tar.Header, io.Reader) error) error {
	gz, err := gzip.NewReader(io.NewSectionReader(src, 0, size))
	if err != nil {
		return fmt.Errorf("read the archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read the archive: %w", err)
		}
		if err := fn(hdr, tr); err != nil {
			return err
		}
	}
}

// writeEntry creates one file from an archive, making its parents as needed.
// The mode is masked to the permission bits: setuid and sticky have no business
// arriving from an upload, and a zip written on Windows carries no mode at all.
func writeEntry(dst string, body io.Reader, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	perm := mode.Perm()
	if perm == 0 {
		perm = 0o644
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// wrapperDir returns the single directory every file in the archive sits
// inside, or "" when they do not share one.
//
// This is the difference between an app that works and one that serves a
// directory listing. Zipping a folder — in Finder, in Explorer, or with `zip
// -r` — puts everything under a directory named after it, and GitHub's "Download
// ZIP" does the same with a name nobody chose. Unpacked verbatim, the app's
// index.html ends up one level below where the app looks for it.
//
// Only regular files are considered. A directory entry cannot tell us anything
// here: "myapp/" and a top-level file called "myapp" are the same string once
// the trailing slash is gone.
func wrapperDir(files []string) string {
	root := ""
	for _, name := range files {
		first, _, nested := strings.Cut(name, "/")
		if !nested {
			return "" // a file at the top level: this archive is already the code
		}
		if root == "" {
			root = first
		} else if first != root {
			return "" // more than one thing at the top level
		}
	}
	return root
}

// entryPath validates an archive entry and removes the wrapper directory,
// returning the path to write relative to the app folder. "" means the entry is
// not one to write (the wrapper itself, or an empty directory beside it).
//
// An entry that would escape the app folder is an error rather than something
// to skip: an archive carrying "../../etc/cron.d/x" is not one to take the rest
// of on trust.
func entryPath(name, root string) (string, error) {
	clean := path.Clean(name)
	if clean == "" || clean == "." {
		return "", nil
	}
	if local := filepath.FromSlash(clean); !filepath.IsLocal(local) {
		return "", fmt.Errorf("archive entry escapes the app folder: %q", name)
	}
	if root != "" {
		if clean == root {
			return "", nil
		}
		trimmed, inside := strings.CutPrefix(clean, root+"/")
		if !inside {
			return "", nil
		}
		clean = trimmed
	}
	return filepath.FromSlash(clean), nil
}

// junk reports whether an entry is packaging debris rather than code: the
// resource-fork tree Finder adds to every zip it makes, and .DS_Store.
//
// Skipping these is not tidiness. Left in, __MACOSX/ gives the archive a second
// top-level directory, and wrapperDir would conclude there is no wrapper to
// strip — so a Mac user's zip would unpack one level too deep, which is exactly
// the case this is all here to get right.
func junk(name string) bool {
	clean := path.Clean(name)
	return clean == "__MACOSX" || strings.HasPrefix(clean, "__MACOSX/") || path.Base(clean) == ".DS_Store"
}
