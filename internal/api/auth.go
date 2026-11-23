package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"

	"yingku/internal/store"
)

// ---- API key, user tokens & Emby-style auth ----
//
// Two credential classes ride the same Emby-style carriers (an
// `api_key` query parameter, an `X-Emby-Token` header, or an
// `X-Emby-Authorization` header carrying Token="…"):
//
//   - the server API key, for companion software (Nan's Emby helper,
//     MoviePilot) calling server-to-server;
//   - per-user access tokens issued at login (batch 12), which pin the
//     request's watch state to that account.
//
// Enforcement is opt-in: while app_settings `api_auth_required` is not
// "true" (the default), credentials are only *recognized* (recorded in
// the request context) and every request passes. When the user flips
// the switch, API requests without a valid credential get 401 —
// static pages and the login/bootstrap endpoints stay open.

const (
	// settingAPIKey is the app_settings key holding the API key.
	settingAPIKey = "api_key"
	// settingAPIAuthRequired is the app_settings key for the auth switch
	// ("true" enforces, anything else — including unset — does not).
	settingAPIAuthRequired = "api_auth_required"
	// settingServerID is the app_settings key holding the stable server
	// id reported by /emby/System/Info.
	settingServerID = "server_id"
)

// serverVersion is reported by /emby/System/Info.
const serverVersion = "0.1.0"

// newToken returns n random bytes hex-encoded (2n chars).
func newToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ensureAPIKey returns the stored API key, generating and storing one on
// first use so the settings page always has a usable key to show.
func (s *Server) ensureAPIKey() (string, error) {
	key, err := s.st.GetSetting(settingAPIKey)
	if err != nil {
		return "", err
	}
	if key != "" {
		return key, nil
	}
	key, err = newToken(32)
	if err != nil {
		return "", err
	}
	if err := s.st.SetSetting(settingAPIKey, key); err != nil {
		return "", err
	}
	return key, nil
}

// currentAPIKey is the stored key ("" when none); never generates.
func (s *Server) currentAPIKey() string {
	key, err := s.st.GetSetting(settingAPIKey)
	if err != nil {
		return ""
	}
	return key
}

// ensureServerID returns the stable server id, generating it once.
func (s *Server) ensureServerID() string {
	id, err := s.st.GetSetting(settingServerID)
	if err == nil && id != "" {
		return id
	}
	id, err = newToken(16)
	if err != nil {
		return ""
	}
	_ = s.st.SetSetting(settingServerID, id)
	return id
}

// authRequired reports whether API-key enforcement is switched on.
func (s *Server) authRequired() bool {
	v, err := s.st.GetSetting(settingAPIAuthRequired)
	return err == nil && v == "true"
}

var tokenParamRe = regexp.MustCompile(`(?i)Token\s*=\s*(?:"([^"]*)"|([^,\s"]+))`)

// presentedKey extracts the API key a request carries, Emby-style: the
// api_key query parameter, the X-Emby-Token header, or the Token="…"
// part of an X-Emby-Authorization header. "" when none is present.
func presentedKey(r *http.Request) string {
	if k := r.URL.Query().Get("api_key"); k != "" {
		return k
	}
	if k := r.Header.Get("X-Emby-Token"); k != "" {
		return k
	}
	if h := r.Header.Get("X-Emby-Authorization"); h != "" {
		if m := tokenParamRe.FindStringSubmatch(h); m != nil {
			if m[1] != "" {
				return m[1]
			}
			return m[2]
		}
	}
	return ""
}

// keyMatches reports whether presented is the stored key (constant time;
// an empty presented or stored key never matches).
func keyMatches(presented, stored string) bool {
	if presented == "" || stored == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(stored)) == 1
}

type ctxKeyAuthed struct{}

// authInfo is what the middleware resolved from the request's
// credential: either the server API key (Nan's server-to-server calls)
// or a per-user access token issued at login (batch 12).
type authInfo struct {
	authed bool   // any valid credential was presented
	server bool   // the credential was the server API key
	userID string // token owner, when the credential was a user token
	token  string // presented user token (for logout/revoke), "" otherwise
}

// requestAuthed reports whether the request presented a valid
// credential (server key or user token). Handlers can use it to
// distinguish authenticated companions (Nan, clients) from anonymous
// web-UI traffic once enforcement exists.
func requestAuthed(r *http.Request) bool {
	info, _ := r.Context().Value(ctxKeyAuthed{}).(authInfo)
	return info.authed
}

// isServerCred reports whether the request authenticated with the
// server API key (as opposed to a per-user token or nothing).
func isServerCred(r *http.Request) bool {
	info, _ := r.Context().Value(ctxKeyAuthed{}).(authInfo)
	return info.server
}

// presentedUserToken returns the user access token the request
// carried ("" for server-key or anonymous requests).
func presentedUserToken(r *http.Request) string {
	info, _ := r.Context().Value(ctxKeyAuthed{}).(authInfo)
	return info.token
}

// currentUserID resolves the account a request's watch state belongs
// to: the token's owner for user-token requests; otherwise (server
// key, or no credential while enforcement is off) the first
// administrator; and while no accounts exist at all, the legacy
// default user so pre-account data keeps flowing.
func (s *Server) currentUserID(r *http.Request) string {
	info, _ := r.Context().Value(ctxKeyAuthed{}).(authInfo)
	if info.userID != "" {
		return info.userID
	}
	if admin, err := s.st.FirstAdmin(); err == nil && admin != nil {
		return admin.ID
	}
	return defaultUserID
}

// currentUser resolves the account behind currentUserID (nil while no
// accounts exist, or when the id names no real user).
func (s *Server) currentUser(r *http.Request) *store.User {
	u, err := s.st.GetUserByID(s.currentUserID(r))
	if err != nil {
		return nil
	}
	return u
}

// isAPIPath reports whether path is an API surface (as opposed to the
// static web UI): yingku's own /api/, plus the Emby-style /emby/ and
// root-level Emby routes.
func isAPIPath(path string) bool {
	for _, p := range []string{"/api/", "/emby/", "/System/", "/Users/",
		"/Library/", "/Items/", "/Videos/", "/Sessions/", "/Shows/", "/Search/"} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// authExemptPath lists the endpoints that must stay reachable without
// any credential even when enforcement is on: liveness, the login
// endpoints themselves (they are how credentials are obtained), the
// frontend's session probe, and the one-time bootstrap.
func authExemptPath(path string) bool {
	switch path {
	case "/api/health", "/api/auth/login", "/api/auth/me", "/api/users/bootstrap":
		return true
	}
	return strings.HasSuffix(path, "/Users/AuthenticateByName")
}

// authMiddleware recognizes Emby-style credentials on every request
// and records the outcome in the request context: a presented value
// matching the server API key is a server credential; otherwise it is
// tried as a per-user access token. It only rejects when the user has
// switched enforcement on (api_auth_required=true); then API requests
// without a valid credential get 401, while static pages and the
// exempt endpoints stay reachable so the UI can load and users can
// log in.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := presentedKey(r)
		info := authInfo{}
		switch {
		case keyMatches(presented, s.currentAPIKey()):
			info.authed, info.server = true, true
		case presented != "":
			if uid, ok, err := s.st.ResolveToken(presented); err == nil && ok {
				info.authed, info.userID, info.token = true, uid, presented
			}
		}
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyAuthed{}, info))
		if s.authRequired() && !info.authed && isAPIPath(r.URL.Path) && !authExemptPath(r.URL.Path) {
			writeErr(w, http.StatusUnauthorized, "unauthorized: valid api key required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- handlers ----

func (s *Server) handleAPIKeyGet(w http.ResponseWriter, r *http.Request) {
	key, err := s.ensureAPIKey()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"apiKey": key})
}

func (s *Server) handleAPIKeyRegenerate(w http.ResponseWriter, r *http.Request) {
	key, err := newToken(32)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.st.SetSetting(settingAPIKey, key); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"apiKey": key})
}
