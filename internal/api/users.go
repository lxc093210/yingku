package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"yingku/internal/store"
)

// ---- accounts: web login, bootstrap, user management (batch 12) ----
//
// The web UI logs in with username/password and gets a per-user access
// token (the same tokens Emby clients receive from
// AuthenticateByName). Management endpoints require the server API
// key or an administrator's token.

type userDTO struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsAdmin   bool   `json:"isAdmin"`
	CreatedAt int64  `json:"createdAt,omitempty"`
}

func toUserDTO(u *store.User) userDTO {
	return userDTO{ID: u.ID, Name: u.Name, IsAdmin: u.IsAdmin, CreatedAt: u.CreatedAt}
}

// verifyLogin checks a username/password pair and returns the user
// (nil when the name is unknown or the password wrong — deliberately
// indistinguishable).
func (s *Server) verifyLogin(name, password string) (*store.User, error) {
	u, err := s.st.GetUserByName(strings.TrimSpace(name))
	if err != nil {
		return nil, err
	}
	if u == nil || !store.VerifyPassword(password, u.PasswordHash) {
		return nil, nil
	}
	return u, nil
}

// requireAdmin reports whether the request may manage users: server
// API key, or a token belonging to an administrator.
func (s *Server) requireAdmin(r *http.Request) bool {
	if isServerCred(r) {
		return true
	}
	u := s.currentUser(r)
	return u != nil && u.IsAdmin && presentedUserToken(r) != ""
}

// handleAuthMe is the frontend's session probe: bootstrap state when
// no accounts exist, the current account for valid credentials, and
// 401 when accounts exist but the request carries none — the login
// screen keys off exactly these three outcomes.
func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	n, err := s.st.CountUsers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"bootstrapNeeded": true})
		return
	}
	if isServerCred(r) {
		admin, err := s.st.FirstAdmin()
		if err != nil || admin == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		d := toUserDTO(admin)
		writeJSON(w, http.StatusOK, map[string]any{
			"id": d.ID, "name": d.Name, "isAdmin": true, "serverKey": true,
		})
		return
	}
	if presentedUserToken(r) == "" {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	u := s.currentUser(r)
	if u == nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	d := toUserDTO(u)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": d.ID, "name": d.Name, "isAdmin": d.IsAdmin,
	})
}

type loginPayload struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// loginResponse answers a successful login/bootstrap: the account and
// a fresh access token.
func (s *Server) loginResponse(w http.ResponseWriter, u *store.User) {
	token, err := s.st.IssueToken(u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":        toUserDTO(u),
		"accessToken": token,
	})
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var p loginPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	u, err := s.verifyLogin(p.Username, p.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if u == nil {
		writeErr(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	s.loginResponse(w, u)
}

// handleAuthLogout revokes the presented user token (server-key
// requests have nothing to revoke and simply succeed).
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if tok := presentedUserToken(r); tok != "" {
		if err := s.st.RevokeToken(tok); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type bootstrapPayload struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

// handleUserBootstrap creates the very first account (an
// administrator) — only while no accounts exist. The new admin adopts
// the pre-account watch state, and is logged in immediately.
func (s *Server) handleUserBootstrap(w http.ResponseWriter, r *http.Request) {
	n, err := s.st.CountUsers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n > 0 {
		writeErr(w, http.StatusConflict, "users already exist")
		return
	}
	var p bootstrapPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	name := strings.TrimSpace(p.Name)
	if name == "" || p.Password == "" {
		writeErr(w, http.StatusBadRequest, "name and password are required")
		return
	}
	u, err := s.st.CreateUser(name, p.Password, true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.st.MigrateLegacyUserData(u.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.loginResponse(w, u)
}

func (s *Server) handleUsersList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(r) {
		writeErr(w, http.StatusForbidden, "admin required")
		return
	}
	users, err := s.st.ListUsers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]userDTO, 0, len(users))
	for _, u := range users {
		out = append(out, toUserDTO(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

type createUserPayload struct {
	Name     string `json:"name"`
	Password string `json:"password"`
	IsAdmin  bool   `json:"isAdmin"`
}

func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(r) {
		writeErr(w, http.StatusForbidden, "admin required")
		return
	}
	var p createUserPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	name := strings.TrimSpace(p.Name)
	if name == "" || p.Password == "" {
		writeErr(w, http.StatusBadRequest, "name and password are required")
		return
	}
	if existing, err := s.st.GetUserByName(name); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	} else if existing != nil {
		writeErr(w, http.StatusConflict, "user name already exists")
		return
	}
	u, err := s.st.CreateUser(name, p.Password, p.IsAdmin)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": toUserDTO(u)})
}

// handleUserDelete removes an account, with two guards: an
// administrator account can never be deleted while it is the last one,
// and a token-authenticated user cannot delete their own account.
func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(r) {
		writeErr(w, http.StatusForbidden, "admin required")
		return
	}
	id := r.PathValue("id")
	target, err := s.st.GetUserByID(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if target == nil {
		writeErr(w, http.StatusNotFound, "user not found")
		return
	}
	if !isServerCred(r) && s.currentUserID(r) == id {
		writeErr(w, http.StatusForbidden, "cannot delete your own account")
		return
	}
	if target.IsAdmin {
		admins, err := s.st.CountAdmins()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if admins <= 1 {
			writeErr(w, http.StatusForbidden, "cannot delete the last administrator")
			return
		}
	}
	if err := s.st.DeleteUser(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type passwordPayload struct {
	Password string `json:"password"`
}

// handleUserPassword sets a user's password: administrators may reset
// anyone's; a non-admin token holder may only change their own.
// Changing a password revokes that user's tokens; when the caller
// changed their own, a fresh token is returned so the web session
// survives.
func (s *Server) handleUserPassword(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	self := presentedUserToken(r) != "" && s.currentUserID(r) == id
	if !s.requireAdmin(r) && !self {
		writeErr(w, http.StatusForbidden, "admin required")
		return
	}
	target, err := s.st.GetUserByID(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if target == nil {
		writeErr(w, http.StatusNotFound, "user not found")
		return
	}
	var p passwordPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if p.Password == "" {
		writeErr(w, http.StatusBadRequest, "password is required")
		return
	}
	if err := s.st.SetUserPassword(id, p.Password); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := map[string]any{"ok": true}
	if self {
		token, err := s.st.IssueToken(id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp["accessToken"] = token
	}
	writeJSON(w, http.StatusOK, resp)
}
