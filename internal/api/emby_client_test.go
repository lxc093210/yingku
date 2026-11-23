package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// ---- Emby client mount surface (batch 11) ----

func episodeByNumber(t *testing.T, h http.Handler, seasonID int64, n int) embyItemDTO {
	t.Helper()
	var page embyPage
	if code := get(t, h, "/emby/Items?ParentId="+itoa(seasonID), &page); code != 200 {
		t.Fatalf("season items = %d", code)
	}
	for _, it := range page.Items {
		if it.IndexNumber != nil && *it.IndexNumber == n {
			return it
		}
	}
	t.Fatalf("episode %d not found in season %d", n, seasonID)
	return embyItemDTO{}
}

func TestEmbyViews(t *testing.T) {
	h := testHandler(t)

	var folders embyPage
	if code := get(t, h, "/emby/Library/MediaFolders", &folders); code != 200 {
		t.Fatalf("MediaFolders = %d", code)
	}
	for _, p := range []string{"/emby/Users/some-user/Views", "/Users/some-user/Views"} {
		var views embyPage
		if code := get(t, h, p, &views); code != 200 {
			t.Fatalf("GET %s = %d", p, code)
		}
		if views.TotalRecordCount != folders.TotalRecordCount || len(views.Items) != len(folders.Items) {
			t.Fatalf("Views = %+v, MediaFolders = %+v", views, folders)
		}
		for i := range views.Items {
			if views.Items[i].Id != folders.Items[i].Id || views.Items[i].Type != "CollectionFolder" {
				t.Errorf("Views[%d] = %+v, want folder %+v", i, views.Items[i], folders.Items[i])
			}
		}
	}
}

func TestEmbyLatest(t *testing.T) {
	h := testHandler(t)
	movieLibID, seriesLibID, movieID, seriesID, _, _ := embyFixture(t, h)

	// No ParentId: a BARE array (not a wrapped query result) of the
	// top-level items.
	code, body := doReq(t, h, http.MethodGet, "/emby/Items/Latest", "")
	if code != 200 {
		t.Fatalf("Latest = %d %s", code, body)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		t.Fatalf("Latest body is not a bare array: %s", body)
	}
	var latest []embyItemDTO
	if err := json.Unmarshal(body, &latest); err != nil {
		t.Fatal(err)
	}
	if len(latest) != 2 {
		t.Fatalf("Latest = %+v, want the 2 top-level items", latest)
	}
	ids := map[string]bool{}
	for _, it := range latest {
		ids[it.Id] = true
		if it.Type == "Episode" || it.Type == "Season" {
			t.Errorf("Latest leaked a child item: %+v", it)
		}
	}
	if !ids[itoa(movieID)] || !ids[itoa(seriesID)] {
		t.Errorf("Latest ids = %v", ids)
	}

	// Library scope: only that library's top-level items.
	var libLatest []embyItemDTO
	if code := get(t, h, "/emby/Users/u/Items/Latest?ParentId="+seriesLibID, &libLatest); code != 200 {
		t.Fatalf("Latest by series library = %d", code)
	}
	if len(libLatest) != 1 || libLatest[0].Id != itoa(seriesID) {
		t.Fatalf("Latest by series library = %+v", libLatest)
	}
	if code := get(t, h, "/Items/Latest?ParentId="+movieLibID, &libLatest); code != 200 {
		t.Fatalf("Latest by movie library = %d", code)
	}
	if len(libLatest) != 1 || libLatest[0].Id != itoa(movieID) || libLatest[0].Type != "Movie" {
		t.Fatalf("Latest by movie library = %+v", libLatest)
	}

	// Series scope: its episodes, newest first.
	var eps []embyItemDTO
	if code := get(t, h, "/emby/Items/Latest?ParentId="+itoa(seriesID), &eps); code != 200 {
		t.Fatalf("Latest by series = %d", code)
	}
	if len(eps) != 20 {
		t.Fatalf("Latest by series = %d episodes, want 20", len(eps))
	}
	for _, ep := range eps {
		if ep.Type != "Episode" {
			t.Fatalf("Latest by series leaked %+v", ep)
		}
	}

	// Limit applies.
	var one []embyItemDTO
	if code := get(t, h, "/emby/Items/Latest?Limit=1", &one); code != 200 || len(one) != 1 {
		t.Fatalf("Latest Limit=1 = %d %+v", code, one)
	}
}

func TestEmbyResume(t *testing.T) {
	h := testHandler(t)
	movieLibID, seriesLibID, movieID, _, seasonID, _ := embyFixture(t, h)
	ep := episodeByNumber(t, h, seasonID, 1)
	epNumID := ep.Id

	var page embyPage
	if code := get(t, h, "/emby/Items/Resume", &page); code != 200 || page.TotalRecordCount != 0 || len(page.Items) != 0 {
		t.Fatalf("empty Resume = %d %+v", code, page)
	}

	// Start the movie and the first episode (small positions: neither
	// is anywhere near played).
	pos := int64(300 * 10_000_000)
	for _, id := range []string{itoa(movieID), epNumID} {
		code, _ := doReq(t, h, http.MethodPost, "/emby/Sessions/Playing/Progress",
			`{"ItemId":`+id+`,"PositionTicks":`+itoa(pos)+`}`)
		if code != 204 {
			t.Fatalf("progress for %s = %d", id, code)
		}
	}

	if code := get(t, h, "/emby/Items/Resume", &page); code != 200 {
		t.Fatalf("Resume = %d", code)
	}
	if page.TotalRecordCount != 2 || len(page.Items) != 2 {
		t.Fatalf("Resume = %+v, want movie + episode", page)
	}
	got := map[string]bool{}
	for _, it := range page.Items {
		got[it.Id] = true
		if it.UserData == nil || it.UserData.PlaybackPositionTicks != pos || it.UserData.Played {
			t.Errorf("Resume item user data = %+v", it)
		}
	}
	if !got[itoa(movieID)] || !got[epNumID] {
		t.Errorf("Resume ids = %v", got)
	}

	// ParentId filters: library scope keeps only its own items.
	if code := get(t, h, "/emby/Users/u/Items/Resume?ParentId="+movieLibID, &page); code != 200 {
		t.Fatalf("Resume by movie library = %d", code)
	}
	if page.TotalRecordCount != 1 || page.Items[0].Id != itoa(movieID) {
		t.Fatalf("Resume by movie library = %+v", page)
	}
	if code := get(t, h, "/emby/Items/Resume?ParentId="+seriesLibID, &page); code != 200 {
		t.Fatalf("Resume by series library = %d", code)
	}
	if page.TotalRecordCount != 1 || page.Items[0].Id != epNumID {
		t.Fatalf("Resume by series library = %+v", page)
	}

	// Marking the episode played removes it from Resume.
	if code, _ := doReq(t, h, http.MethodPost, "/emby/Users/u/PlayedItems/"+epNumID, ""); code != 200 {
		t.Fatalf("PlayedItems = %d", code)
	}
	if code := get(t, h, "/emby/Items/Resume", &page); code != 200 || page.TotalRecordCount != 1 || page.Items[0].Id != itoa(movieID) {
		t.Fatalf("Resume after played = %d %+v", code, page)
	}
}

func TestEmbyNextUp(t *testing.T) {
	h := testHandler(t)
	movieLibID, _, _, seriesID, seasonID, _ := embyFixture(t, h)

	var page embyPage
	if code := get(t, h, "/emby/Shows/NextUp", &page); code != 200 || page.TotalRecordCount != 0 {
		t.Fatalf("NextUp with no history = %d %+v", code, page)
	}

	e1 := episodeByNumber(t, h, seasonID, 1)
	e2 := episodeByNumber(t, h, seasonID, 2)
	for _, e := range []embyItemDTO{e1, e2} {
		if code, _ := doReq(t, h, http.MethodPost, "/emby/Users/u/PlayedItems/"+e.Id, ""); code != 200 {
			t.Fatalf("mark %s played = %d", e.Id, code)
		}
	}
	if code := get(t, h, "/emby/Shows/NextUp", &page); code != 200 {
		t.Fatalf("NextUp = %d", code)
	}
	if page.TotalRecordCount != 1 || len(page.Items) != 1 {
		t.Fatalf("NextUp = %+v, want exactly the next episode", page)
	}
	next := page.Items[0]
	if next.IndexNumber == nil || *next.IndexNumber != 3 || next.SeriesId != itoa(seriesID) {
		t.Fatalf("NextUp item = %+v, want S01E03 of the series", next)
	}

	// An in-progress later episode moves the anchor: after touching
	// E05 the next unwatched after the highest watched is E06.
	e5 := episodeByNumber(t, h, seasonID, 5)
	pos := int64(120 * 10_000_000)
	if code, _ := doReq(t, h, http.MethodPost, "/emby/Sessions/Playing/Progress",
		`{"ItemId":`+e5.Id+`,"PositionTicks":`+itoa(pos)+`}`); code != 204 {
		t.Fatalf("progress E05 = %d", code)
	}
	if code := get(t, h, "/emby/Shows/NextUp?UserId=u&Limit=10", &page); code != 200 {
		t.Fatalf("NextUp after E05 = %d", code)
	}
	if len(page.Items) != 1 || page.Items[0].IndexNumber == nil || *page.Items[0].IndexNumber != 6 {
		t.Fatalf("NextUp after E05 = %+v, want E06", page.Items)
	}

	// ParentId narrowing: the movie library has no series; the series
	// itself keeps its next-up.
	if code := get(t, h, "/emby/Shows/NextUp?ParentId="+movieLibID, &page); code != 200 || page.TotalRecordCount != 0 {
		t.Fatalf("NextUp by movie library = %d %+v", code, page)
	}
	if code := get(t, h, "/Shows/NextUp?ParentId="+itoa(seriesID), &page); code != 200 || len(page.Items) != 1 {
		t.Fatalf("NextUp by series = %d %+v", code, page)
	}
}

func TestSearchHints(t *testing.T) {
	h := testHandler(t)
	_, _, movieID, seriesID, _, _ := embyFixture(t, h)

	var resp searchHintsResponse
	if code := get(t, h, "/emby/Search/Hints?searchTerm="+pct("上海"), &resp); code != 200 {
		t.Fatalf("Hints = %d", code)
	}
	if resp.TotalRecordCount != 1 || len(resp.SearchHints) != 1 {
		t.Fatalf("Hints 上海 = %+v", resp)
	}
	hint := resp.SearchHints[0]
	if hint.ItemId != itoa(seriesID) || hint.Id != hint.ItemId {
		t.Errorf("hint ids = %+v", hint)
	}
	if hint.Name != "上海女子图鉴" || hint.Type != "Series" || hint.MatchedTerm != "上海" {
		t.Errorf("hint = %+v", hint)
	}

	if code := get(t, h, "/Search/Hints?searchTerm="+pct("蜘蛛侠"), &resp); code != 200 {
		t.Fatalf("Hints movie = %d", code)
	}
	if len(resp.SearchHints) != 1 || resp.SearchHints[0].ItemId != itoa(movieID) || resp.SearchHints[0].Type != "Movie" {
		t.Fatalf("Hints 蜘蛛侠 = %+v", resp)
	}

	// Episodes never appear, even when the term matches their series.
	if code := get(t, h, "/emby/Search/Hints?searchTerm="+pct("女子"), &resp); code != 200 || len(resp.SearchHints) != 1 {
		t.Fatalf("Hints 女子 = %d %+v", code, resp)
	}

	// No match / no term: empty hints, still 200.
	if code := get(t, h, "/emby/Search/Hints?searchTerm=zzz-no-such-title", &resp); code != 200 ||
		resp.TotalRecordCount != 0 || len(resp.SearchHints) != 0 {
		t.Fatalf("Hints no-match = %d %+v", code, resp)
	}
	if code := get(t, h, "/emby/Search/Hints", &resp); code != 200 || len(resp.SearchHints) != 0 {
		t.Fatalf("Hints no-term = %d %+v", code, resp)
	}
}

// pct percent-encodes a query value (fixture titles are Chinese).
func pct(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		b.WriteString("%")
		const hex = "0123456789ABCDEF"
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0xf])
	}
	return b.String()
}

func TestEmbyFavoriteAndPlayedItems(t *testing.T) {
	h := testHandler(t)
	movieLibID, _, movieID, _, _, _ := embyFixture(t, h)
	mid := itoa(movieID)

	// Favorite round trip, both route shapes.
	code, body := doReq(t, h, http.MethodPost, "/emby/Users/u/FavoriteItems/"+mid, "")
	if code != 200 {
		t.Fatalf("POST FavoriteItems = %d %s", code, body)
	}
	var ud embyUserDataDTO
	if err := json.Unmarshal(body, &ud); err != nil || !ud.IsFavorite {
		t.Fatalf("favorite response = %+v err %v", ud, err)
	}
	var one embyItemDTO
	if code := get(t, h, "/emby/Items/"+mid, &one); code != 200 || one.UserData == nil || !one.UserData.IsFavorite {
		t.Fatalf("item after favorite = %d %+v", code, one)
	}
	code, body = doReq(t, h, http.MethodDelete, "/Users/u/FavoriteItems/"+mid, "")
	if code != 200 {
		t.Fatalf("DELETE FavoriteItems = %d %s", code, body)
	}
	if err := json.Unmarshal(body, &ud); err != nil || ud.IsFavorite {
		t.Fatalf("unfavorite response = %+v err %v", ud, err)
	}
	if code := get(t, h, "/emby/Items/"+mid, &one); code != 200 || one.UserData == nil || one.UserData.IsFavorite {
		t.Fatalf("item after unfavorite = %d %+v", code, one)
	}

	// Played round trip.
	code, body = doReq(t, h, http.MethodPost, "/emby/Users/u/PlayedItems/"+mid, "")
	if code != 200 {
		t.Fatalf("POST PlayedItems = %d %s", code, body)
	}
	if err := json.Unmarshal(body, &ud); err != nil || !ud.Played {
		t.Fatalf("played response = %+v err %v", ud, err)
	}
	if code := get(t, h, "/emby/Items/"+mid, &one); code != 200 || one.UserData == nil || !one.UserData.Played {
		t.Fatalf("item after played = %d %+v", code, one)
	}
	code, body = doReq(t, h, http.MethodDelete, "/emby/Users/u/PlayedItems/"+mid, "")
	if code != 200 {
		t.Fatalf("DELETE PlayedItems = %d %s", code, body)
	}
	if err := json.Unmarshal(body, &ud); err != nil || ud.Played {
		t.Fatalf("unplayed response = %+v err %v", ud, err)
	}

	// Unknown items and library-encoded ids are JSON 404s.
	for _, p := range []string{"/emby/Users/u/FavoriteItems/999999999", "/emby/Users/u/PlayedItems/" + movieLibID} {
		code, body := doReq(t, h, http.MethodPost, p, "")
		if code != 404 || !strings.HasPrefix(string(body), "{") {
			t.Errorf("POST %s = %d %s, want JSON 404", p, code, body)
		}
	}
}

func TestSystemEndpoint(t *testing.T) {
	h := testHandler(t)
	for _, p := range []string{"/emby/System/Endpoint", "/System/Endpoint"} {
		var ep systemEndpointDTO
		if code := get(t, h, p, &ep); code != 200 {
			t.Fatalf("GET %s = %d", p, code)
		}
		if !ep.IsLocal || !ep.IsInNetwork {
			t.Errorf("endpoint flags = %+v", ep)
		}
		if !strings.HasPrefix(ep.LocalAddress, "http://") || ep.RemoteAddress != ep.LocalAddress {
			t.Errorf("endpoint addresses = %+v", ep)
		}
	}
}

func TestEmbyItemsClientParams(t *testing.T) {
	h := testHandler(t)
	_, _, _, _, seasonID, _ := embyFixture(t, h)

	// The parameter soup real clients send must be accepted (unknown
	// parameters ignored, known ones honored).
	var page embyPage
	p := "/emby/Users/u/Items?ParentId=" + itoa(seasonID) +
		"&Fields=MediaSources,Overview,People&EnableImageTypes=Primary,Backdrop,Thumb" +
		"&ImageTypeLimit=1&EnableImages=true&EnableUserData=true&EnableTotalRecordCount=true"
	if code := get(t, h, p, &page); code != 200 || page.TotalRecordCount != 20 {
		t.Fatalf("Items with client params = %d %+v", code, page)
	}
	// Date-ish SortBy values map onto date_added instead of erroring.
	for _, sb := range []string{"DateLastContentAdded", "PremiereDate", "DateCreated", "SortName", "Random"} {
		if code := get(t, h, "/emby/Items?ParentId="+itoa(seasonID)+"&SortBy="+sb+"&SortOrder=Descending", &page); code != 200 {
			t.Errorf("Items SortBy=%s = %d", sb, code)
		}
	}
}
