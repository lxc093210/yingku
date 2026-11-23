package api

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"yingku/internal/scan"
	"yingku/internal/store"
)

// authHandler returns a handler plus its store so tests can flip the
// api_auth_required setting directly.
func authHandler(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, scan.New(st), ""), st
}

func reqWith(t *testing.T, h http.Handler, method, path string, headers map[string]string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func fetchKey(t *testing.T, h http.Handler) string {
	t.Helper()
	var out struct {
		APIKey string `json:"apiKey"`
	}
	if code := get(t, h, "/api/settings/api-key", &out); code != 200 {
		t.Fatalf("GET api-key code = %d", code)
	}
	if out.APIKey == "" {
		t.Fatal("api-key is empty")
	}
	return out.APIKey
}

func TestAPIKeyLifecycle(t *testing.T) {
	h, st := authHandler(t)

	// Auto-generated on first use: 32 random bytes, hex (64 chars).
	k1 := fetchKey(t, h)
	if len(k1) != 64 {
		t.Fatalf("key length = %d, want 64 hex chars", len(k1))
	}
	if _, err := hex.DecodeString(k1); err != nil {
		t.Fatalf("key is not hex: %v", err)
	}
	// Stable across reads, and present in the store from startup.
	if k := fetchKey(t, h); k != k1 {
		t.Fatalf("key changed between reads: %q vs %q", k, k1)
	}
	if stored, _ := st.GetSetting(settingAPIKey); stored != k1 {
		t.Fatalf("stored key = %q, want %q", stored, k1)
	}

	// Regenerate: new key differs and replaces the stored one.
	code, body := doReq(t, h, http.MethodPost, "/api/settings/api-key/regenerate", "")
	if code != 200 {
		t.Fatalf("regenerate code = %d %s", code, body)
	}
	var out struct {
		APIKey string `json:"apiKey"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	k2 := out.APIKey
	if k2 == "" || k2 == k1 {
		t.Fatalf("regenerated key = %q (old %q)", k2, k1)
	}
	if stored, _ := st.GetSetting(settingAPIKey); stored != k2 {
		t.Fatalf("stored key after regen = %q, want %q", stored, k2)
	}

	// With enforcement on, the old key is dead and the new one works.
	if err := st.SetSetting(settingAPIAuthRequired, "true"); err != nil {
		t.Fatal(err)
	}
	if code, _ := reqWith(t, h, http.MethodGet, "/api/home?api_key="+k1, nil); code != 401 {
		t.Fatalf("old key code = %d, want 401", code)
	}
	if code, _ := reqWith(t, h, http.MethodGet, "/api/home?api_key="+k2, nil); code != 200 {
		t.Fatalf("new key code = %d, want 200", code)
	}
}

func TestPresentedKeyParsing(t *testing.T) {
	mk := func(target string, headers map[string]string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return r
	}
	cases := []struct {
		name    string
		target  string
		headers map[string]string
		want    string
	}{
		{"query", "/api/home?api_key=abc123", nil, "abc123"},
		{"token header", "/api/home", map[string]string{"X-Emby-Token": "tok42"}, "tok42"},
		{"authz quoted", "/api/home", map[string]string{
			"X-Emby-Authorization": `MediaBrowser Client="Nan", Device="NAS", Token="sekret", Version="1.0"`}, "sekret"},
		{"authz bare", "/api/home", map[string]string{
			"X-Emby-Authorization": `Emby Client="X", Token=bare42`}, "bare42"},
		{"query wins", "/api/home?api_key=q1", map[string]string{"X-Emby-Token": "h1"}, "q1"},
		{"none", "/api/home", nil, ""},
		{"authz no token", "/api/home", map[string]string{
			"X-Emby-Authorization": `MediaBrowser Client="Nan"`}, ""},
	}
	for _, c := range cases {
		if got := presentedKey(mk(c.target, c.headers)); got != c.want {
			t.Errorf("%s: presentedKey = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestAuthNotRequiredByDefault(t *testing.T) {
	h, _ := authHandler(t)
	// No key, wrong key: everything passes while the switch is off.
	if code, _ := reqWith(t, h, http.MethodGet, "/api/home", nil); code != 200 {
		t.Fatalf("no-key /api/home = %d, want 200", code)
	}
	if code, _ := reqWith(t, h, http.MethodGet, "/api/home?api_key=wrong", nil); code != 200 {
		t.Fatalf("wrong-key /api/home = %d, want 200", code)
	}
	if code, _ := reqWith(t, h, http.MethodGet, "/emby/System/Info", nil); code != 200 {
		t.Fatalf("no-key System/Info = %d, want 200", code)
	}
}

func TestAuthRequiredEnforcement(t *testing.T) {
	h, st := authHandler(t)
	key := fetchKey(t, h)
	if err := st.SetSetting(settingAPIAuthRequired, "true"); err != nil {
		t.Fatal(err)
	}

	// API without a key (or with a wrong one) is rejected.
	for _, p := range []string{"/api/home", "/api/settings", "/emby/System/Info", "/System/Ping"} {
		if code, _ := reqWith(t, h, http.MethodGet, p, nil); code != 401 {
			t.Errorf("no-key %s = %d, want 401", p, code)
		}
	}
	if code, _ := reqWith(t, h, http.MethodGet, "/api/home?api_key=nope", nil); code != 401 {
		t.Errorf("wrong key = %d, want 401", code)
	}

	// All three Emby-style carriers authenticate.
	if code, _ := reqWith(t, h, http.MethodGet, "/api/home?api_key="+key, nil); code != 200 {
		t.Errorf("query key = %d, want 200", code)
	}
	if code, _ := reqWith(t, h, http.MethodGet, "/api/home",
		map[string]string{"X-Emby-Token": key}); code != 200 {
		t.Errorf("X-Emby-Token = %d, want 200", code)
	}
	if code, _ := reqWith(t, h, http.MethodGet, "/api/home",
		map[string]string{"X-Emby-Authorization": `MediaBrowser Client="Nan", Token="` + key + `"`}); code != 200 {
		t.Errorf("X-Emby-Authorization = %d, want 200", code)
	}
	if code, _ := reqWith(t, h, http.MethodGet, "/emby/System/Info?api_key="+key, nil); code != 200 {
		t.Errorf("emby info with key = %d, want 200", code)
	}

	// Exemptions: health and the static UI stay reachable without a key.
	if code, _ := reqWith(t, h, http.MethodGet, "/api/health", nil); code != 200 {
		t.Errorf("health = %d, want 200", code)
	}
	code, body := reqWith(t, h, http.MethodGet, "/", nil)
	if code != 200 || !strings.Contains(string(body), "<html") {
		t.Errorf("UI root = %d, want 200 HTML", code)
	}
}

func TestSettingsAuthSwitchRoundTrip(t *testing.T) {
	h, _ := authHandler(t)
	var dto settingsDTO
	if code := get(t, h, "/api/settings", &dto); code != 200 || dto.APIAuthRequired {
		t.Fatalf("default settings = %d %+v, want auth off", code, dto)
	}
	if code, body := doReq(t, h, http.MethodPut, "/api/settings", `{"apiAuthRequired":true}`); code != 200 {
		t.Fatalf("PUT settings = %d %s", code, body)
	} else if err := json.Unmarshal(body, &dto); err != nil || !dto.APIAuthRequired {
		t.Fatalf("after PUT dto = %+v err %v", dto, err)
	}
	// Enforcement is live immediately after the PUT.
	if code, _ := reqWith(t, h, http.MethodGet, "/api/home", nil); code != 401 {
		t.Fatalf("after enabling, no-key /api/home = %d, want 401", code)
	}
}

func TestSystemInfoAndPing(t *testing.T) {
	h, _ := authHandler(t)

	var info systemInfoDTO
	if code := get(t, h, "/emby/System/Info", &info); code != 200 {
		t.Fatalf("System/Info code = %d", code)
	}
	if info.ServerName != "影库 Yingku" || info.ProductName != "Yingku" {
		t.Errorf("info names = %q/%q", info.ServerName, info.ProductName)
	}
	if info.Version != serverVersion || info.Version == "" {
		t.Errorf("info version = %q", info.Version)
	}
	if info.Id == "" {
		t.Error("info Id is empty")
	}
	if info.OperatingSystem != "Linux" {
		t.Errorf("info OS = %q (tests run on Linux)", info.OperatingSystem)
	}

	// The /System alias serves the same identity; the Id is stable.
	var alias systemInfoDTO
	if code := get(t, h, "/System/Info", &alias); code != 200 || alias.Id != info.Id {
		t.Fatalf("alias info = %d %+v, want same Id %q", code, alias, info.Id)
	}

	for _, p := range []string{"/emby/System/Ping", "/System/Ping"} {
		code, body := reqWith(t, h, http.MethodGet, p, nil)
		if code != 200 || string(body) != "Yingku Server Ready" {
			t.Errorf("%s = %d %q, want 200 %q", p, code, body, "Yingku Server Ready")
		}
	}
}
