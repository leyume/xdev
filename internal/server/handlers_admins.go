package server

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"xdev/internal/auth"
	"xdev/internal/store"
)

// handleAdmins lists admin accounts and renders the add/reset/delete controls.
// xdev has no roles yet, so every account is a full admin.
func (s *Server) handleAdmins(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers()
	if err != nil {
		http.Error(w, "could not load admins", http.StatusInternalServerError)
		return
	}
	s.render(w, r, "admins", viewData{
		"Title":  "Admins · xdev",
		"Admins": users,
		"Msg":    r.URL.Query().Get("msg"),
		"Err":    r.URL.Query().Get("err"),
	})
}

// handleAdminCreate adds another admin account.
func (s *Server) handleAdminCreate(w http.ResponseWriter, r *http.Request) {
	email := r.FormValue("email")
	password := r.FormValue("password")
	if _, err := s.auth.CreateUser(email, password); err != nil {
		s.redirectAdmins(w, r, "", err.Error())
		return
	}
	s.store.AddEvent(0, 0, "info", "Added admin "+auth.NormalizeEmail(email))
	s.redirectAdmins(w, r, "Added admin "+auth.NormalizeEmail(email), "")
}

// handleAdminPassword resets an existing admin's password.
func (s *Server) handleAdminPassword(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	u, err := s.store.UserByID(id)
	if err != nil {
		s.redirectAdmins(w, r, "", "no such admin")
		return
	}
	if err := s.auth.SetPassword(u.Email, r.FormValue("password")); err != nil {
		s.redirectAdmins(w, r, "", err.Error())
		return
	}
	s.store.AddEvent(0, 0, "info", "Reset password for "+u.Email)
	s.redirectAdmins(w, r, "Password reset for "+u.Email, "")
}

// handleAdminDelete removes another admin. You can't delete your own account
// (that both avoids self-lockout and guarantees at least one admin remains).
func (s *Server) handleAdminDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if cur, ok := auth.UserFrom(r); ok && cur.ID == id {
		s.redirectAdmins(w, r, "", "you can't delete the account you're signed in as")
		return
	}
	u, err := s.store.UserByID(id)
	if err != nil {
		s.redirectAdmins(w, r, "", "no such admin")
		return
	}
	if err := s.store.DeleteUser(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.redirectAdmins(w, r, "", "no such admin")
		} else {
			s.redirectAdmins(w, r, "", err.Error())
		}
		return
	}
	s.store.AddEvent(0, 0, "info", "Removed admin "+u.Email)
	s.redirectAdmins(w, r, "Removed admin "+u.Email, "")
}

// redirectAdmins sends the browser back to /admins with a flash message.
func (s *Server) redirectAdmins(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	q := url.Values{}
	if msg != "" {
		q.Set("msg", msg)
	}
	if errMsg != "" {
		q.Set("err", errMsg)
	}
	dest := "/admins"
	if e := q.Encode(); e != "" {
		dest += "?" + e
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}
