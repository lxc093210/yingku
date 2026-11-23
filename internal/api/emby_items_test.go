package api

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// ---- Emby core read surface (batch 9) ----

type embyPage struct {
	Items            []embyItemDTO `json:"Items"`
	TotalRecordCount int           `json:"TotalRecordCount"`
	StartIndex       int           `json:"StartIndex"`
}

// embyFixture resolves the fixture library/item ids in Emby-surface
// terms (library ids are offset, item ids are raw).
func embyFixture(t *testing.T, h http.Handler) (movieLibID, seriesLibID string, movieID, seriesID, seasonID, episodeID int64) {
	t.Helper()
	movieID, seriesID = fixtureIDs(t, h)
	var libs []libraryDTO
	if code := get(t, h, "/api/libraries", &libs); code != 200 {
		t.Fatalf("libraries = %d", code)
	}
	for _, l := range libs {
		if l.Kind == "movie" {
			movieLibID = embyLibraryID(l.ID)
		} else {
			seriesLibID = embyLibraryID(l.ID)
		}
	}
	var detail itemDTO
	if code := get(t, h, "/api/items/"+itoa(seriesID), &detail); code != 200 {
		t.Fatalf("series detail = %d", code)
	}
	if len(detail.Seasons) != 1 || len(detail.Seasons[0].Episodes) == 0 {
		t.Fatalf("fixture seasons = %+v", detail.Seasons)
	}
	seasonID = detail.Seasons[0].ID
	episodeID = detail.Seasons[0].Episodes[0].ID
	return movieLibID, seriesLibID, movieID, seriesID, seasonID, episodeID
}

func TestEmbyMediaFolders(t *testing.T) {
	h := testHandler(t)
	movieLibID, seriesLibID, _, _, _, _ := embyFixture(t, h)

	for _, p := range []string{"/emby/Library/MediaFolders", "/Library/MediaFolders"} {
		var page embyPage
		if code := get(t, h, p, &page); code != 200 {
			t.Fatalf("GET %s = %d", p, code)
		}
		if page.TotalRecordCount != 2 || len(page.Items) != 2 {
			t.Fatalf("media folders = %+v", page)
		}
		byID := map[string]embyItemDTO{}
		for _, it := range page.Items {
			byID[it.Id] = it
			if it.Type != "CollectionFolder" || !it.IsFolder {
				t.Errorf("folder dto = %+v", it)
			}
		}
		if byID[movieLibID].CollectionType != "movies" {
			t.Errorf("movie folder = %+v", byID[movieLibID])
		}
		if byID[seriesLibID].CollectionType != "tvshows" {
			t.Errorf("series folder = %+v", byID[seriesLibID])
		}
	}

	// Items with no scope returns the same folders.
	var page embyPage
	if code := get(t, h, "/emby/Items", &page); code != 200 || page.TotalRecordCount != 2 {
		t.Fatalf("bare Items = %d %+v", code, page)
	}
}

func TestEmbyItemsDrilldown(t *testing.T) {
	h := testHandler(t)
	movieLibID, _, movieID, seriesID, seasonID, _ := embyFixture(t, h)

	// Library -> top-level items.
	var page embyPage
	if code := get(t, h, "/emby/Items?ParentId="+movieLibID, &page); code != 200 {
		t.Fatalf("library items = %d", code)
	}
	if page.TotalRecordCount != 1 || len(page.Items) != 1 || page.Items[0].Id != itoa(movieID) {
		t.Fatalf("library items = %+v", page)
	}
	if page.Items[0].Type != "Movie" || page.Items[0].ParentId != movieLibID {
		t.Errorf("movie dto = %+v", page.Items[0])
	}
	if page.Items[0].ProviderIds["Tmdb"] != "324857" {
		t.Errorf("movie ProviderIds = %v", page.Items[0].ProviderIds)
	}
	// The fixture movie owns local poster+backdrop files.
	if page.Items[0].ImageTags["Primary"] == "" || len(page.Items[0].BackdropImageTags) != 1 {
		t.Errorf("movie image tags = %+v", page.Items[0])
	}

	// Series -> seasons, season -> episodes (via the Users-scoped alias).
	if code := get(t, h, "/emby/Users/anyone/Items?ParentId="+itoa(seriesID), &page); code != 200 {
		t.Fatalf("series items = %d", code)
	}
	if page.TotalRecordCount != 1 || page.Items[0].Id != itoa(seasonID) || page.Items[0].Type != "Season" {
		t.Fatalf("series items = %+v", page)
	}
	if page.Items[0].ChildCount == nil || *page.Items[0].ChildCount != 20 {
		t.Errorf("season ChildCount = %v, want 20", page.Items[0].ChildCount)
	}
	if code := get(t, h, "/Items?ParentId="+itoa(seasonID), &page); code != 200 {
		t.Fatalf("season items = %d", code)
	}
	if page.TotalRecordCount != 20 || len(page.Items) != 20 {
		t.Fatalf("season items total = %d len %d, want 20/20", page.TotalRecordCount, len(page.Items))
	}
	ep := page.Items[0]
	if ep.Type != "Episode" || ep.SeriesId != itoa(seriesID) || ep.SeasonId != itoa(seasonID) {
		t.Errorf("episode dto = %+v", ep)
	}
	if ep.SeriesName != "上海女子图鉴" {
		t.Errorf("episode SeriesName = %q", ep.SeriesName)
	}
	if ep.IndexNumber == nil || ep.ParentIndexNumber == nil {
		t.Errorf("episode numbers = %+v", ep)
	}

	// Pagination: TotalRecordCount is the pre-pagination total.
	if code := get(t, h, "/emby/Items?ParentId="+itoa(seasonID)+"&StartIndex=18&Limit=5", &page); code != 200 {
		t.Fatalf("paged items = %d", code)
	}
	if page.TotalRecordCount != 20 || len(page.Items) != 2 || page.StartIndex != 18 {
		t.Fatalf("paged items = %+v", page)
	}

	// Ids lookup.
	if code := get(t, h, "/emby/Items?Ids="+itoa(movieID), &page); code != 200 {
		t.Fatalf("ids items = %d", code)
	}
	if page.TotalRecordCount != 1 || page.Items[0].Id != itoa(movieID) {
		t.Fatalf("ids items = %+v", page)
	}

	// Recursive + type filter flattens the series to its episodes.
	if code := get(t, h, "/emby/Items?ParentId="+itoa(seriesID)+"&Recursive=true&IncludeItemTypes=Episode", &page); code != 200 {
		t.Fatalf("recursive items = %d", code)
	}
	if page.TotalRecordCount != 20 {
		t.Fatalf("recursive episodes total = %d, want 20", page.TotalRecordCount)
	}
	for _, it := range page.Items {
		if it.Type != "Episode" {
			t.Fatalf("recursive item type = %q", it.Type)
		}
	}

	// Single-item detail through both route shapes.
	var one embyItemDTO
	if code := get(t, h, "/emby/Items/"+itoa(seriesID), &one); code != 200 {
		t.Fatalf("item detail = %d", code)
	}
	if one.Type != "Series" || one.ChildCount == nil || *one.ChildCount != 1 {
		t.Errorf("series detail = %+v", one)
	}
	if code := get(t, h, "/emby/Users/u1/Items/"+itoa(movieID), &one); code != 200 || one.Type != "Movie" {
		t.Fatalf("user item detail = %d %+v", code, one)
	}
}

type pbInfo struct {
	MediaSources []struct {
		Id                   string `json:"Id"`
		Name                 string `json:"Name"`
		Path                 string `json:"Path"`
		Protocol             string `json:"Protocol"`
		Container            string `json:"Container"`
		SupportsDirectPlay   bool   `json:"SupportsDirectPlay"`
		SupportsDirectStream bool   `json:"SupportsDirectStream"`
		SupportsTranscoding  bool   `json:"SupportsTranscoding"`
		MediaStreams         []struct {
			Type   string `json:"Type"`
			Codec  string `json:"Codec"`
			Width  int    `json:"Width"`
			Height int    `json:"Height"`
		} `json:"MediaStreams"`
	} `json:"MediaSources"`
	PlaySessionId string `json:"PlaySessionId"`
}

// strmTargetOf reads the first line of the .strm file at path.
func strmTargetOf(t *testing.T, strmPath string) string {
	t.Helper()
	raw, err := os.ReadFile(strmPath)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
}

func TestEmbyPlaybackInfo(t *testing.T) {
	h := testHandler(t)
	_, _, movieID, seriesID, _, _ := embyFixture(t, h)

	// The movie's expected Path is the exact content of its .strm file.
	var detail itemDTO
	if code := get(t, h, "/api/items/"+itoa(movieID), &detail); code != 200 || len(detail.MediaSources) != 1 {
		t.Fatalf("movie detail = %d %+v", code, detail)
	}
	wantPath := strmTargetOf(t, detail.MediaSources[0].Path)

	for _, method := range []string{http.MethodPost, http.MethodGet} {
		code, body := doReq(t, h, method, "/emby/Items/"+itoa(movieID)+"/PlaybackInfo", "")
		if code != 200 {
			t.Fatalf("%s PlaybackInfo = %d %s", method, code, body)
		}
		var info pbInfo
		if err := json.Unmarshal(body, &info); err != nil {
			t.Fatalf("decode PlaybackInfo: %v", err)
		}
		if len(info.MediaSources) != 1 {
			t.Fatalf("media sources = %+v", info)
		}
		ms := info.MediaSources[0]
		if ms.Path != wantPath {
			t.Errorf("Path = %q, want strm content %q", ms.Path, wantPath)
		}
		if ms.Id != itoa(movieID) || ms.Protocol != "Http" || !ms.SupportsDirectPlay || ms.SupportsTranscoding {
			t.Errorf("media source = %+v", ms)
		}
		if info.PlaySessionId != "yk-"+itoa(movieID) {
			t.Errorf("PlaySessionId = %q", info.PlaySessionId)
		}
		if len(ms.MediaStreams) == 0 || ms.MediaStreams[0].Type != "Video" || ms.MediaStreams[0].Width != 3840 {
			t.Errorf("media streams = %+v", ms.MediaStreams)
		}
	}

	// An episode reports its own STRM.
	var sdetail itemDTO
	if code := get(t, h, "/api/items/"+itoa(seriesID), &sdetail); code != 200 {
		t.Fatalf("series detail = %d", code)
	}
	ep := sdetail.Seasons[0].Episodes[0]
	if len(ep.MediaSources) != 1 {
		t.Fatalf("episode sources = %+v", ep)
	}
	wantEpPath := strmTargetOf(t, ep.MediaSources[0].Path)
	code, body := doReq(t, h, http.MethodPost, "/Items/"+itoa(ep.ID)+"/PlaybackInfo", "")
	if code != 200 {
		t.Fatalf("episode PlaybackInfo = %d %s", code, body)
	}
	var info pbInfo
	if err := json.Unmarshal(body, &info); err != nil {
		t.Fatal(err)
	}
	if len(info.MediaSources) != 1 || info.MediaSources[0].Path != wantEpPath {
		t.Errorf("episode PlaybackInfo = %+v, want path %q", info, wantEpPath)
	}
}

func TestEmbyImagesAndVideos(t *testing.T) {
	h := testHandler(t)
	_, _, movieID, _, _, _ := embyFixture(t, h)

	// Primary image of the fixture movie (it owns a poster.jpg).
	if code := get(t, h, "/emby/Items/"+itoa(movieID)+"/Images/Primary", nil); code != 200 {
		t.Errorf("Images/Primary = %d, want 200", code)
	}
	if code := get(t, h, "/Items/"+itoa(movieID)+"/Images/Backdrop/0?maxWidth=300", nil); code != 200 {
		t.Errorf("Images/Backdrop/0 = %d, want 200", code)
	}
	// Unknown image type: JSON 404.
	code, body := doReq(t, h, http.MethodGet, "/emby/Items/"+itoa(movieID)+"/Images/Logo", "")
	if code != 404 || !strings.HasPrefix(string(body), "{") {
		t.Errorf("Images/Logo = %d %s, want JSON 404", code, body)
	}

	// Videos stream: both the bare and extension forms 302 to the STRM.
	var detail itemDTO
	if code := get(t, h, "/api/items/"+itoa(movieID), &detail); code != 200 {
		t.Fatalf("movie detail = %d", code)
	}
	want := strmTargetOf(t, detail.MediaSources[0].Path)
	for _, p := range []string{"/Videos/" + itoa(movieID) + "/stream", "/emby/Videos/" + itoa(movieID) + "/stream.mkv"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound {
			t.Errorf("GET %s = %d, want 302", p, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != want {
			t.Errorf("GET %s Location = %q, want %q", p, loc, want)
		}
	}
}

func TestEmbySessionsReporting(t *testing.T) {
	h := testHandler(t)
	_, _, movieID, _, _, _ := embyFixture(t, h)

	// Progress with a numeric ItemId (flex id form).
	pos := int64(600 * 10_000_000)
	code, _ := doReq(t, h, http.MethodPost, "/emby/Sessions/Playing/Progress",
		`{"ItemId":`+itoa(movieID)+`,"PositionTicks":`+itoa(pos)+`}`)
	if code != 204 {
		t.Fatalf("Sessions/Progress = %d, want 204", code)
	}
	var detail itemDTO
	if code := get(t, h, "/api/items/"+itoa(movieID), &detail); code != 200 {
		t.Fatalf("detail = %d", code)
	}
	if detail.UserData == nil || detail.UserData.PlaybackPositionTicks != pos {
		t.Fatalf("user data after progress = %+v", detail.UserData)
	}
	// The Emby DTO carries the same state.
	var ed embyItemDTO
	if code := get(t, h, "/emby/Items/"+itoa(movieID), &ed); code != 200 {
		t.Fatalf("emby detail = %d", code)
	}
	if ed.UserData == nil || ed.UserData.PlaybackPositionTicks != pos || ed.UserData.Played {
		t.Fatalf("emby user data = %+v", ed.UserData)
	}

	// Stopped near the end (string ItemId form) marks it played.
	if ed.RunTimeTicks <= 0 {
		t.Fatalf("fixture movie has no runtime: %+v", ed)
	}
	end := ed.RunTimeTicks * 97 / 100
	code, _ = doReq(t, h, http.MethodPost, "/Sessions/Playing/Stopped",
		`{"ItemId":"`+itoa(movieID)+`","PositionTicks":`+itoa(end)+`}`)
	if code != 204 {
		t.Fatalf("Sessions/Stopped = %d, want 204", code)
	}
	if code := get(t, h, "/api/items/"+itoa(movieID), &detail); code != 200 {
		t.Fatalf("detail after stop = %d", code)
	}
	if !detail.UserData.Played {
		t.Errorf("user data after stop = %+v, want Played", detail.UserData)
	}

	// Unknown item / garbage body still answer 204.
	if code, _ := doReq(t, h, http.MethodPost, "/emby/Sessions/Playing", `{"ItemId":"424242","PositionTicks":1}`); code != 204 {
		t.Errorf("Sessions/Playing unknown = %d, want 204", code)
	}
	if code, _ := doReq(t, h, http.MethodPost, "/emby/Sessions/Playing/Progress", `not json`); code != 204 {
		t.Errorf("Sessions/Progress garbage = %d, want 204", code)
	}
}

func TestAuthenticateByName(t *testing.T) {
	h, st := userTestHandler(t)
	created, err := st.CreateUser("nanuser", "s3cret-pw", false)
	if err != nil {
		t.Fatal(err)
	}

	// Wrong password, unknown user and empty body are all rejected:
	// batch 12 made this a real credential check.
	for _, body := range []string{
		`{"Username":"nanuser","Pw":"wrong"}`,
		`{"Username":"ghost","Pw":"s3cret-pw"}`,
		``,
	} {
		if code, respBody := doReq(t, h, http.MethodPost, "/emby/Users/AuthenticateByName", body); code != 401 {
			t.Errorf("AuthenticateByName %q = %d %s, want 401", body, code, respBody)
		}
	}

	code, body := doReq(t, h, http.MethodPost, "/emby/Users/AuthenticateByName",
		`{"Username":"nanuser","Pw":"s3cret-pw"}`)
	if code != 200 {
		t.Fatalf("AuthenticateByName = %d %s", code, body)
	}
	var resp struct {
		User struct {
			Id                    string `json:"Id"`
			Name                  string `json:"Name"`
			HasPassword           bool   `json:"HasPassword"`
			HasConfiguredPassword bool   `json:"HasConfiguredPassword"`
		} `json:"User"`
		AccessToken string `json:"AccessToken"`
		SessionInfo struct {
			Id     string `json:"Id"`
			UserId string `json:"UserId"`
		} `json:"SessionInfo"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.User.Name != "nanuser" || resp.User.Id != created.ID {
		t.Errorf("user = %+v, want id %q", resp.User, created.ID)
	}
	if !resp.User.HasPassword || !resp.User.HasConfiguredPassword {
		t.Errorf("user password flags = %+v, want both true", resp.User)
	}
	// The access token is a per-user token, NOT the server API key,
	// and it resolves back to this user.
	if resp.AccessToken == "" || resp.AccessToken == fetchKey(t, h) {
		t.Errorf("AccessToken = %q, want a fresh per-user token", resp.AccessToken)
	}
	if resp.SessionInfo.UserId != resp.User.Id || resp.SessionInfo.Id != "yk-session" {
		t.Errorf("session = %+v", resp.SessionInfo)
	}
	code, body = reqWith(t, h, http.MethodGet, "/api/auth/me",
		map[string]string{"X-Emby-Token": resp.AccessToken})
	if code != 200 || !strings.Contains(string(body), `"name":"nanuser"`) {
		t.Errorf("me with login token = %d %s", code, body)
	}
	// The root-level alias enforces the same check.
	if code, _ := doReq(t, h, http.MethodPost, "/Users/AuthenticateByName",
		`{"Username":"nanuser","Pw":"wrong"}`); code != 401 {
		t.Errorf("alias wrong-password = %d, want 401", code)
	}
}

func TestEmbyPublicAndUnknownPaths(t *testing.T) {
	h := testHandler(t)

	// System/Info/Public mirrors System/Info.
	var pub, full systemInfoDTO
	if code := get(t, h, "/emby/System/Info/Public", &pub); code != 200 {
		t.Fatalf("Info/Public = %d", code)
	}
	if code := get(t, h, "/emby/System/Info", &full); code != 200 || pub.Id != full.Id {
		t.Fatalf("Info/Public = %+v vs Info = %+v", pub, full)
	}

	// Users/Public is an empty JSON array.
	code, body := reqWith(t, h, http.MethodGet, "/emby/Users/Public", nil)
	if code != 200 || strings.TrimSpace(string(body)) != "[]" {
		t.Errorf("Users/Public = %d %s, want []", code, body)
	}

	// Unknown Emby sub-paths answer JSON 404, never the SPA page — the
	// HTML fallthrough is what produced Nan's `invalid character '<'`.
	for _, p := range []string{"/emby/Library/Whatever", "/Items/999999999", "/emby/Items/999999999"} {
		code, body := reqWith(t, h, http.MethodGet, p, nil)
		if code != 404 {
			t.Errorf("GET %s = %d, want 404", p, code)
		}
		var v map[string]any
		if err := json.Unmarshal(body, &v); err != nil {
			t.Errorf("GET %s body is not JSON: %q", p, body)
		}
		if strings.Contains(string(body), "<html") {
			t.Errorf("GET %s returned the SPA page", p)
		}
	}
}

func TestRequestLogRedaction(t *testing.T) {
	h := testHandler(t)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	if code := get(t, h, "/api/health?api_key=TOPSECRET-XYZ-123", nil); code != 200 {
		t.Fatalf("health = %d", code)
	}
	out := buf.String()
	if strings.Contains(out, "TOPSECRET-XYZ-123") {
		t.Fatalf("log leaks the api key: %q", out)
	}
	if !strings.Contains(out, "api_key=***") {
		t.Errorf("log line lacks the redacted key: %q", out)
	}
	if !strings.Contains(out, "GET /api/health") || !strings.Contains(out, "200") {
		t.Errorf("log line shape = %q", out)
	}
}
