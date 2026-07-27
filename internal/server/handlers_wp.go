package server

import (
	"archive/zip"
	"net/http"
	"net/url"
	"path/filepath"
)

// handleWordPress renders the central WordPress plugin/theme pool page: shared
// installs used by every shared-host site, alongside each site's own.
func (s *Server) handleWordPress(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "wordpress", viewData{
		"Title":   "WordPress · xdev",
		"Plugins": s.apps.WPPoolList("plugins"),
		"Themes":  s.apps.WPPoolList("themes"),
		"Msg":     r.URL.Query().Get("msg"),
		"Err":     r.URL.Query().Get("err"),
	})
}

// handleWPPoolUpload accepts a plugin/theme .zip and adds it to the shared pool.
func (s *Server) handleWPPoolUpload(w http.ResponseWriter, r *http.Request) {
	kind := r.FormValue("kind")
	if err := r.ParseMultipartForm(128 << 20); err != nil {
		s.redirectWP(w, r, "", "upload too large or malformed")
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		s.redirectWP(w, r, "", "choose a .zip file to upload")
		return
	}
	defer file.Close()
	zr, err := zip.NewReader(file, hdr.Size)
	if err != nil {
		s.redirectWP(w, r, "", "not a valid .zip file")
		return
	}
	if err := s.apps.WPPoolAdd(kind, hdr.Filename, zr); err != nil {
		s.redirectWP(w, r, "", firstLine(err.Error()))
		return
	}
	name := filepath.Base(hdr.Filename)
	s.store.AddEvent(0, 0, "info", "Added shared WordPress "+kind+" "+name)
	s.redirectWP(w, r, "Added "+name+" to shared "+kind+".", "")
}

// handleWPPoolRemove removes a shared plugin/theme (unlinking it from sites).
func (s *Server) handleWPPoolRemove(w http.ResponseWriter, r *http.Request) {
	kind, name := r.FormValue("kind"), r.FormValue("name")
	if err := s.apps.WPPoolRemove(kind, name); err != nil {
		s.redirectWP(w, r, "", firstLine(err.Error()))
		return
	}
	s.redirectWP(w, r, "Removed "+name+" from shared "+kind+".", "")
}

// redirectWP returns to /wordpress with a flash message.
func (s *Server) redirectWP(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	q := url.Values{}
	if msg != "" {
		q.Set("msg", msg)
	}
	if errMsg != "" {
		q.Set("err", errMsg)
	}
	dest := "/wordpress"
	if e := q.Encode(); e != "" {
		dest += "?" + e
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}
