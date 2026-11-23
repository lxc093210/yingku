package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"yingku/internal/scan"
	"yingku/internal/store"
)

// ---- accounts (batch 12) ----

// userTestHandler is testHandler with the store exposed, so tests can
// seed accounts and legacy watch state directly.
func userTestHandler(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	root, _ := filepath.Abs(filepath.Join("..", "..", "fixtures", "library"))
	sc := scan.New(st)
	if err := sc.Scan(root); err != nil {
		t.Fatal(err)
	}
	return New(st, sc, root), st
}

// tokenReq performs a request carrying a user token (or the server
// key) in the X-Emby-Token header.
func tokenReq(t *testing.T, h http.Handler, method, path, body, token string) (int, []byte) {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("X-Emby-Token", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

type loginResult struct {
	User struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		IsAdmin bool   `json:"isAdmin"`
	} `json:"user"`
	AccessToken string `json:"accessToken"`
}

func mustBootstrap(t *testing.T, h http.Handler, name, password string) loginResult {
	t.Helper()
	code, body := doReq(t, h, http.MethodPost, "/api/users/bootstrap",
		`{"name":"`+name+`","password":"`+password+`"}`)
	if code != 200 {
		t.Fatalf("bootstrap = %d %s", code, body)
	}
	var out loginResult
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.AccessToken == "" || out.User.ID == "" {
		t.Fatalf("bootstrap response = %s", body)
	}
	return out
}

func mustLogin(t *testing.T, h http.Handler, name, password string) loginResult {
	t.Helper()
	code, body := doReq(t, h, http.MethodPost, "/api/auth/login",
		`{"username":"`+name+`","password":"`+password+`"}`)
	if code != 200 {
		t.Fatalf("login %s = %d %s", name, code, body)
	}
	var out loginResult
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestWebUIHasAuthScreens pins the login/bootstrap/user-management
// markup and wiring in the served page: if any of these strings go
// missing, the web login flow is broken even though the API works.
func TestWebUIHasAuthScreens(t *testing.T) {
	h, _ := userTestHandler(t)
	code, body := doReq(t, h, http.MethodGet, "/", "")
	if code != 200 {
		t.Fatalf("GET / = %d", code)
	}
	for _, want := range []string{
		`id="authScreen"`, `id="loginForm"`, `id="bootstrapForm"`,
		`id="userAdminSection"`, `id="userList"`, `id="newUserForm"`,
		`id="logoutBtn"`, `id="myPwSave"`,
		"/api/auth/me", "/api/auth/login", "/api/users/bootstrap", "yk_token",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("served page is missing %q", want)
		}
	}
}

func TestBootstrapAndLogin(t *testing.T) {
	h, _ := userTestHandler(t)

	// No accounts yet: the probe says bootstrap is needed.
	code, body := doReq(t, h, http.MethodGet, "/api/auth/me", "")
	if code != 200 || !strings.Contains(string(body), `"bootstrapNeeded":true`) {
		t.Fatalf("me before bootstrap = %d %s", code, body)
	}
	// Login is impossible before any account exists.
	if code, _ := doReq(t, h, http.MethodPost, "/api/auth/login",
		`{"username":"boss","password":"pw"}`); code != 401 {
		t.Fatalf("login before bootstrap = %d, want 401", code)
	}

	admin := mustBootstrap(t, h, "boss", "pw-admin")
	if !admin.User.IsAdmin || admin.User.Name != "boss" {
		t.Fatalf("bootstrap user = %+v", admin.User)
	}
	// Bootstrap is a one-time door.
	if code, body := doReq(t, h, http.MethodPost, "/api/users/bootstrap",
		`{"name":"second","password":"pw"}`); code != 409 {
		t.Fatalf("second bootstrap = %d %s, want 409", code, body)
	}
	// The bootstrap token identifies the admin.
	code, body = tokenReq(t, h, http.MethodGet, "/api/auth/me", "", admin.AccessToken)
	if code != 200 || !strings.Contains(string(body), `"name":"boss"`) ||
		!strings.Contains(string(body), `"isAdmin":true`) {
		t.Fatalf("me with bootstrap token = %d %s", code, body)
	}
	// Without a credential the probe now refuses (login screen time).
	if code, _ := doReq(t, h, http.MethodGet, "/api/auth/me", ""); code != 401 {
		t.Fatalf("me without credential = %d, want 401", code)
	}
	// Wrong password and unknown user both 401; the right pair logs in.
	if code, _ := doReq(t, h, http.MethodPost, "/api/auth/login",
		`{"username":"boss","password":"nope"}`); code != 401 {
		t.Fatalf("wrong-password login = %d, want 401", code)
	}
	if code, _ := doReq(t, h, http.MethodPost, "/api/auth/login",
		`{"username":"ghost","password":"pw-admin"}`); code != 401 {
		t.Fatalf("unknown-user login = %d, want 401", code)
	}
	again := mustLogin(t, h, "boss", "pw-admin")
	if again.User.ID != admin.User.ID || again.AccessToken == admin.AccessToken {
		t.Fatalf("second login = %+v (bootstrap %+v)", again, admin)
	}
	// Logout revokes exactly that token.
	if code, _ := tokenReq(t, h, http.MethodPost, "/api/auth/logout", "", again.AccessToken); code != 200 {
		t.Fatalf("logout = %d", code)
	}
	if code, _ := tokenReq(t, h, http.MethodGet, "/api/auth/me", "", again.AccessToken); code != 401 {
		t.Fatalf("me with revoked token = %d, want 401", code)
	}
	if code, _ := tokenReq(t, h, http.MethodGet, "/api/auth/me", "", admin.AccessToken); code != 200 {
		t.Fatalf("bootstrap token after other logout = %d, want 200", code)
	}
}

func TestTwoUserIsolation(t *testing.T) {
	h, _ := userTestHandler(t)
	movieID, _ := fixtureIDs(t, h)
	admin := mustBootstrap(t, h, "alice", "pw-alice")

	// Admin creates a second, non-admin user; that user logs in.
	code, body := tokenReq(t, h, http.MethodPost, "/api/users",
		`{"name":"bob","password":"pw-bob","isAdmin":false}`, admin.AccessToken)
	if code != 200 {
		t.Fatalf("create bob = %d %s", code, body)
	}
	bob := mustLogin(t, h, "bob", "pw-bob")
	if bob.User.IsAdmin {
		t.Fatal("bob must not be admin")
	}

	// Alice watches part of the movie and favorites it.
	if code, body := tokenReq(t, h, http.MethodPost, "/api/items/"+itoa(movieID)+"/progress",
		`{"positionSeconds":600,"durationSeconds":3600}`, admin.AccessToken); code != 200 {
		t.Fatalf("alice progress = %d %s", code, body)
	}
	if code, body := tokenReq(t, h, http.MethodPost, "/api/items/"+itoa(movieID)+"/favorite",
		`{"favorite":true}`, admin.AccessToken); code != 200 {
		t.Fatalf("alice favorite = %d %s", code, body)
	}

	var detailA, detailB itemDTO
	if code := get(t, h, "/api/items/"+itoa(movieID), &detailA); code != 200 {
		t.Fatalf("anonymous detail = %d", code)
	}
	// Bob's view of the same item is clean.
	code, body = tokenReq(t, h, http.MethodGet, "/api/items/"+itoa(movieID), "", bob.AccessToken)
	if code != 200 || json.Unmarshal(body, &detailB) != nil {
		t.Fatalf("bob detail = %d %s", code, body)
	}
	if detailB.UserData == nil || detailB.UserData.PlaybackPositionTicks != 0 ||
		detailB.UserData.Favorite || detailB.UserData.Played {
		t.Errorf("bob user data = %+v, want untouched", detailB.UserData)
	}
	// Alice's view carries her own state (read back with her token).
	code, body = tokenReq(t, h, http.MethodGet, "/api/items/"+itoa(movieID), "", admin.AccessToken)
	if code != 200 || json.Unmarshal(body, &detailA) != nil {
		t.Fatalf("alice detail = %d %s", code, body)
	}
	if detailA.UserData == nil || detailA.UserData.PlaybackPositionTicks != 600*10_000_000 ||
		!detailA.UserData.Favorite {
		t.Errorf("alice user data = %+v, want 600s + favorite", detailA.UserData)
	}

	// Resume: empty for Bob, holds the movie for Alice.
	var resumeB, resumeA embyPage
	code, body = tokenReq(t, h, http.MethodGet, "/emby/Items/Resume", "", bob.AccessToken)
	if code != 200 || json.Unmarshal(body, &resumeB) != nil || resumeB.TotalRecordCount != 0 {
		t.Errorf("bob resume = %d %s, want empty", code, body)
	}
	code, body = tokenReq(t, h, http.MethodGet, "/emby/Items/Resume", "", admin.AccessToken)
	if code != 200 || json.Unmarshal(body, &resumeA) != nil || resumeA.TotalRecordCount != 1 ||
		len(resumeA.Items) != 1 || resumeA.Items[0].Id != itoa(movieID) {
		t.Errorf("alice resume = %d %s, want the movie", code, body)
	}
	// Home ContinueWatching follows the same split.
	var homeB, homeA struct {
		ContinueWatching []itemDTO `json:"ContinueWatching"`
	}
	code, body = tokenReq(t, h, http.MethodGet, "/api/home", "", bob.AccessToken)
	if code != 200 || json.Unmarshal(body, &homeB) != nil || len(homeB.ContinueWatching) != 0 {
		t.Errorf("bob home = %d %s, want no continue watching", code, body)
	}
	code, body = tokenReq(t, h, http.MethodGet, "/api/home", "", admin.AccessToken)
	if code != 200 || json.Unmarshal(body, &homeA) != nil || len(homeA.ContinueWatching) != 1 {
		t.Errorf("alice home = %d %s, want one continue entry", code, body)
	}
	// Bob's own progress then shows up for Bob only.
	if code, body := tokenReq(t, h, http.MethodPost, "/api/items/"+itoa(movieID)+"/progress",
		`{"positionSeconds":60,"durationSeconds":3600}`, bob.AccessToken); code != 200 {
		t.Fatalf("bob progress = %d %s", code, body)
	}
	code, body = tokenReq(t, h, http.MethodGet, "/api/items/"+itoa(movieID), "", bob.AccessToken)
	if code != 200 || json.Unmarshal(body, &detailB) != nil ||
		detailB.UserData.PlaybackPositionTicks != 60*10_000_000 {
		t.Errorf("bob detail after own progress = %d %s", code, body)
	}
}

func TestUserAdminGuards(t *testing.T) {
	h, _ := userTestHandler(t)
	admin := mustBootstrap(t, h, "root", "pw-root")

	// A second admin and a plain user.
	if code, body := tokenReq(t, h, http.MethodPost, "/api/users",
		`{"name":"admin2","password":"pw-2","isAdmin":true}`, admin.AccessToken); code != 200 {
		t.Fatalf("create admin2 = %d %s", code, body)
	}
	if code, body := tokenReq(t, h, http.MethodPost, "/api/users",
		`{"name":"plain","password":"pw-p","isAdmin":false}`, admin.AccessToken); code != 200 {
		t.Fatalf("create plain = %d %s", code, body)
	}
	// Duplicate names are refused.
	if code, _ := tokenReq(t, h, http.MethodPost, "/api/users",
		`{"name":"plain","password":"pw-x"}`, admin.AccessToken); code != 409 {
		t.Fatalf("duplicate create = %d, want 409", code)
	}
	admin2 := mustLogin(t, h, "admin2", "pw-2")
	plain := mustLogin(t, h, "plain", "pw-p")

	// Non-admins cannot list or create users.
	if code, _ := tokenReq(t, h, http.MethodGet, "/api/users", "", plain.AccessToken); code != 403 {
		t.Fatalf("plain list users = %d, want 403", code)
	}
	if code, _ := tokenReq(t, h, http.MethodPost, "/api/users",
		`{"name":"sneaky","password":"pw"}`, plain.AccessToken); code != 403 {
		t.Fatalf("plain create user = %d, want 403", code)
	}
	// The admin list shows all three accounts.
	code, body := tokenReq(t, h, http.MethodGet, "/api/users", "", admin.AccessToken)
	if code != 200 || strings.Count(string(body), `"name":`) != 3 {
		t.Fatalf("admin list = %d %s", code, body)
	}
	// Nobody deletes their own account.
	if code, _ := tokenReq(t, h, http.MethodDelete, "/api/users/"+admin2.User.ID, "", admin2.AccessToken); code != 403 {
		t.Fatalf("admin2 self-delete = %d, want 403", code)
	}
	// Deleting the other admin is fine (one remains)…
	if code, body := tokenReq(t, h, http.MethodDelete, "/api/users/"+admin2.User.ID, "", admin.AccessToken); code != 200 {
		t.Fatalf("delete admin2 = %d %s", code, body)
	}
	// …but the last administrator is untouchable, even for the server key.
	key := fetchKey(t, h)
	if code, _ := tokenReq(t, h, http.MethodDelete, "/api/users/"+admin.User.ID, "", key); code != 403 {
		t.Fatalf("delete last admin via server key = %d, want 403", code)
	}
	// A plain user CAN be deleted by an admin.
	if code, body := tokenReq(t, h, http.MethodDelete, "/api/users/"+plain.User.ID, "", admin.AccessToken); code != 200 {
		t.Fatalf("delete plain = %d %s", code, body)
	}
	if code, _ := doReq(t, h, http.MethodPost, "/api/auth/login",
		`{"username":"plain","password":"pw-p"}`); code != 401 {
		t.Fatalf("deleted user login = %d, want 401", code)
	}
	// Password reset by an admin works and kills the old password.
	if code, body := tokenReq(t, h, http.MethodPost, "/api/users",
		`{"name":"carol","password":"pw-c"}`, admin.AccessToken); code != 200 {
		t.Fatalf("create carol = %d %s", code, body)
	}
	var carol struct {
		User userDTO `json:"user"`
	}
	code, body = tokenReq(t, h, http.MethodGet, "/api/users", "", admin.AccessToken)
	_ = code
	var list struct {
		Users []userDTO `json:"users"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	for _, u := range list.Users {
		if u.Name == "carol" {
			carol.User = u
		}
	}
	if code, body := tokenReq(t, h, http.MethodPost, "/api/users/"+carol.User.ID+"/password",
		`{"password":"pw-c2"}`, admin.AccessToken); code != 200 {
		t.Fatalf("reset carol password = %d %s", code, body)
	}
	if code, _ := doReq(t, h, http.MethodPost, "/api/auth/login",
		`{"username":"carol","password":"pw-c"}`); code != 401 {
		t.Fatalf("carol old password = %d, want 401", code)
	}
	mustLogin(t, h, "carol", "pw-c2")
}

func TestServerKeyAttributionAndEnforcement(t *testing.T) {
	h, st := userTestHandler(t)
	movieID, _ := fixtureIDs(t, h)
	admin := mustBootstrap(t, h, "owner", "pw-owner")
	key := fetchKey(t, h)

	// Nan-style traffic: server key browses fine…
	if code, _ := tokenReq(t, h, http.MethodGet, "/emby/Library/MediaFolders", "", key); code != 200 {
		t.Fatalf("MediaFolders with server key = %d", code)
	}
	// …and its session reports land on the first administrator.
	code, body := doReq(t, h, http.MethodPost,
		"/emby/Sessions/Playing/Progress?api_key="+key,
		`{"ItemId":`+itoa(movieID)+`,"PositionTicks":3000000000}`)
	if code != 204 {
		t.Fatalf("session report = %d %s", code, body)
	}
	var detail itemDTO
	code, body = tokenReq(t, h, http.MethodGet, "/api/items/"+itoa(movieID), "", admin.AccessToken)
	if code != 200 || json.Unmarshal(body, &detail) != nil ||
		detail.UserData == nil || detail.UserData.PlaybackPositionTicks != 3_000_000_000 {
		t.Errorf("admin detail after server-key report = %d %s", code, body)
	}

	// With enforcement on, user tokens and the server key both pass;
	// anonymous API calls fail; login stays reachable.
	if err := st.SetSetting(settingAPIAuthRequired, "true"); err != nil {
		t.Fatal(err)
	}
	if code, _ := reqWith(t, h, http.MethodGet, "/api/home", nil); code != 401 {
		t.Errorf("enforced anonymous /api/home = %d, want 401", code)
	}
	if code, _ := tokenReq(t, h, http.MethodGet, "/api/home", "", admin.AccessToken); code != 200 {
		t.Errorf("enforced token /api/home = %d, want 200", code)
	}
	if code, _ := tokenReq(t, h, http.MethodGet, "/api/home", "", key); code != 200 {
		t.Errorf("enforced server-key /api/home = %d, want 200", code)
	}
	if code, _ := doReq(t, h, http.MethodPost, "/api/auth/login",
		`{"username":"owner","password":"pw-owner"}`); code != 200 {
		t.Errorf("enforced login = %d, want 200", code)
	}
	if code, _ := doReq(t, h, http.MethodPost, "/emby/Users/AuthenticateByName",
		`{"Username":"owner","Pw":"pw-owner"}`); code != 200 {
		t.Errorf("enforced AuthenticateByName = %d, want 200", code)
	}
}

func TestLegacyDataAdoptedAtBootstrap(t *testing.T) {
	h, st := userTestHandler(t)
	movieID, _ := fixtureIDs(t, h)

	// Pre-account watch state under the legacy user id.
	if err := st.UpsertProgress(movieID, store.LegacyUserID, 777*10_000_000, false, 1700); err != nil {
		t.Fatal(err)
	}
	if err := st.SetFavorite(movieID, store.LegacyUserID, true); err != nil {
		t.Fatal(err)
	}

	admin := mustBootstrap(t, h, "heir", "pw-heir")
	var detail itemDTO
	code, body := tokenReq(t, h, http.MethodGet, "/api/items/"+itoa(movieID), "", admin.AccessToken)
	if code != 200 || json.Unmarshal(body, &detail) != nil {
		t.Fatalf("detail = %d %s", code, body)
	}
	if detail.UserData == nil || detail.UserData.PlaybackPositionTicks != 777*10_000_000 ||
		!detail.UserData.Favorite {
		t.Errorf("adopted user data = %+v, want legacy progress + favorite", detail.UserData)
	}
	// …and it surfaces in the new admin's continue watching.
	var home struct {
		ContinueWatching []itemDTO `json:"ContinueWatching"`
	}
	code, body = tokenReq(t, h, http.MethodGet, "/api/home", "", admin.AccessToken)
	if code != 200 || json.Unmarshal(body, &home) != nil || len(home.ContinueWatching) != 1 {
		t.Errorf("home after adoption = %d %s", code, body)
	}
	if ud, err := st.UserDataOf(movieID, store.LegacyUserID); err != nil || ud.ProgressTicks != 0 {
		t.Errorf("legacy rows after adoption = %+v, %v", ud, err)
	}
}
