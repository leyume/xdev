package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"xdev/internal/store"
)

// actionFixture is deployFixture's server with a Laravel app added, which is
// the only type that has maintenance commands.
func actionFixture(t *testing.T) (*Server, store.App) {
	t.Helper()
	srv, static, _, _ := deployFixture(t)
	app, err := srv.store.CreateApp(store.App{
		ProjectID: static.ProjectID, Name: "api", Slug: "api", Type: "laravel",
		Domain: "api.demo.test", Runtime: "xdev-no-such-engine",
		ComposePath: t.TempDir() + "/compose.yml", Status: store.AppRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, app
}

func postAction(t *testing.T, srv *Server, appID int64, key, accept string) *httptest.ResponseRecorder {
	t.Helper()
	body := strings.NewReader("action=" + key)
	r := httptest.NewRequest("POST", "/apps/"+strconv.FormatInt(appID, 10)+"/action", body)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	r.SetPathValue("id", strconv.FormatInt(appID, 10))
	w := httptest.NewRecorder()
	srv.handleAppAction(w, r)
	return w
}

// A command that fails is a normal answer, not a transport error: the request
// succeeded, the command didn't, and its output is the entire point. Answering
// non-2xx would send the client down a path with no body to show.
func TestActionJSONReportsAFailedCommandWith200(t *testing.T) {
	srv, app := actionFixture(t)

	// The engine does not exist, so the command cannot run — exactly the shape
	// of a command that fails, without needing docker in the test environment.
	w := postAction(t, srv, app.ID, "migrate-status", "application/json")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 — a failed command still has output to show", w.Code)
	}
	var got struct {
		Label  string `json:"label"`
		Output string `json:"output"`
		Failed bool   `json:"failed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("answer is not JSON: %v\n%s", err, w.Body.String())
	}
	if !got.Failed {
		t.Error("a command that could not run is not marked failed")
	}
	if got.Label != "Migration status" {
		t.Errorf("label = %q, want the action's own name", got.Label)
	}
	if strings.TrimSpace(got.Output) == "" {
		t.Error("no output at all — the row would open on an empty box")
	}
}

// An unknown key means nothing ran, so there is nothing to show: that is a
// request error, and the client should treat it as one.
func TestActionJSONRejectsAnUnknownCommand(t *testing.T) {
	srv, app := actionFixture(t)
	w := postAction(t, srv, app.ID, "rm -rf /", "application/json")
	if w.Code != 400 {
		t.Errorf("status = %d, want 400 for an action that is not in the allowlist", w.Code)
	}
	if strings.Contains(w.Body.String(), "rm -rf") {
		t.Error("the rejected input is echoed back into the page")
	}
}

// Without the JSON Accept header the old behaviour stands: the page is
// re-rendered with the output, so the section works with no JavaScript.
func TestActionWithoutJSONStillRendersThePage(t *testing.T) {
	srv, app := actionFixture(t)
	w := postAction(t, srv, app.ID, "migrate-status", "")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<!DOCTYPE html>") {
		t.Error("the no-JS path no longer renders a page")
	}
	if !strings.Contains(body, "Migration status") {
		t.Error("the rendered page does not name the command that ran")
	}
}

// End to end through the real router: session cookie, CSRF, multipart body —
// the same request the page makes. The direct-handler tests above skip all of
// that, and it is exactly where a "could not reach xdev" in the browser comes
// from: any answer that is not JSON (a CSRF rejection, a login redirect, an
// older server rendering a page) lands in the client's catch.
func TestActionThroughTheRouterAnswersJSON(t *testing.T) {
	srv, app := actionFixture(t)

	user, err := srv.store.CreateUser("a@b.co", "hash")
	if err != nil {
		t.Fatal(err)
	}
	const token, csrf = "sess-token", "csrf-token"
	if err := srv.store.CreateSession(token, user.ID, csrf, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	// Multipart, because that is what fetch sends for a FormData body.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("csrf_token", csrf)
	mw.WriteField("action", "migrate-status")
	mw.Close()

	r := httptest.NewRequest("POST", "/apps/"+strconv.FormatInt(app.ID, 10)+"/action", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("Accept", "application/json")
	r.AddCookie(&http.Cookie{Name: "xdev_session", Value: token})

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)

	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("answer is %s, not JSON (status %d) — the page would show \"could not reach xdev\"\n%s",
			ct, w.Code, firstN(w.Body.String(), 200))
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if got["label"] != "Migration status" {
		t.Errorf("label = %v", got["label"])
	}
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
