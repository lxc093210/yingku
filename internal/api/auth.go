package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
)

// ---- API key & Emby-style auth ----
//
// yingku is a single-user LAN server. The API key exists so companion
// software (Nan's Emby helper, MoviePilot, Emby clients) can authenticate
// the way they would against a real Emby server: an `api_key` query
// parameter, an `X-Emby-Token` header, or an `X-Emby-Authorization`
// header carrying Token="…".
//
// Enforcement is opt-in: while app_settings `api_auth_required` is not
// "true" (the default), credentials are only *recognized* (recorded in
// the request context) and every request passes, so the key-less web UI
// keeps working. When the user flips the switch, API requests without a
// valid key get 401 — static pages and /api/health stay open.

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

// requestAuthed reports whether the request presented a valid API key.
// Handlers can use it to distinguish key-authenticated companions (Nan,
// clients) from anonymous web-UI traffic once enforcement exists.
func requestAuthed(r *http.Request) bool {
	v, _ := r.Context().Value(ctxKeyAuthed{}).(bool)
	return v
}

// isAPIPath reports whether path is an API surface (as opposed to the
// static web UI): yingku's own /api/, plus the Emby-style /emby/ and
// root-level Emby routes.
func isAPIPath(path string) bool {
	for _, p := range []string{"/api/", "/emby/", "/System/", "/Users/",
		"/Library/", "/Items/", "/Videos/", "/Sessions/"} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// authMiddleware recognizes Emby-style credentials on every request and
// records the outcome in the request context. It only rejects when the
// user has switched enforcement on (api_auth_required=true); then API
// requests without a valid key get 401, while static pages and
// /api/health stay reachable so the UI can load and liveness checks work.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authed := keyMatches(presentedKey(r), s.currentAPIKey())
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyAuthed{}, authed))
		if s.authRequired() && !authed && isAPIPath(r.URL.Path) && r.URL.Path != "/api/health" {
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
