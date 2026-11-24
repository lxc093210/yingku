package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"yingku/internal/meta"
	"yingku/internal/scan"
	"yingku/internal/store"
)

func doJSON(t *testing.T, h http.Handler, method, path string, body any, out any) int {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if out != nil && rec.Code >= 200 && rec.Code < 300 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
	return rec.Code
}

func TestJobsEndpoints(t *testing.T) {
	h := testHandler(t)
	var list struct {
		Jobs []jobDTO `json:"Jobs"`
	}
	if code := doJSON(t, h, http.MethodGet, "/api/jobs", nil, &list); code != 200 {
		t.Fatalf("GET /api/jobs = %d", code)
	}
	if len(list.Jobs) != 4 {
		t.Fatalf("jobs = %d, want 4", len(list.Jobs))
	}
	byName := map[string]jobDTO{}
	for _, j := range list.Jobs {
		byName[j.Name] = j
	}
	if j := byName["scan"]; !j.Enabled || j.EveryHours != 6 {
		t.Errorf("scan defaults = %+v", j)
	}
	if j := byName["covers"]; j.Enabled || j.EveryHours != 168 {
		t.Errorf("covers defaults = %+v", j)
	}
	if j := byName["mediainfo"]; j.Pending == nil {
		t.Errorf("mediainfo pending missing: %+v", j)
	}
	if j := byName["metadata"]; j.Pending == nil {
		t.Errorf("metadata pending missing: %+v", j)
	}

	var upd jobDTO
	if code := doJSON(t, h, http.MethodPut, "/api/jobs/scan", map[string]any{"everyHours": 12}, &upd); code != 200 || upd.EveryHours != 12 {
		t.Fatalf("PUT scan = %d %+v", code, upd)
	}
	if code := doJSON(t, h, http.MethodPut, "/api/jobs/scan", map[string]any{"everyHours": 0}, nil); code != 400 {
		t.Errorf("PUT scan everyHours=0 = %d, want 400", code)
	}
	if code := doJSON(t, h, http.MethodPut, "/api/jobs/nope", map[string]any{"enabled": true}, nil); code != 404 {
		t.Errorf("PUT unknown job = %d, want 404", code)
	}
	if code := doJSON(t, h, http.MethodPost, "/api/jobs/nope/run", nil, nil); code != 404 {
		t.Errorf("POST unknown job run = %d, want 404", code)
	}
	if code := doJSON(t, h, http.MethodPost, "/api/jobs/covers/run", nil, nil); code != 202 {
		t.Errorf("POST covers run = %d, want 202", code)
	}
}

func TestCoverEndpoints(t *testing.T) {
	h := testHandler(t)
	var cov coverDTO
	if code := doJSON(t, h, http.MethodGet, "/api/cover", nil, &cov); code != 200 {
		t.Fatalf("GET /api/cover = %d", code)
	}
	if cov.Style != "editorial" || cov.Sort != "datecreated" {
		t.Errorf("cover defaults = %q/%q", cov.Style, cov.Sort)
	}
	if len(cov.Styles) != 5 || len(cov.Libraries) != 2 {
		t.Errorf("styles/libs = %d/%d", len(cov.Styles), len(cov.Libraries))
	}
	if code := doJSON(t, h, http.MethodPut, "/api/cover", map[string]any{"style": "noir"}, &cov); code != 200 || cov.Style != "noir" {
		t.Fatalf("PUT style = %d %+v", code, cov)
	}
	if code := doJSON(t, h, http.MethodPut, "/api/cover", map[string]any{"style": "bogus"}, nil); code != 400 {
		t.Errorf("PUT bogus style = %d, want 400", code)
	}
	if code := doJSON(t, h, http.MethodPut, "/api/cover", map[string]any{"sort": "bogus"}, nil); code != 400 {
		t.Errorf("PUT bogus sort = %d, want 400", code)
	}
}

func TestLibraryOrderEndpoint(t *testing.T) {
	h := testHandler(t)
	var libs []libraryDTO
	if code := doJSON(t, h, http.MethodGet, "/api/libraries", nil, &libs); code != 200 || len(libs) != 2 {
		t.Fatalf("libraries = %d %+v", code, libs)
	}
	rev := []int64{libs[1].ID, libs[0].ID}
	var out []libraryDTO
	if code := doJSON(t, h, http.MethodPut, "/api/libraries/order", map[string]any{"ids": rev}, &out); code != 200 {
		t.Fatalf("PUT order = %d", code)
	}
	if out[0].ID != rev[0] || out[1].ID != rev[1] {
		t.Fatalf("order result = %+v", out)
	}
	if code := doJSON(t, h, http.MethodPut, "/api/libraries/order", map[string]any{"ids": []int64{}}, nil); code != 400 {
		t.Errorf("PUT empty order = %d, want 400", code)
	}
}

func TestLibraryImageEndpoint(t *testing.T) {
	h := testHandler(t)
	var libs []libraryDTO
	doJSON(t, h, http.MethodGet, "/api/libraries", nil, &libs)
	libID := libs[0].ID
	// Upload a cover through the Emby surface (batch 26); the web
	// endpoint must then serve exactly those bytes (uploads outrank
	// the artwork-collage fallback).
	const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8Xw8AAwMCAO+ip1sAAAAASUVORK5CYII="
	up := httptest.NewRequest(http.MethodPost, "/emby/Items/"+itoa(libID+embyLibraryIDOffset)+"/Images/Primary",
		strings.NewReader(pngB64))
	upRec := httptest.NewRecorder()
	h.ServeHTTP(upRec, up)
	if upRec.Code != 204 {
		t.Fatalf("emby upload = %d (%s)", upRec.Code, upRec.Body.String())
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/libraries/"+itoa(libID)+"/image", nil))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "image/") {
		t.Fatalf("image after upload = %d, ct %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	raw, _ := base64.StdEncoding.DecodeString(pngB64)
	if !bytes.Equal(rec.Body.Bytes(), raw) {
		t.Fatalf("served cover is not the uploaded png (%d bytes)", rec.Body.Len())
	}
	// Unknown library: 404.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/libraries/99999/image", nil))
	if rec.Code != 404 {
		t.Fatalf("image for unknown library = %d, want 404", rec.Code)
	}
}

func TestSettingsBaseGuardAndDouban(t *testing.T) {
	h := testHandler(t)
	var dto settingsDTO
	if code := doJSON(t, h, http.MethodGet, "/api/settings", nil, &dto); code != 200 {
		t.Fatalf("GET settings = %d", code)
	}
	if !dto.DoubanAssist {
		t.Errorf("doubanAssist default = false, want true (default on)")
	}
	if code := doJSON(t, h, http.MethodPut, "/api/settings", map[string]any{"tmdbBaseUrl": "https://example.com/3"}, &dto); code != 200 {
		t.Fatalf("PUT base = %d", code)
	}
	if dto.TMDBBaseURL != "https://example.com/3" {
		t.Fatalf("base after PUT = %q", dto.TMDBBaseURL)
	}
	// Empty base must NOT wipe the stored value (batch-28 fix).
	if code := doJSON(t, h, http.MethodPut, "/api/settings", map[string]any{"tmdbBaseUrl": ""}, &dto); code != 200 {
		t.Fatalf("PUT empty base = %d", code)
	}
	if dto.TMDBBaseURL != "https://example.com/3" {
		t.Errorf("base after empty PUT = %q, want preserved", dto.TMDBBaseURL)
	}
	off := false
	if code := doJSON(t, h, http.MethodPut, "/api/settings", map[string]any{"doubanAssist": off}, &dto); code != 200 || dto.DoubanAssist {
		t.Fatalf("PUT doubanAssist=false = %d %+v", code, dto)
	}
}

func TestDoubanGateCacheAndBudget(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	root, _ := filepath.Abs(filepath.Join("..", "..", "fixtures", "library"))
	sc := scan.New(st)
	srv := &Server{st: st, scanner: sc, defaultRoot: root, imgLocks: newKeyedLocks(), running: map[string]bool{}}

	// Fake Douban backed by httptest.
	mux := http.NewServeMux()
	mux.HandleFunc("/j/subject_suggest", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"42","title":"流浪地球","sub_title":"The Wandering Earth","year":"2019","type":"movie"}]`))
	})
	mux.HandleFunc("/subject/42/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<span property="v:summary">中文简介在此。</span>`))
	})
	fake := httptest.NewServer(mux)
	t.Cleanup(fake.Close)
	dc := meta.NewDoubanClient()
	dc.SuggestURL = fake.URL + "/j/subject_suggest"
	dc.SubjectBase = fake.URL + "/subject/"
	dc.Interval = 0
	srv.douban = dc
	srv.beginDoubanRound()

	it := &store.Item{Type: "movie", Title: "The Wandering Earth", OriginalTitle: "The Wandering Earth", Year: 2019, TmdbID: 530915}
	info := srv.doubanLookup(it)
	if info == nil || info.Title != "流浪地球" || info.Overview != "中文简介在此。" {
		t.Fatalf("lookup = %+v", info)
	}
	budgetAfterFirst := srv.doubanBudget
	if budgetAfterFirst != doubanRoundBudget-1 {
		t.Fatalf("budget after first lookup = %d, want %d", budgetAfterFirst, doubanRoundBudget-1)
	}
	// Second lookup is served from the cache: no budget spent.
	info2 := srv.doubanLookup(it)
	if info2 == nil || info2.Title != "流浪地球" {
		t.Fatalf("cached lookup = %+v", info2)
	}
	if srv.doubanBudget != budgetAfterFirst {
		t.Errorf("budget after cached lookup = %d, want %d", srv.doubanBudget, budgetAfterFirst)
	}
	// applyDouban writes the Chinese title/overview only into gaps.
	target := &store.Item{ID: 1, Type: "movie", Title: "The Wandering Earth", TmdbID: 530915}
	// (No item row 1 exists; applyDouban's Fill calls no-op on missing
	// rows but report "changed" from the title fill attempt — instead
	// verify the fill semantics on a real row:)
	libID, err := st.CreateLibrary("豆瓣库", "movie", []string{"/db/root"})
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := st.Begin()
	rid, err := tx.UpsertItem(&store.Item{LibraryID: libID, Type: "movie", Path: "/db/root/W", Title: "The Wandering Earth", OriginalTitle: "The Wandering Earth", SortTitle: "The Wandering Earth", Year: 2019, TmdbID: 530915})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	row, _ := st.GetItem(rid)
	if !srv.applyDouban(row) {
		t.Fatalf("applyDouban reported no change")
	}
	row, _ = st.GetItem(rid)
	if row.Title != "流浪地球" || row.Overview != "中文简介在此。" {
		t.Errorf("after applyDouban: title %q overview %q", row.Title, row.Overview)
	}
	_ = target
}
