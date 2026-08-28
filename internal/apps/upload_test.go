package apps

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"xdev/internal/store"
)

// zipOf builds a zip in memory. A name ending in "/" is a directory entry, the
// way a real archiver writes one.
func codeZip(t *testing.T, files map[string]string) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if strings.HasSuffix(n, "/") {
			if _, err := zw.Create(n); err != nil {
				t.Fatal(err)
			}
			continue
		}
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(files[n])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

func codeTarGz(t *testing.T, files map[string]string) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if strings.HasSuffix(n, "/") {
			if err := tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		body := files[n]
		hdr := &tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

// laidOut returns every file under dir as slash-separated relative paths, so a
// test can state the whole result in one comparison.
func laidOut(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestUploadDropsTheWrapperDirectory is the case this whole path exists for.
// Zipping a folder puts everything inside a directory named after it; unpacked
// verbatim, a static app's index.html sits one level below where Caddy serves.
func TestUploadDropsTheWrapperDirectory(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(*testing.T, map[string]string) *bytes.Reader
	}{
		{"zip", codeZip},
		{"tar.gz", codeTarGz},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := tc.make(t, map[string]string{
				"my-site/":            "",
				"my-site/index.html":  "<h1>hi</h1>",
				"my-site/assets/a.js": "console.log(1)",
			})
			if err := extractUpload(src, dir); err != nil {
				t.Fatal(err)
			}
			want := []string{"assets/a.js", "index.html"}
			if got := laidOut(t, dir); !equal(got, want) {
				t.Errorf("unpacked %v, want %v", got, want)
			}
			b, err := os.ReadFile(filepath.Join(dir, "index.html"))
			if err != nil || string(b) != "<h1>hi</h1>" {
				t.Errorf("index.html = %q, %v", b, err)
			}
		})
	}
}

// TestUploadKeepsTopLevelFiles is the other half: an archive made from *inside*
// the folder has no wrapper, and dropping its first directory would throw away
// a real one.
func TestUploadKeepsTopLevelFiles(t *testing.T) {
	dir := t.TempDir()
	src := codeZip(t, map[string]string{
		"index.html":   "<h1>hi</h1>",
		"assets/a.js":  "console.log(1)",
		"assets/b.css": "body{}",
	})
	if err := extractUpload(src, dir); err != nil {
		t.Fatal(err)
	}
	want := []string{"assets/a.js", "assets/b.css", "index.html"}
	if got := laidOut(t, dir); !equal(got, want) {
		t.Errorf("unpacked %v, want %v", got, want)
	}
}

// TestUploadKeepsTwoTopLevelDirs: two roots is not a wrapper. Stripping the
// first one seen would silently delete the other.
func TestUploadKeepsTwoTopLevelDirs(t *testing.T) {
	dir := t.TempDir()
	src := codeZip(t, map[string]string{
		"api/main.go":  "package main",
		"web/index.js": "1",
	})
	if err := extractUpload(src, dir); err != nil {
		t.Fatal(err)
	}
	want := []string{"api/main.go", "web/index.js"}
	if got := laidOut(t, dir); !equal(got, want) {
		t.Errorf("unpacked %v, want %v", got, want)
	}
}

// TestUploadIgnoresFinderJunk covers the archive a Mac user actually produces.
// __MACOSX/ is a second top-level directory, so left in it would defeat the
// wrapper detection and unpack the site one level too deep.
func TestUploadIgnoresFinderJunk(t *testing.T) {
	dir := t.TempDir()
	src := codeZip(t, map[string]string{
		"my-site/index.html":            "<h1>hi</h1>",
		"my-site/.DS_Store":             "junk",
		"__MACOSX/._my-site":            "junk",
		"__MACOSX/my-site/._index.html": "junk",
	})
	if err := extractUpload(src, dir); err != nil {
		t.Fatal(err)
	}
	want := []string{"index.html"}
	if got := laidOut(t, dir); !equal(got, want) {
		t.Errorf("unpacked %v, want %v — Finder junk should be dropped and the wrapper stripped", got, want)
	}
}

// TestUploadRefusesTraversal: an archive that writes outside the app folder is
// refused whole, not entry by entry.
func TestUploadRefusesTraversal(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(*testing.T, map[string]string) *bytes.Reader
	}{
		{"zip", codeZip},
		{"tar.gz", codeTarGz},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// A real file alongside it, so a refusal has to come from the
			// escaping entry rather than from the archive turning out empty.
			src := tc.make(t, map[string]string{
				"index.html":     "hi",
				"../escaped.txt": "no",
			})
			err := extractUpload(src, dir)
			if err == nil {
				t.Fatal("an archive escaping the app folder was accepted")
			}
			if !strings.Contains(err.Error(), "escapes") {
				t.Errorf("err = %v, want it to name the escaping entry", err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escaped.txt")); err == nil {
				t.Fatal("the escaping entry was written")
			}
		})
	}
}

// TestUploadRejectsOtherFormats: the format is decided by the bytes, so a .zip
// that is really something else fails with a sentence rather than a stack of
// unpacked garbage.
func TestUploadRejectsOtherFormats(t *testing.T) {
	dir := t.TempDir()
	err := extractUpload(bytes.NewReader([]byte("Rar!\x1a\x07\x00some rar file")), dir)
	if !errors.Is(err, ErrNotAnArchive) {
		t.Fatalf("err = %v, want ErrNotAnArchive", err)
	}
}

// TestUploadRejectsEmpty: an archive of an empty folder would otherwise create
// an app that serves a blank page with nothing to explain it.
func TestUploadRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := extractUpload(codeZip(t, map[string]string{"empty/": ""}), dir); !errors.Is(err, ErrEmptyArchive) {
		t.Fatalf("zip err = %v, want ErrEmptyArchive", err)
	}
	if err := extractUpload(codeTarGz(t, map[string]string{"empty/": ""}), dir); !errors.Is(err, ErrEmptyArchive) {
		t.Fatalf("tar err = %v, want ErrEmptyArchive", err)
	}
}

// TestUploadKeepsTheExecutableBit — a command-mode app's start command is often
// a script from the archive, and a script that arrives 0644 cannot be run.
func TestUploadKeepsTheExecutableBit(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range []struct {
		name string
		mode os.FileMode
	}{{"app/run.sh", 0o755}, {"app/readme.md", 0o644}} {
		hdr := &zip.FileHeader{Name: f.name, Method: zip.Deflate}
		hdr.SetMode(f.mode)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extractUpload(bytes.NewReader(buf.Bytes()), dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("run.sh unpacked as %v — the executable bit was lost", info.Mode().Perm())
	}
}

// TestUploadFromAPlainReader covers the fallback in positioned(): an upload
// that is not already seekable still has to work, since nothing in the type
// system stops a caller passing one.
func TestUploadFromAPlainReader(t *testing.T) {
	dir := t.TempDir()
	src := codeZip(t, map[string]string{"site/index.html": "hi"})
	// Wrapping in a struct with only Read hides the ReaderAt and Seeker the
	// bytes.Reader would otherwise have offered.
	if err := extractUpload(struct{ io.Reader }{src}, dir); err != nil {
		t.Fatal(err)
	}
	if got := laidOut(t, dir); !equal(got, []string{"index.html"}) {
		t.Errorf("unpacked %v, want [index.html]", got)
	}
}

// TestCreateStaticFromUpload is the feature end to end: a static app whose
// files come out of an archive rather than a scaffold.
//
// The archive deliberately has no index.html, because that is the only way to
// see the difference. writeStaticPlaceholder skips a directory that already has
// one, so an upload that includes index.html would look identical whether or
// not the placeholder step ran.
func TestCreateStaticFromUpload(t *testing.T) {
	s, _, proj := editFixture(t)

	app, err := s.Create(proj.ID, CreateOpts{
		Name:   "site",
		Type:   store.TypeStatic,
		Domain: "up.demo.test",
		Upload: codeZip(t, map[string]string{
			"my-site/hello.txt":   "hi",
			"my-site/css/app.css": "body{}",
		}),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	dir := filepath.Join(proj.Dir, app.Slug)
	if got := laidOut(t, dir); !equal(got, []string{"css/app.css", "hello.txt"}) {
		t.Errorf("app folder holds %v, want the archive's files with the wrapper dropped", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err == nil {
		t.Error("the placeholder index.html was written over the uploaded code")
	}
}

// TestCreateStaticFromUploadRefusesASecondSource: an upload unpacked over a
// checkout is thrown away by the next deploy, and unpacked over a folder the
// user owns it overwrites files xdev promised not to touch. Both are refused
// rather than silently resolved in some order.
func TestCreateStaticFromUploadRefusesASecondSource(t *testing.T) {
	s, _, proj := editFixture(t)
	own := t.TempDir()

	for name, opts := range map[string]CreateOpts{
		"a repository": {Name: "a", Type: store.TypeStatic, Git: GitOpts{URL: "owner/repo"}},
		"a folder":     {Name: "b", Type: store.TypeStatic, SourceDir: own},
	} {
		opts.Upload = codeZip(t, map[string]string{"site/index.html": "hi"})
		if _, err := s.Create(proj.ID, opts); err == nil {
			t.Errorf("an upload combined with %s was accepted", name)
		}
	}
}

// TestCreateRejectsUploadForAContainerType: a WordPress app's files come from
// its image, so an archive would be unpacked where nothing reads it. Saying so
// beats accepting the upload and dropping it.
func TestCreateRejectsUploadForAContainerType(t *testing.T) {
	s, _, proj := editFixture(t)
	_, err := s.Create(proj.ID, CreateOpts{
		Name:   "blog",
		Type:   "wordpress",
		Upload: codeZip(t, map[string]string{"site/index.html": "hi"}),
	})
	if err == nil {
		t.Fatal("a wordpress app was created from an uploaded archive")
	}
	if !strings.Contains(err.Error(), "static") {
		t.Errorf("error %q does not say which types can take an upload", err)
	}
}
