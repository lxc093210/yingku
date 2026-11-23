package api

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"yingku/internal/scan"
	"yingku/internal/store"
)

func getRaw(t *testing.T, h http.Handler, path string) (int, []byte, http.Header) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes(), rec.Header()
}

func jpegBytes(t *testing.T, w, hgt int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, hgt))
	for y := 0; y < hgt; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// thumbFixture builds a handler over a temp store whose library root is
// a temp dir, plus one movie whose poster is a generated JPEG on disk.
func thumbFixture(t *testing.T) (http.Handler, *store.Store, int64, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	libDir := filepath.Join(dir, "lib")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	libID, err := st.CreateLibrary("测试库", "movie", []string{libDir})
	if err != nil {
		t.Fatal(err)
	}
	orig := jpegBytes(t, 400, 600, color.RGBA{R: 30, G: 90, B: 160, A: 255})
	posterPath := filepath.Join(libDir, "poster.jpg")
	if err := os.WriteFile(posterPath, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	tx, err := st.Begin()
	if err != nil {
		t.Fatal(err)
	}
	id, err := tx.UpsertItem(&store.Item{
		LibraryID: libID, Type: "movie", Title: "缩略图测试", SortTitle: "缩略图测试",
		Path: filepath.Join(libDir, "movie"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.ReplaceImages(id, map[string]store.ImageFile{
		"poster": {Path: posterPath, Width: 400, Height: 600},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return New(st, scan.New(st), ""), st, id, posterPath, orig
}

func TestImageThumbGenerationAndCache(t *testing.T) {
	h, st, id, posterPath, orig := thumbFixture(t)
	base := fmt.Sprintf("/api/items/%d/image/poster", id)

	// No w: the original bytes, unchanged behavior.
	code, body, _ := getRaw(t, h, base)
	if code != 200 || !bytes.Equal(body, orig) {
		t.Fatalf("no-w = %d, %d bytes; want 200 + original %d bytes", code, len(body), len(orig))
	}
	// Invalid widths fall back to the original.
	for _, q := range []string{"?w=10", "?w=5000", "?w=abc", "?w=-50"} {
		code, body, _ = getRaw(t, h, base+q)
		if code != 200 || !bytes.Equal(body, orig) {
			t.Fatalf("%s = %d, %d bytes; want original", q, code, len(body))
		}
	}
	// Wider than the source: no upscale, original served.
	code, body, _ = getRaw(t, h, base+"?w=1280")
	if code != 200 || !bytes.Equal(body, orig) {
		t.Fatalf("w=1280 = %d, %d bytes; want original", code, len(body))
	}

	// A real resize: 400x600 -> w=100 gives a 100x150 JPEG, cached.
	code, body, hdr := getRaw(t, h, base+"?w=100")
	if code != 200 {
		t.Fatalf("w=100 code = %d", code)
	}
	if ct := hdr.Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("w=100 Content-Type = %q", ct)
	}
	dec, err := jpeg.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("w=100 body is not JPEG: %v", err)
	}
	if b := dec.Bounds(); b.Dx() != 100 || b.Dy() != 150 {
		t.Fatalf("w=100 size = %dx%d, want 100x150", b.Dx(), b.Dy())
	}
	cacheFile := filepath.Join(filepath.Dir(st.DBPath()), "cache", "thumbs",
		fmt.Sprintf("%d-poster-100.jpg", id))
	if !fileExists(cacheFile) {
		t.Fatalf("thumb cache not written: %s", cacheFile)
	}

	// Cache hit: replacing the source must not change the served thumb.
	red := jpegBytes(t, 400, 600, color.RGBA{R: 200, G: 30, B: 30, A: 255})
	if err := os.WriteFile(posterPath, red, 0o644); err != nil {
		t.Fatal(err)
	}
	code, body2, _ := getRaw(t, h, base+"?w=100")
	if code != 200 || !bytes.Equal(body2, body) {
		t.Fatalf("cache hit mismatch: %d, %d vs %d bytes", code, len(body2), len(body))
	}
}

func TestHomeSlim(t *testing.T) {
	h := testHandler(t)
	var home map[string]any
	if code := get(t, h, "/api/home", &home); code != 200 {
		t.Fatalf("home code = %d", code)
	}
	for _, gone := range []string{"Hero", "Heroes", "Recent"} {
		if _, ok := home[gone]; ok {
			t.Errorf("home still contains %s", gone)
		}
	}
	for _, want := range []string{"Libraries", "ContinueWatching"} {
		if _, ok := home[want]; !ok {
			t.Errorf("home missing %s", want)
		}
	}
}

// fakeTMDB serves the three TMDB endpoints the lazy fillers use, plus
// the image CDN paths, and counts CDN downloads.
func fakeTMDB(t *testing.T, poster, backdrop []byte) (*httptest.Server, *int32) {
	t.Helper()
	var downloads int32
	mux := http.NewServeMux()
	mux.HandleFunc("/3/movie/4242/images", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"posters":[
			{"file_path":"/p0.jpg","width":500,"vote_average":5.0},
			{"file_path":"/p1.jpg","width":780,"vote_average":7.5}],
			"backdrops":[
			{"file_path":"/b0.jpg","width":1000,"height":562},
			{"file_path":"/b1.jpg","width":1920,"height":1080}]}`)
	})
	mux.HandleFunc("/3/movie/4242", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"overview":"TMDB 补的简介","vote_average":8.4,"runtime":123,`+
			`"genres":[{"name":"动作"},{"name":"科幻"}]}`)
	})
	mux.HandleFunc("/t/p/w780/p1.jpg", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&downloads, 1)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(poster)
	})
	mux.HandleFunc("/t/p/w1280/b1.jpg", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&downloads, 1)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(backdrop)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &downloads
}

func TestTMDBLazyArtAndMetaFill(t *testing.T) {
	poster := jpegBytes(t, 780, 1170, color.RGBA{R: 10, G: 120, B: 60, A: 255})
	backdrop := jpegBytes(t, 1280, 720, color.RGBA{R: 90, G: 40, B: 140, A: 255})
	srv, downloads := fakeTMDB(t, poster, backdrop)

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SetSetting(settingTMDBBaseURL, srv.URL+"/3"); err != nil {
		t.Fatal(err)
	}
	libID, err := st.CreateLibrary("懒补库", "movie", []string{filepath.Join(dir, "lib")})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(title string, overview string, rating float64, genres []string) int64 {
		tx, err := st.Begin()
		if err != nil {
			t.Fatal(err)
		}
		id, err := tx.UpsertItem(&store.Item{
			LibraryID: libID, Type: "movie", Title: title, SortTitle: title,
			Path:   filepath.Join(dir, "lib", title),
			TmdbID: 4242, Overview: overview, Rating: rating, Genres: genres,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// Item 1: everything empty except a genre list that must survive.
	id1 := mk("懒补一号", "", 0, []string{"剧情"})
	// Item 2: overview present (must survive), rating/genres/runtime empty.
	id2 := mk("懒补二号", "本地已有简介", 0, nil)

	h := New(st, scan.New(st), "")

	// --- lazy art: poster fetched once, cached, then served from cache.
	code, body, _ := getRaw(t, h, fmt.Sprintf("/api/items/%d/image/poster", id1))
	if code != 200 || !bytes.Equal(body, poster) {
		t.Fatalf("lazy poster = %d, %d bytes; want the TMDB poster", code, len(body))
	}
	artFile := filepath.Join(dir, "cache", "tmdb-art", fmt.Sprintf("%d-poster.jpg", id1))
	cached, err := os.ReadFile(artFile)
	if err != nil || !bytes.Equal(cached, poster) {
		t.Fatalf("tmdb-art cache = %v, %d bytes", err, len(cached))
	}
	if n := atomic.LoadInt32(downloads); n != 1 {
		t.Fatalf("downloads after first poster = %d, want 1", n)
	}
	code, body, _ = getRaw(t, h, fmt.Sprintf("/api/items/%d/image/poster", id1))
	if code != 200 || !bytes.Equal(body, poster) {
		t.Fatalf("cached poster = %d", code)
	}
	if n := atomic.LoadInt32(downloads); n != 1 {
		t.Fatalf("downloads after cached poster = %d, want 1 (cache hit)", n)
	}
	// Backdrop comes down at w1280.
	code, body, _ = getRaw(t, h, fmt.Sprintf("/api/items/%d/image/backdrop", id1))
	if code != 200 || !bytes.Equal(body, backdrop) {
		t.Fatalf("lazy backdrop = %d, %d bytes", code, len(body))
	}
	// A scaled request is generated from the cached TMDB art.
	code, body, _ = getRaw(t, h, fmt.Sprintf("/api/items/%d/image/poster?w=100", id1))
	if code != 200 {
		t.Fatalf("scaled lazy poster = %d", code)
	}
	dec, err := jpeg.Decode(bytes.NewReader(body))
	if err != nil || dec.Bounds().Dx() != 100 {
		t.Fatalf("scaled lazy poster: err=%v bounds=%v", err, dec.Bounds())
	}

	// --- metadata fill: empty fields filled, present fields preserved.
	var d1 itemDTO
	if code := get(t, h, "/api/items/"+itoa(id1), &d1); code != 200 {
		t.Fatalf("detail 1 = %d", code)
	}
	if d1.Overview != "TMDB 补的简介" || d1.CommunityRating != 8.4 {
		t.Errorf("detail 1 meta = %q / %v", d1.Overview, d1.CommunityRating)
	}
	if len(d1.Genres) != 1 || d1.Genres[0] != "剧情" {
		t.Errorf("detail 1 genres = %v, want [剧情] (not overwritten)", d1.Genres)
	}
	if d1.RunTimeTicks != 123*60*10_000_000 {
		t.Errorf("detail 1 runtime ticks = %d, want 123min", d1.RunTimeTicks)
	}
	var d2 itemDTO
	if code := get(t, h, "/api/items/"+itoa(id2), &d2); code != 200 {
		t.Fatalf("detail 2 = %d", code)
	}
	if d2.Overview != "本地已有简介" {
		t.Errorf("detail 2 overview = %q, want local value preserved", d2.Overview)
	}
	if d2.CommunityRating != 8.4 || len(d2.Genres) != 2 {
		t.Errorf("detail 2 filled = %v / %v, want 8.4 + 2 genres", d2.CommunityRating, d2.Genres)
	}
	// The fill is persisted, not just rendered.
	it, err := st.GetItem(id1)
	if err != nil || it == nil {
		t.Fatalf("get item 1: %v", err)
	}
	if it.Overview != "TMDB 补的简介" || it.Rating != 8.4 || it.RuntimeMin != 123 {
		t.Errorf("persisted meta = %q / %v / %d", it.Overview, it.Rating, it.RuntimeMin)
	}
}
