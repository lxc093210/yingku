package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"

	"yingku/internal/scan"
	"yingku/internal/store"
)

func testHandler(t *testing.T) http.Handler {
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
	return New(st, sc, root)
}

func get(t *testing.T, h http.Handler, path string, out any) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if out != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("GET %s: decode: %v", path, err)
		}
	}
	return rec.Code
}

func TestAPI(t *testing.T) {
	h := testHandler(t)

	var health map[string]bool
	if code := get(t, h, "/api/health", &health); code != 200 || !health["ok"] {
		t.Fatalf("health = %d %v", code, health)
	}

	var libs []libraryDTO
	if code := get(t, h, "/api/libraries", &libs); code != 200 || len(libs) != 2 {
		t.Fatalf("libraries = %d %+v", code, libs)
	}
	var movieLib, seriesLib libraryDTO
	for _, l := range libs {
		if l.Kind == "movie" {
			movieLib = l
		} else {
			seriesLib = l
		}
	}

	var page struct {
		Items            []itemDTO `json:"Items"`
		NextCursor       string    `json:"NextCursor"`
		TotalRecordCount int       `json:"TotalRecordCount"`
	}
	if code := get(t, h, "/api/libraries/"+itoa(movieLib.ID)+"/items", &page); code != 200 {
		t.Fatalf("movie items code = %d", code)
	}
	if len(page.Items) != 1 || page.Items[0].Name != "蜘蛛侠：平行宇宙" {
		t.Fatalf("movie page = %+v", page.Items)
	}
	if page.Items[0].Type != "Movie" || page.Items[0].ProviderIds["Tmdb"] != "324857" {
		t.Errorf("movie dto = %+v", page.Items[0])
	}

	// Keyset pagination over the series library (1 series, limit 1).
	var p1 struct {
		Items      []itemDTO `json:"Items"`
		NextCursor string    `json:"NextCursor"`
	}
	if code := get(t, h, "/api/libraries/"+itoa(seriesLib.ID)+"/items?limit=1", &p1); code != 200 || len(p1.Items) != 1 {
		t.Fatalf("series page = %d %+v", code, p1)
	}

	// Series detail with seasons/episodes.
	var detail itemDTO
	if code := get(t, h, "/api/items/"+itoa(p1.Items[0].ID), &detail); code != 200 {
		t.Fatalf("detail code = %d", code)
	}
	if detail.Type != "Series" || len(detail.Seasons) != 1 {
		t.Fatalf("detail = %+v", detail)
	}
	eps := detail.Seasons[0].Episodes
	if len(eps) != 20 {
		t.Fatalf("episodes in detail = %d, want 20", len(eps))
	}
	if eps[8].IndexNumber == nil || *eps[8].IndexNumber != 9 {
		t.Errorf("episode 9 IndexNumber = %v", eps[8].IndexNumber)
	}
	if len(eps[8].MediaSources) != 1 || eps[8].MediaSources[0].Width != 3840 {
		t.Errorf("episode 9 media source = %+v", eps[8].MediaSources)
	}

	// Movie detail: media sources + subtitles.
	var mdetail itemDTO
	if code := get(t, h, "/api/items/"+itoa(page.Items[0].ID), &mdetail); code != 200 {
		t.Fatalf("movie detail code = %d", code)
	}
	if len(mdetail.MediaSources) != 1 || mdetail.MediaSources[0].HDR != "DolbyVision" {
		t.Errorf("movie detail sources = %+v", mdetail.MediaSources)
	}
	if len(mdetail.Subtitles) != 3 {
		t.Errorf("movie subtitles = %d, want 3", len(mdetail.Subtitles))
	}

	// Image serving + 404 for a missing kind on an item without images.
	req := httptest.NewRequest(http.MethodGet, "/api/items/"+itoa(page.Items[0].ID)+"/image/poster", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("Content-Type") == "" {
		t.Errorf("poster = %d", rec.Code)
	}
	if code := get(t, h, "/api/items/"+itoa(p1.Items[0].ID)+"/image/poster", nil); code != 404 {
		t.Errorf("series poster code = %d, want 404", code)
	}

	// Search.
	var search struct {
		Items []itemDTO `json:"Items"`
	}
	if code := get(t, h, "/api/search?q="+queryEscape("蜘蛛侠"), &search); code != 200 || len(search.Items) == 0 {
		t.Fatalf("search = %d %+v", code, search)
	}
	found := false
	for _, it := range search.Items {
		if it.Name == "蜘蛛侠：平行宇宙" {
			found = true
		}
	}
	if !found {
		t.Errorf("search did not return the movie: %+v", search.Items)
	}

	// Stats.
	var stats struct {
		TotalItems       int            `json:"TotalItems"`
		TotalEpisodes    int            `json:"TotalEpisodes"`
		MediaInfoSources map[string]int `json:"MediaInfoSources"`
	}
	if code := get(t, h, "/api/stats", &stats); code != 200 {
		t.Fatalf("stats code = %d", code)
	}
	if stats.TotalItems != 23 || stats.TotalEpisodes != 20 || stats.MediaInfoSources["json"] != 21 {
		t.Errorf("stats = %+v", stats)
	}

	// Scan status endpoint.
	var status scan.Status
	if code := get(t, h, "/api/scan/status", &status); code != 200 {
		t.Errorf("scan status code = %d", code)
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

func queryEscape(s string) string {
	return url.QueryEscape(s)
}

func TestFavoritesEndpointAndHomeThumbs(t *testing.T) {
	h := testHandler(t)
	movieID, seriesID := fixtureIDs(t, h)

	// Favorites start empty.
	var favs struct {
		Items []itemDTO `json:"Items"`
	}
	if code := get(t, h, "/api/favorites", &favs); code != 200 || len(favs.Items) != 0 {
		t.Fatalf("favorites empty = %d %+v", code, favs)
	}
	if code, body := doReq(t, h, http.MethodPost, "/api/items/"+itoa(movieID)+"/favorite", `{"favorite":true}`); code != 200 {
		t.Fatalf("favorite post = %d %s", code, body)
	}
	if code, body := doReq(t, h, http.MethodPost, "/api/items/"+itoa(seriesID)+"/favorite", `{"favorite":true}`); code != 200 {
		t.Fatalf("favorite series post = %d %s", code, body)
	}
	if code := get(t, h, "/api/favorites", &favs); code != 200 || len(favs.Items) != 2 {
		t.Fatalf("favorites = %d %+v", code, favs)
	}

	// Home: library cards carry Thumbs. The movie owns the fixture's only
	// backdrop art; the series fixture folder has none.
	var home struct {
		Libraries        []libraryDTO `json:"Libraries"`
		ContinueWatching []itemDTO    `json:"ContinueWatching"`
	}
	if code := get(t, h, "/api/home", &home); code != 200 {
		t.Fatalf("home = %d", code)
	}
	var movieLibID int64
	for _, l := range home.Libraries {
		if l.Kind == "movie" {
			movieLibID = l.ID
			if len(l.Thumbs) != 1 || l.Thumbs[0] != movieID {
				t.Errorf("movie thumbs = %v, want [%d]", l.Thumbs, movieID)
			}
		} else if len(l.Thumbs) != 0 {
			t.Errorf("series thumbs = %v, want none (fixture has no backdrop)", l.Thumbs)
		}
	}

	// sort=added serves the library page.
	var page struct {
		Items []itemDTO `json:"Items"`
	}
	if code := get(t, h, "/api/libraries/"+itoa(movieLibID)+"/items?sort=added", &page); code != 200 ||
		len(page.Items) != 1 || page.Items[0].ID != movieID {
		t.Fatalf("sort=added page = %d %+v", code, page)
	}

	// ContinueWatching episode entries carry season/episode/name fields.
	var info playInfoDTO
	if code := get(t, h, "/api/items/"+itoa(seriesID)+"/play-info", &info); code != 200 || info.ItemID == 0 {
		t.Fatalf("play-info = %d %+v", code, info)
	}
	if code, body := doReq(t, h, http.MethodPost, "/api/items/"+itoa(info.ItemID)+"/progress",
		`{"positionSeconds":600,"durationSeconds":3600}`); code != 200 {
		t.Fatalf("episode progress = %d %s", code, body)
	}
	if code := get(t, h, "/api/home", &home); code != 200 {
		t.Fatalf("home after progress = %d", code)
	}
	if len(home.ContinueWatching) != 1 {
		t.Fatalf("continue watching = %+v", home.ContinueWatching)
	}
	cw := home.ContinueWatching[0]
	if cw.SeriesName != "上海女子图鉴" || cw.EpisodeName == "" {
		t.Errorf("cw series/episode name = %q/%q", cw.SeriesName, cw.EpisodeName)
	}
	if cw.SeasonNumber == nil || *cw.SeasonNumber != 1 || cw.EpisodeNumber == nil || *cw.EpisodeNumber != 1 {
		t.Errorf("cw S/E = %v/%v, want 1/1", cw.SeasonNumber, cw.EpisodeNumber)
	}
}
