package server

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBackupStats writes a couple of archives under the per-app layout and
// checks the aggregate count and that the newest modtime wins.
func TestBackupStats(t *testing.T) {
	root := t.TempDir()

	// Empty root: no archives.
	if n, latest := backupStats(root); n != 0 || !latest.IsZero() {
		t.Fatalf("empty root: got count=%d latest=%v, want 0/zero", n, latest)
	}

	older := filepath.Join(root, "demo_api", "20240101-000000.tar.gz")
	newer := filepath.Join(root, "demo_web", "20240202-000000.tar.gz")
	for _, p := range []string{older, newer} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldT := time.Now().Add(-48 * time.Hour)
	newT := time.Now().Add(-1 * time.Hour)
	os.Chtimes(older, oldT, oldT)
	os.Chtimes(newer, newT, newT)

	n, latest := backupStats(root)
	if n != 2 {
		t.Fatalf("count: got %d, want 2", n)
	}
	if latest.Unix() != newT.Unix() {
		t.Fatalf("latest: got %v, want %v (the newer archive)", latest, newT)
	}
}

func TestHumanizeSince(t *testing.T) {
	now := time.Now()
	cases := []struct {
		t    time.Time
		want string
	}{
		{now.Add(-10 * time.Second), "just now"},
		{now.Add(-5 * time.Minute), "5m ago"},
		{now.Add(-3 * time.Hour), "3h ago"},
		{now.Add(-50 * time.Hour), "2d ago"},
	}
	for _, c := range cases {
		if got := humanizeSince(c.t); got != c.want {
			t.Errorf("humanizeSince(%v): got %q, want %q", c.t, got, c.want)
		}
	}
}

// TestSuppliedComposeFile covers how a "Compose" app's file reaches the apps
// service: an uploaded compose.yml wins, the pasted textarea is the fallback,
// neither means "" (the app gets the starter file), and a file that isn't YAML
// or is oversized is rejected at the boundary.
func TestSuppliedComposeFile(t *testing.T) {
	const yaml = "services:\n  web:\n    image: nginx\n"

	// Plain (non-multipart) form: the textarea is all there is.
	r := httptest.NewRequest(http.MethodPost, "/projects/demo/apps",
		strings.NewReader(url.Values{"compose_yaml": {yaml}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got, err := suppliedComposeFile(r); err != nil || got != yaml {
		t.Errorf("pasted yaml: got %q, %v; want the textarea contents", got, err)
	}

	// Multipart with an upload: the file wins over the textarea.
	r = multipartRequest(t, map[string]string{"compose_yaml": "pasted: ignored\n"}, "compose_file", "compose.yml", yaml)
	if got, err := suppliedComposeFile(r); err != nil || got != yaml {
		t.Errorf("uploaded file: got %q, %v; want the uploaded contents", got, err)
	}

	// Multipart without an upload: fall back to the textarea.
	r = multipartRequest(t, map[string]string{"compose_yaml": yaml}, "", "", "")
	if got, err := suppliedComposeFile(r); err != nil || got != yaml {
		t.Errorf("multipart, no file: got %q, %v; want the textarea contents", got, err)
	}

	// Nothing supplied at all: the app gets the starter compose file.
	r = multipartRequest(t, nil, "", "", "")
	if got, err := suppliedComposeFile(r); err != nil || got != "" {
		t.Errorf("nothing supplied: got %q, %v; want empty", got, err)
	}

	// Rejected at the boundary.
	r = multipartRequest(t, nil, "compose_file", "stack.txt", yaml)
	if _, err := suppliedComposeFile(r); err == nil {
		t.Error("a non-YAML filename should be rejected")
	}
	r = multipartRequest(t, nil, "compose_file", "compose.yml", strings.Repeat("x", maxComposeUpload+1))
	if _, err := suppliedComposeFile(r); err == nil {
		t.Error("an oversized compose file should be rejected")
	}
}

// multipartRequest builds a parsed multipart POST with the given text fields and
// (when field is non-empty) one uploaded file — the shape handleAppCreate sees
// after uploadedArchive has parsed the form.
func multipartRequest(t *testing.T, fields map[string]string, field, filename, body string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if field != "" {
		w, err := mw.CreateFormFile(field, filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	mw.Close()

	r := httptest.NewRequest(http.MethodPost, "/projects/demo/apps", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		t.Fatal(err)
	}
	return r
}
