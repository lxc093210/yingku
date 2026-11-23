package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureIDs resolves the scanned fixture movie/series ids via the API.
func fixtureIDs(t *testing.T, h http.Handler) (movieID, seriesID int64) {
	t.Helper()
	var libs []libraryDTO
	if code := get(t, h, "/api/libraries", &libs); code != 200 || len(libs) != 2 {
		t.Fatalf("libraries = %d %+v", code, libs)
	}
	for _, l := range libs {
		var page struct {
			Items []itemDTO `json:"Items"`
		}
		if code := get(t, h, "/api/libraries/"+itoa(l.ID)+"/items", &page); code != 200 || len(page.Items) != 1 {
			t.Fatalf("items of %s = %d %+v", l.Name, code, page)
		}
		if l.Kind == "movie" {
			movieID = page.Items[0].ID
		} else {
			seriesID = page.Items[0].ID
		}
	}
	return movieID, seriesID
}

type playInfoDTO struct {
	ItemID        int64  `json:"ItemId"`
	Name          string `json:"Name"`
	SeriesName    string `json:"SeriesName"`
	PositionTicks int64  `json:"PositionTicks"`
}

func TestPlaybackFlow(t *testing.T) {
	h := testHandler(t)
	movieID, seriesID := fixtureIDs(t, h)

	// Series detail: cast comes from tvshow.nfo.
	var seriesDetail itemDTO
	if code := get(t, h, "/api/items/"+itoa(seriesID), &seriesDetail); code != 200 {
		t.Fatalf("series detail = %d", code)
	}
	if len(seriesDetail.People) == 0 {
		t.Errorf("series detail has no People from tvshow.nfo")
	}
	if len(seriesDetail.Seasons) != 1 || len(seriesDetail.Seasons[0].Episodes) == 0 {
		t.Fatalf("series detail seasons = %+v", seriesDetail.Seasons)
	}
	firstEp := seriesDetail.Seasons[0].Episodes[0]
	if firstEp.IndexNumber == nil || *firstEp.IndexNumber != 1 {
		t.Fatalf("first episode IndexNumber = %v, want 1", firstEp.IndexNumber)
	}

	// play-info: a series resolves to its first playable episode.
	var info playInfoDTO
	if code := get(t, h, "/api/items/"+itoa(seriesID)+"/play-info", &info); code != 200 {
		t.Fatalf("series play-info = %d", code)
	}
	if info.ItemID != firstEp.ID {
		t.Errorf("series play-info ItemId = %d, want first episode %d", info.ItemID, firstEp.ID)
	}
	if info.SeriesName != "上海女子图鉴" {
		t.Errorf("play-info SeriesName = %q", info.SeriesName)
	}
	// play-info: a movie resolves to itself, no progress yet.
	if code := get(t, h, "/api/items/"+itoa(movieID)+"/play-info", &info); code != 200 {
		t.Fatalf("movie play-info = %d", code)
	}
	if info.ItemID != movieID || info.PositionTicks != 0 {
		t.Errorf("movie play-info = %+v", info)
	}

	// Movie detail: cast from the movie NFO, capped at 30.
	var movieDetail itemDTO
	if code := get(t, h, "/api/items/"+itoa(movieID), &movieDetail); code != 200 {
		t.Fatalf("movie detail = %d", code)
	}
	if len(movieDetail.People) != 30 {
		t.Fatalf("movie People = %d, want 30 (NFO cap)", len(movieDetail.People))
	}
	if movieDetail.People[0].Name != "沙梅克·摩尔" {
		t.Errorf("first cast member = %q", movieDetail.People[0].Name)
	}
	if movieDetail.UserData == nil || movieDetail.UserData.Favorite || movieDetail.UserData.Played {
		t.Errorf("initial user data = %+v", movieDetail.UserData)
	}

	// stream: 302 to the exact first line of the fixture .strm file.
	matches, _ := filepath.Glob(filepath.Join(fixturesAbs(t), "电影", "动画电影", "*", "*.strm"))
	if len(matches) != 1 {
		t.Fatalf("fixture strms = %v", matches)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	wantLocation := strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
	req := httptest.NewRequest(http.MethodGet, "/api/items/"+itoa(movieID)+"/stream", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("stream code = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != wantLocation {
		t.Errorf("stream Location = %q, want %q", loc, wantLocation)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("stream Cache-Control = %q", cc)
	}
	// A series is not directly streamable.
	if code := get(t, h, "/api/items/"+itoa(seriesID)+"/stream", nil); code != http.StatusBadRequest {
		t.Errorf("series stream = %d, want 400", code)
	}

	// Progress at ~17%: home ContinueWatching picks the movie up.
	if code, body := doReq(t, h, http.MethodPost, "/api/items/"+itoa(movieID)+"/progress",
		`{"positionSeconds":600,"durationSeconds":3600}`); code != 200 {
		t.Fatalf("progress = %d %s", code, body)
	}
	var home struct {
		ContinueWatching []itemDTO `json:"ContinueWatching"`
	}
	if code := get(t, h, "/api/home", &home); code != 200 {
		t.Fatalf("home = %d", code)
	}
	if len(home.ContinueWatching) != 1 || home.ContinueWatching[0].ID != movieID {
		t.Fatalf("continue watching = %+v", home.ContinueWatching)
	}
	if got := home.ContinueWatching[0].PlaybackPositionTicks; got != 600*10_000_000 {
		t.Errorf("continue ticks = %d, want %d", got, 600*10_000_000)
	}
	// play-info now reports the resume position.
	if code := get(t, h, "/api/items/"+itoa(movieID)+"/play-info", &info); code != 200 || info.PositionTicks != 600*10_000_000 {
		t.Errorf("resume play-info = %d %+v", code, info)
	}

	// Progress at 97% marks it played: it leaves ContinueWatching.
	if code, body := doReq(t, h, http.MethodPost, "/api/items/"+itoa(movieID)+"/progress",
		`{"positionSeconds":3500,"durationSeconds":3600}`); code != 200 {
		t.Fatalf("progress finish = %d %s", code, body)
	}
	if code := get(t, h, "/api/home", &home); code != 200 {
		t.Fatalf("home after finish = %d", code)
	}
	if len(home.ContinueWatching) != 0 {
		t.Errorf("continue watching after played = %+v", home.ContinueWatching)
	}
	if code := get(t, h, "/api/items/"+itoa(movieID), &movieDetail); code != 200 {
		t.Fatalf("detail after finish = %d", code)
	}
	if movieDetail.UserData == nil || !movieDetail.UserData.Played {
		t.Errorf("user data after finish = %+v", movieDetail.UserData)
	}
	// Played items resume from zero.
	if code := get(t, h, "/api/items/"+itoa(movieID)+"/play-info", &info); code != 200 || info.PositionTicks != 0 {
		t.Errorf("played play-info = %d %+v", code, info)
	}

	// Reset clears progress/played.
	if code, body := doReq(t, h, http.MethodPost, "/api/items/"+itoa(movieID)+"/progress/reset", ""); code != 200 {
		t.Fatalf("progress reset = %d %s", code, body)
	}
	if code := get(t, h, "/api/items/"+itoa(movieID), &movieDetail); code != 200 {
		t.Fatalf("detail after reset = %d", code)
	}
	if movieDetail.UserData.Played || movieDetail.UserData.PlaybackPositionTicks != 0 {
		t.Errorf("user data after reset = %+v", movieDetail.UserData)
	}

	// Favorite toggles through the detail DTO.
	if code, body := doReq(t, h, http.MethodPost, "/api/items/"+itoa(movieID)+"/favorite",
		`{"favorite":true}`); code != 200 {
		t.Fatalf("favorite = %d %s", code, body)
	}
	if code := get(t, h, "/api/items/"+itoa(movieID), &movieDetail); code != 200 {
		t.Fatalf("detail after favorite = %d", code)
	}
	if !movieDetail.UserData.Favorite {
		t.Errorf("user data after favorite = %+v", movieDetail.UserData)
	}
}

func TestSettingsEndpoints(t *testing.T) {
	h := bareHandler(t)

	var out struct {
		TMDBKeySet bool `json:"tmdbKeySet"`
	}
	if code := get(t, h, "/api/settings", &out); code != 200 || out.TMDBKeySet {
		t.Fatalf("initial settings = %d %+v", code, out)
	}
	code, body := doReq(t, h, http.MethodPut, "/api/settings", `{"tmdbApiKey":"TESTKEY-SECRET-123"}`)
	if code != 200 {
		t.Fatalf("put settings = %d %s", code, body)
	}
	if strings.Contains(string(body), "TESTKEY-SECRET-123") {
		t.Errorf("settings response leaks the key: %s", body)
	}
	var saved struct {
		TMDBKeySet bool `json:"tmdbKeySet"`
	}
	if err := json.Unmarshal(body, &saved); err != nil || !saved.TMDBKeySet {
		t.Fatalf("put settings body = %s err %v", body, err)
	}
	if code := get(t, h, "/api/settings", &out); code != 200 || !out.TMDBKeySet {
		t.Fatalf("settings after put = %d %+v", code, out)
	}
	// Empty value clears.
	if code, _ := doReq(t, h, http.MethodPut, "/api/settings", `{"tmdbApiKey":""}`); code != 200 {
		t.Fatalf("clear settings = %d", code)
	}
	if code := get(t, h, "/api/settings", &out); code != 200 || out.TMDBKeySet {
		t.Fatalf("settings after clear = %d %+v", code, out)
	}
}

func TestSettingsBaseURL(t *testing.T) {
	h := bareHandler(t)

	var out settingsDTO
	if code := get(t, h, "/api/settings", &out); code != 200 {
		t.Fatalf("settings = %d", code)
	}
	if out.TMDBBaseURL != defaultTMDBBase {
		t.Fatalf("default base = %q, want %q", out.TMDBBaseURL, defaultTMDBBase)
	}
	// Point at the user's key-injecting proxy; key stays unset and a
	// key-only PUT afterwards must not disturb the base.
	if code, body := doReq(t, h, http.MethodPut, "/api/settings",
		`{"tmdbBaseUrl":"https://tmdb.example.com/"}`); code != 200 {
		t.Fatalf("put base = %d %s", code, body)
	}
	if code := get(t, h, "/api/settings", &out); code != 200 || out.TMDBBaseURL != "https://tmdb.example.com" {
		t.Fatalf("base after put = %d %+v", code, out)
	}
	if out.TMDBKeySet {
		t.Errorf("key unexpectedly set")
	}
	if code, _ := doReq(t, h, http.MethodPut, "/api/settings", `{"tmdbApiKey":"K"}`); code != 200 {
		t.Fatalf("put key = %d", code)
	}
	if code := get(t, h, "/api/settings", &out); code != 200 || out.TMDBBaseURL != "https://tmdb.example.com" || !out.TMDBKeySet {
		t.Fatalf("settings after key put = %d %+v (base must survive)", code, out)
	}
	// Clearing the base restores the official default.
	if code, _ := doReq(t, h, http.MethodPut, "/api/settings", `{"tmdbBaseUrl":""}`); code != 200 {
		t.Fatalf("clear base = %d", code)
	}
	if code := get(t, h, "/api/settings", &out); code != 200 || out.TMDBBaseURL != defaultTMDBBase {
		t.Fatalf("base after clear = %d %+v", code, out)
	}
}
