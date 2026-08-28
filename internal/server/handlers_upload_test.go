package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"xdev/internal/store"
)

// codeZipBytes builds a zip of name->contents, the shape a browser uploads.
func codeZipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// createRequest builds the multipart POST the add-app dialog sends: text fields
// plus one uploaded file. json picks between the dialog's background path
// (Accept: application/json, answered with a job to poll) and a native submit.
func createRequest(t *testing.T, fields map[string]string, field, filename string, body []byte, json bool) *http.Request {
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
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	mw.Close()

	r := httptest.NewRequest(http.MethodPost, "/projects/demo/apps", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if json {
		r.Header.Set("Accept", "application/json")
	}
	r.SetPathValue("slug", "demo")
	return r
}

// awaitJob polls a job the way the dialog does, and returns its final snapshot.
func awaitJob(t *testing.T, srv *Server, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r := httptest.NewRequest(http.MethodGet, "/jobs/"+id, nil)
		r.SetPathValue("id", id)
		w := httptest.NewRecorder()
		srv.handleJob(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("poll job %s: status %d", id, w.Code)
		}
		var snap map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
			t.Fatal(err)
		}
		if done, _ := snap["done"].(bool); done {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never finished: %v", id, snap)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCreateStaticFromUploadedZip is the whole feature through the handler: the
// dialog's own request shape, the background job it polls, and the files on
// disk at the end.
//
// The background path is what makes this worth a server test rather than an
// apps one. The uploaded file dies with the request while the create outlives
// it, so the upload has to be spooled to somewhere the job owns — and a spool
// released at the wrong moment fails only here, never in the apps package.
func TestCreateStaticFromUploadedZip(t *testing.T) {
	srv, _, _, _ := deployFixture(t)

	zipped := codeZipBytes(t, map[string]string{
		"my-site/hello.txt":   "hi",
		"my-site/css/app.css": "body{}",
	})
	r := createRequest(t, map[string]string{
		"name": "uploaded", "type": "static", "domain": "up.demo.test", "serve_mode": "serve",
	}, "code_archive", "my-site.zip", zipped, true)
	w := httptest.NewRecorder()
	srv.handleAppCreate(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("create returned %d: %s", w.Code, w.Body)
	}
	var reply map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if reply["job"] == "" {
		t.Fatalf("no job id in %s", w.Body)
	}
	snap := awaitJob(t, srv, reply["job"])
	if msg, _ := snap["error"].(string); msg != "" {
		t.Fatalf("create failed: %s", msg)
	}

	app, ok := appNamed(t, srv, "uploaded")
	if !ok {
		t.Fatal("the app was not saved")
	}
	proj, err := srv.store.ProjectByID(app.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(proj.Dir, app.Slug)
	for _, want := range []string{"hello.txt", filepath.Join("css", "app.css")} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("%s did not reach the app folder: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err == nil {
		t.Error("the placeholder was written over the uploaded code")
	}
}

// TestCreateRejectsNonArchiveUpload: the extension is what the user can see and
// fix, so a wrong one is refused before anything is created.
func TestCreateRejectsNonArchiveUpload(t *testing.T) {
	srv, _, _, _ := deployFixture(t)

	r := createRequest(t, map[string]string{
		"name": "bad", "type": "static", "domain": "bad.demo.test",
	}, "code_archive", "site.rar", []byte("Rar!\x1a\x07\x00"), true)
	w := httptest.NewRecorder()
	srv.handleAppCreate(w, r)

	if w.Code == http.StatusOK {
		t.Fatalf("a .rar upload was accepted: %s", w.Body)
	}
	if !strings.Contains(w.Body.String(), ".zip") {
		t.Errorf("error %s does not say what to upload instead", w.Body)
	}
	if _, ok := appNamed(t, srv, "bad"); ok {
		t.Error("the app was created despite the rejected upload")
	}
}

// appNamed finds an app by slug across every project in the fixture.
func appNamed(t *testing.T, srv *Server, slug string) (store.App, bool) {
	t.Helper()
	projects, err := srv.store.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range projects {
		list, err := srv.store.ListAppsByProject(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range list {
			if a.Slug == slug {
				return a, true
			}
		}
	}
	return store.App{}, false
}

// uploadRequest posts the settings page's upload form for one app.
func uploadRequest(t *testing.T, id int64, filename string, body []byte) *http.Request {
	t.Helper()
	r := createRequest(t, nil, "code_archive", filename, body, false)
	r.SetPathValue("id", strconv.FormatInt(id, 10))
	return r
}

// TestAppUploadPublishesTheBuild is the feature: a static site's files replaced
// from the browser, with no deploy token and no CI.
func TestAppUploadPublishesTheBuild(t *testing.T) {
	srv, app, _, _ := deployFixture(t)

	proj, err := srv.store.ProjectByID(app.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	served := filepath.Join(proj.Dir, app.Slug, app.RootDir)
	if err := os.MkdirAll(served, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(served, "old.html"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	srv.handleAppUpload(w, uploadRequest(t, app.ID, "site.zip",
		codeZipBytes(t, map[string]string{"index.html": "<h1>new</h1>"})))

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "uploaded=1") {
		t.Errorf("Location = %q, want the settings page with a confirmation", loc)
	}
	if _, err := os.Stat(filepath.Join(served, "index.html")); err != nil {
		t.Errorf("the uploaded file was not published: %v", err)
	}
	// Replaced, not merged: a stale page left behind would still be served.
	if _, err := os.Stat(filepath.Join(served, "old.html")); err == nil {
		t.Error("the previous build survived — the swap is meant to replace the folder")
	}
}

// A missing file is the likeliest mistake at this form, and it has to say so
// rather than publish an empty directory over a working site.
func TestAppUploadNeedsAFile(t *testing.T) {
	srv, app, _, _ := deployFixture(t)

	r := createRequest(t, map[string]string{"csrf_token": "x"}, "", "", nil, false)
	r.SetPathValue("id", strconv.FormatInt(app.ID, 10))
	w := httptest.NewRecorder()
	srv.handleAppUpload(w, r)

	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "error=") {
		t.Fatalf("an upload with no file was accepted (Location %q)", loc)
	}
	if !strings.Contains(loc, "zip") {
		t.Errorf("error %q does not say what to upload", loc)
	}
}

// The extension is checked before anything is unpacked, because it is what the
// user can see and fix.
func TestAppUploadRejectsANonArchive(t *testing.T) {
	srv, app, _, _ := deployFixture(t)

	w := httptest.NewRecorder()
	srv.handleAppUpload(w, uploadRequest(t, app.ID, "site.rar", []byte("Rar!\x1a\x07\x00")))

	if !strings.Contains(w.Header().Get("Location"), "error=") {
		t.Fatalf("a .rar was accepted: %s", w.Header().Get("Location"))
	}
}
