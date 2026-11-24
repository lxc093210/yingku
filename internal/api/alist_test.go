package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"yingku/internal/scan"
	"yingku/internal/store"
)

// ---- fake Alist server -------------------------------------------------

type fakeEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// fakeAlistTree maps remote dir paths to their entries.
type fakeAlistTree map[string][]fakeEntry

func fakeAlistServer(t *testing.T, tree fakeAlistTree) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	writeEnv := func(w http.ResponseWriter, code int, data any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": "success", "data": data})
	}
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Username != "dav" || body.Password != "pw" {
			writeEnv(w, 401, nil)
			return
		}
		writeEnv(w, 200, map[string]any{"token": "tok-test"})
	})
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "tok-test" {
			writeEnv(w, 401, nil)
			return false
		}
		return true
	}
	mux.HandleFunc("/api/fs/list", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var body struct {
			Path string `json:"path"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		entries, ok := tree[body.Path]
		if !ok {
			writeEnv(w, 500, nil)
			return
		}
		writeEnv(w, 200, map[string]any{"total": len(entries), "content": entries})
	})
	mux.HandleFunc("/api/fs/get", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var body struct {
			Path string `json:"path"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeEnv(w, 200, map[string]any{"raw_url": "https://cdn.test" + body.Path + "?sign=abc"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// ---- fake TMDB server --------------------------------------------------

func fakeTMDBServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/search/movie", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		var results []map[string]any
		switch q {
		case "信条":
			results = []map[string]any{{"id": 577922, "title": "信条", "original_title": "Tenet", "release_date": "2020-08-26"}}
		case "无名之辈":
			// Partial containment only — must NOT match.
			results = []map[string]any{{"id": 111, "title": "无名之辈外传", "original_title": "", "release_date": "2018-01-01"}}
		case "精确片":
			results = []map[string]any{{"id": 42, "title": "精确片", "original_title": "Exact", "release_date": "2020-05-01"}}
		case "冲突片":
			// Exact title, wrong year — must NOT match.
			results = []map[string]any{{"id": 43, "title": "冲突片", "original_title": "Conflict", "release_date": "1998-05-01"}}
		default:
			results = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	})
	mux.HandleFunc("/movie/577922", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"title": "信条", "overview": "一个特工试图阻止第三次世界大战。",
			"vote_average": 7.4, "runtime": 150,
			"genres": []map[string]any{{"name": "动作"}, {"name": "科幻"}},
		})
	})
	mux.HandleFunc("/movie/577922/images", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"posters": []any{}, "backdrops": []any{}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// ---- tests --------------------------------------------------------------

func TestAlistSettingsContract(t *testing.T) {
	h := testHandler(t)
	put := func(body map[string]any) settingsDTO {
		var dto settingsDTO
		if code := doJSON(t, h, http.MethodPut, "/api/settings", body, &dto); code != 200 {
			t.Fatalf("PUT /api/settings = %d", code)
		}
		return dto
	}
	dto := put(map[string]any{
		"alistBaseUrl":  "http://192.168.1.5:5678",
		"alistUsername": "dav",
		"alistPassword": "s3cret-value",
	})
	if dto.AlistBaseURL != "http://192.168.1.5:5678" || dto.AlistUsername != "dav" || !dto.AlistPasswordSet {
		t.Fatalf("settings after PUT = %+v", dto)
	}
	// The password must never appear in any settings response body.
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "s3cret-value") {
		t.Fatal("settings GET leaks the alist password")
	}
	// An empty password on PUT must not clear the stored one.
	dto = put(map[string]any{"alistPassword": ""})
	if !dto.AlistPasswordSet {
		t.Fatal("empty password PUT cleared the stored password")
	}
}

func TestAlistLibraryCreateAPI(t *testing.T) {
	h := testHandler(t)
	var dto libraryDTO
	code := doJSON(t, h, http.MethodPost, "/api/libraries", map[string]any{
		"Name": "4K REMUX", "Kind": "movie", "SourceType": "alist",
		"Paths": []string{"/电影/4K REMUX"},
	}, &dto)
	if code != 201 || dto.SourceType != "alist" || len(dto.Paths) != 1 || dto.Paths[0] != "/电影/4K REMUX" {
		t.Fatalf("create alist library = %d %+v", code, dto)
	}
	// Two remote roots are rejected.
	if code := doJSON(t, h, http.MethodPost, "/api/libraries", map[string]any{
		"Name": "bad", "Kind": "movie", "SourceType": "alist",
		"Paths": []string{"/电影", "/电视剧"},
	}, nil); code != 400 {
		t.Errorf("alist library with 2 paths = %d, want 400", code)
	}
	// A relative remote path is rejected.
	if code := doJSON(t, h, http.MethodPost, "/api/libraries", map[string]any{
		"Name": "bad2", "Kind": "movie", "SourceType": "alist",
		"Paths": []string{"电影/4K REMUX"},
	}, nil); code != 400 {
		t.Errorf("alist library with relative path = %d, want 400", code)
	}
	// Local libraries keep working and report SourceType "local".
	var local libraryDTO
	code = doJSON(t, h, http.MethodPost, "/api/libraries", map[string]any{
		"Name": "本地", "Kind": "movie", "Paths": []string{"/data/movies-x"},
	}, &local)
	if code != 201 || local.SourceType != "local" {
		t.Fatalf("create local library = %d %+v", code, local)
	}
}

func TestMatchTMDBMovieClassification(t *testing.T) {
	srv := fakeTMDBServer(t)
	cfg := tmdbConfig{base: srv.URL, custom: true}
	cases := []struct {
		title string
		year  int
		want  int
	}{
		{"精确片", 2020, 42},  // exact title + year agrees
		{"冲突片", 2020, 0},   // exact title + year conflict
		{"无名之辈", 2018, 0},  // partial containment only
		{"不存在的片", 2020, 0}, // no candidates
	}
	for _, c := range cases {
		got, err := matchTMDBMovie(cfg, c.title, c.year)
		if err != nil {
			t.Fatalf("matchTMDBMovie(%q): %v", c.title, err)
		}
		if got != c.want {
			t.Errorf("matchTMDBMovie(%q, %d) = %d, want %d", c.title, c.year, got, c.want)
		}
	}
}

// TestAlistRemoteScanIntegration runs the full remote scan against a
// fake Alist + fake TMDB: one matched folder, one unmatched folder,
// one collection folder (skipped), then verifies playback wiring.
func TestAlistRemoteScanIntegration(t *testing.T) {
	const root = "/电影/4K REMUX"
	const tenetFolder = "信条 4K REMUX（2020）"
	const tenetFile = "Tenet.2020.2160p.UHD.BluRay.REMUX.HEVC.TrueHD.Atmos.mkv"
	tree := fakeAlistTree{
		root: {
			{Name: tenetFolder, IsDir: true},
			{Name: "无名之辈 4K REMUX（2018）", IsDir: true},
			{Name: "007系列 27部", IsDir: true},
			{Name: "readme.txt", Size: 12},
		},
		root + "/" + tenetFolder: {
			{Name: tenetFile, Size: 50_000_000_000},
			{Name: "Tenet.sample.mkv", Size: 100_000_000},
			{Name: "信条.nfo", Size: 2048},
		},
		root + "/无名之辈 4K REMUX（2018）": {
			{Name: "A.Cool.Fish.2018.1080p.WEB-DL.mkv", Size: 4_000_000_000},
		},
		root + "/007系列 27部": {
			{Name: "007大破天幕杀机.mkv", Size: 8_000_000_000},
		},
	}
	alistSrv := fakeAlistServer(t, tree)
	tmdbSrv := fakeTMDBServer(t)

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for k, v := range map[string]string{
		settingAlistBaseURL:  alistSrv.URL,
		settingAlistUsername: "dav",
		settingAlistPassword: "pw",
		settingTMDBBaseURL:   tmdbSrv.URL,
	} {
		if err := st.SetSetting(k, v); err != nil {
			t.Fatal(err)
		}
	}
	libID, err := st.CreateLibraryWithType("4K REMUX", "movie", store.SourceTypeAlist, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	sc := scan.New(st)
	h := New(st, sc, t.TempDir()) // wires scanner.RemoteScan

	if err := sc.ScanLibraryByID(libID); err != nil {
		t.Fatalf("remote scan: %v", err)
	}
	status := sc.Status()
	if status.Items != 2 || status.Skipped != 1 {
		t.Errorf("scan status = items %d skipped %d, want items 2 skipped 1 (collection)",
			status.Items, status.Skipped)
	}
	items, _, err := st.ListTopLevel(libID, "", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	byTitle := map[string]*store.Item{}
	for _, it := range items {
		byTitle[it.Title] = it
	}
	tenet := byTitle["信条"]
	if tenet == nil {
		t.Fatalf("信条 item missing: %v", keysOf(byTitle))
	}
	if tenet.TmdbID != 577922 {
		t.Errorf("信条 tmdb = %d, want 577922 (matched)", tenet.TmdbID)
	}
	if tenet.Path != root+"/"+tenetFolder {
		t.Errorf("信条 path = %q", tenet.Path)
	}
	// Detail fill ran through the standard TMDB fill.
	if tenet.Overview == "" || tenet.Rating != 7.4 || tenet.RuntimeMin != 150 {
		t.Errorf("信条 fill incomplete: overview=%q rating=%v runtime=%d", tenet.Overview, tenet.Rating, tenet.RuntimeMin)
	}
	srcs, err := st.SourcesOf(tenet.ID)
	if err != nil || len(srcs) != 1 {
		t.Fatalf("信条 sources = %v, %v", srcs, err)
	}
	src := srcs[0]
	if src.StrmPath != root+"/"+tenetFolder+"/"+tenetFile {
		t.Errorf("source path = %q (largest video file must win)", src.StrmPath)
	}
	if src.TargetURL != "" || src.MediaInfoSource != "filename" || src.Width != 3840 || src.Height != 2160 {
		t.Errorf("source = %+v", src)
	}
	unmatched := byTitle["无名之辈"]
	if unmatched == nil {
		t.Fatalf("无名之辈 item missing: %v", keysOf(byTitle))
	}
	if unmatched.TmdbID != 0 || unmatched.Year != 2018 {
		t.Errorf("无名之辈 = tmdb %d year %d, want tmdb 0 year 2018 (unmatched but入库)", unmatched.TmdbID, unmatched.Year)
	}

	// Rescan: fingerprints unchanged → both items skipped, none rewritten.
	if err := sc.ScanLibraryByID(libID); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	status = sc.Status()
	// Counters accumulate across both scans: the rescan skips both
	// items by fingerprint and the collection folder again (+3).
	if status.Items != 2 || status.Skipped != 4 {
		t.Errorf("after rescan status = items %d skipped %d, want items 2 skipped 4",
			status.Items, status.Skipped)
	}

	// Playback: the web stream endpoint 302s to the raw URL.
	wantRaw := "https://cdn.test" + root + "/" + tenetFolder + "/" + tenetFile + "?sign=abc"
	for _, url := range []string{
		fmt.Sprintf("/api/items/%d/stream", tenet.ID),
		fmt.Sprintf("/emby/Videos/%d/stream", tenet.ID),
	} {
		req := httptest.NewRequest(http.MethodGet, url, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != wantRaw {
			t.Errorf("GET %s = %d Location %q, want 302 %q", url, rec.Code, rec.Header().Get("Location"), wantRaw)
		}
	}
	// PlaybackInfo hands the raw URL out as MediaSource.Path.
	var pb struct {
		MediaSources []struct {
			Path string `json:"Path"`
		} `json:"MediaSources"`
	}
	if code := get(t, h, fmt.Sprintf("/emby/Items/%d/PlaybackInfo", tenet.ID), &pb); code != 200 {
		t.Fatalf("PlaybackInfo = %d", code)
	}
	if len(pb.MediaSources) != 1 || pb.MediaSources[0].Path != wantRaw {
		t.Errorf("PlaybackInfo sources = %+v, want Path %q", pb.MediaSources, wantRaw)
	}
}

func keysOf(m map[string]*store.Item) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// blockingTMDBServer is a TMDB fake whose search for 阻塞片 parks on
// a channel until the test releases it, so the test can observe the
// store while a remote scan is deterministically mid-prep.
func blockingTMDBServer(t *testing.T, entered chan<- struct{}, release <-chan struct{}) *httptest.Server {
	t.Helper()
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/search/movie", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		switch q {
		case "阻塞片":
			once.Do(func() { close(entered) })
			<-release
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
				{"id": 44, "title": "阻塞片", "original_title": "Blocker", "release_date": "2021-03-01"},
			}})
		case "精确片":
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
				{"id": 42, "title": "精确片", "original_title": "Exact", "release_date": "2020-05-01"},
			}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{}})
		}
	})
	mux.HandleFunc("/movie/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/images") {
			_ = json.NewEncoder(w).Encode(map[string]any{"posters": []any{}, "backdrops": []any{}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"title": "片", "overview": "简介", "vote_average": 7.0, "runtime": 100,
			"genres": []map[string]any{},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestAlistScanPrepDoesNotHoldStoreConnection is the regression test
// for the batch-29 defect: the remote scan used to keep a store.Tx
// open across TMDB network calls, and with MaxOpenConns(1) that
// starved every other store user (the whole API) for the length of
// a batch. The scan must prep with no transaction open: while a
// TMDB search is parked mid-scan, a store read must still complete.
func TestAlistScanPrepDoesNotHoldStoreConnection(t *testing.T) {
	const root = "/电影/4K REMUX"
	tree := fakeAlistTree{
		root: {
			{Name: "精确片 4K REMUX（2020）", IsDir: true},
			{Name: "阻塞片 4K REMUX（2021）", IsDir: true},
		},
		root + "/精确片 4K REMUX（2020）": {
			{Name: "Exact.2020.2160p.REMUX.mkv", Size: 20_000_000_000},
		},
		root + "/阻塞片 4K REMUX（2021）": {
			{Name: "Blocker.2021.2160p.REMUX.mkv", Size: 20_000_000_000},
		},
	}
	alistSrv := fakeAlistServer(t, tree)
	entered := make(chan struct{})
	release := make(chan struct{})
	tmdbSrv := blockingTMDBServer(t, entered, release)

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for k, v := range map[string]string{
		settingAlistBaseURL:  alistSrv.URL,
		settingAlistUsername: "dav",
		settingAlistPassword: "pw",
		settingTMDBBaseURL:   tmdbSrv.URL,
	} {
		if err := st.SetSetting(k, v); err != nil {
			t.Fatal(err)
		}
	}
	libID, err := st.CreateLibraryWithType("4K REMUX", "movie", store.SourceTypeAlist, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	sc := scan.New(st)
	_ = New(st, sc, t.TempDir()) // wires scanner.RemoteScan

	scanDone := make(chan error, 1)
	go func() { scanDone <- sc.ScanLibraryByID(libID) }()

	// Wait until the scan is deterministically parked inside the
	// TMDB search for the second folder — i.e. the first folder has
	// been fully prepped and the scan is mid-prep for the batch.
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("scan never reached the blocking TMDB search")
	}

	// A store read must complete promptly even though the scan is
	// in flight: prep holds no transaction, so the single store
	// connection is free.
	readDone := make(chan error, 1)
	go func() {
		_, err := st.GetLibrary(libID)
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("store read during scan prep: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("store read blocked >2s during scan prep: a transaction is being held open across network calls")
	}

	close(release)
	select {
	case err := <-scanDone:
		if err != nil {
			t.Fatalf("remote scan: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("scan did not finish after the TMDB search was released")
	}
	if got := sc.Status().Items; got != 2 {
		t.Errorf("scan items = %d, want 2", got)
	}
	items, _, err := st.ListTopLevel(libID, "", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Errorf("items in store = %d, want 2", len(items))
	}
}
