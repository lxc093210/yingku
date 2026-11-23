package api

import (
	"bytes"
	"fmt"
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

// fakeTVTMDB serves the TMDB endpoints the episode fallback uses for
// series 777: episode 1 has a still, episode 2 has none. It counts
// image CDN downloads per size class.
func fakeTVTMDB(t *testing.T, still, poster, backdrop []byte) (*httptest.Server, *int32, *int32) {
	t.Helper()
	var stillDL, artDL int32
	mux := http.NewServeMux()
	mux.HandleFunc("/3/tv/777/season/1/episode/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"still_path":"/still1.jpg"}`)
	})
	mux.HandleFunc("/3/tv/777/season/1/episode/2", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"still_path":""}`)
	})
	mux.HandleFunc("/3/tv/777/images", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"posters":[{"file_path":"/sp.jpg","width":780,"vote_average":7.0}],`+
			`"backdrops":[{"file_path":"/sb.jpg","width":1920,"height":1080}]}`)
	})
	mux.HandleFunc("/t/p/w780/still1.jpg", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&stillDL, 1)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(still)
	})
	mux.HandleFunc("/t/p/w780/sp.jpg", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&artDL, 1)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(poster)
	})
	mux.HandleFunc("/t/p/w1280/sb.jpg", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&artDL, 1)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(backdrop)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &stillDL, &artDL
}

type epFixture struct {
	h                    http.Handler
	dir                  string
	seriesID, seasonID   int64
	ep1ID, ep2ID         int64
	localPoster, localBd []byte
}

// epFixtureSetup builds a store with one series (TMDB 777), one season
// and two episodes that own no images at all. The series optionally
// carries local poster/backdrop files inside the library root.
func epFixtureSetup(t *testing.T, tmdbURL string, withLocalPoster, withLocalBackdrop bool) epFixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if tmdbURL != "" {
		if err := st.SetSetting(settingTMDBBaseURL, tmdbURL+"/3"); err != nil {
			t.Fatal(err)
		}
	}
	libDir := filepath.Join(dir, "lib")
	seriesDir := filepath.Join(libDir, "剧集 (2026) {tmdbid-777}")
	if err := os.MkdirAll(seriesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	libID, err := st.CreateLibrary("剧集库", "series", []string{libDir})
	if err != nil {
		t.Fatal(err)
	}
	fx := epFixture{dir: dir}
	imgs := map[string]store.ImageFile{}
	if withLocalPoster {
		fx.localPoster = jpegBytes(t, 400, 600, color.RGBA{R: 20, G: 160, B: 90, A: 255})
		p := filepath.Join(seriesDir, "poster.jpg")
		if err := os.WriteFile(p, fx.localPoster, 0o644); err != nil {
			t.Fatal(err)
		}
		imgs["poster"] = store.ImageFile{Path: p, Width: 400, Height: 600}
	}
	if withLocalBackdrop {
		fx.localBd = jpegBytes(t, 1280, 720, color.RGBA{R: 150, G: 80, B: 20, A: 255})
		p := filepath.Join(seriesDir, "backdrop.jpg")
		if err := os.WriteFile(p, fx.localBd, 0o644); err != nil {
			t.Fatal(err)
		}
		imgs["backdrop"] = store.ImageFile{Path: p, Width: 1280, Height: 720}
	}
	tx, err := st.Begin()
	if err != nil {
		t.Fatal(err)
	}
	intp := func(v int) *int { return &v }
	fx.seriesID, err = tx.UpsertItem(&store.Item{
		LibraryID: libID, Type: "series", Title: "测试剧集", SortTitle: "测试剧集",
		Path: seriesDir, TmdbID: 777,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) > 0 {
		if err := tx.ReplaceImages(fx.seriesID, imgs); err != nil {
			t.Fatal(err)
		}
	}
	fx.seasonID, err = tx.UpsertItem(&store.Item{
		LibraryID: libID, Type: "season", Title: "第 1 季", SortTitle: "第 1 季",
		Path:     filepath.Join(seriesDir, "Season 1"),
		ParentID: &fx.seriesID, SeriesID: &fx.seriesID, SeasonNum: intp(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	mkEp := func(num int) int64 {
		id, err := tx.UpsertItem(&store.Item{
			LibraryID: libID, Type: "episode",
			Title: fmt.Sprintf("第 %d 集", num), SortTitle: fmt.Sprintf("第 %d 集", num),
			Path:     filepath.Join(seriesDir, "Season 1", fmt.Sprintf("E%d.strm", num)),
			ParentID: &fx.seasonID, SeriesID: &fx.seriesID,
			SeasonNum: intp(1), EpisodeNum: intp(num),
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	fx.ep1ID = mkEp(1)
	fx.ep2ID = mkEp(2)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	fx.h = New(st, scan.New(st), "")
	return fx
}

func imgURL(id int64, kind string) string {
	return fmt.Sprintf("/api/items/%d/image/%s", id, kind)
}

// ① An episode with no still on TMDB falls back to the series' local
// backdrop; a season does the same; an episode poster is the
// episode's own still when TMDB has one (batch 18), else the series'
// local poster.
func TestEpisodeImageFallsBackToSeriesLocal(t *testing.T) {
	still := jpegBytes(t, 780, 439, color.RGBA{R: 200, G: 200, B: 30, A: 255})
	poster := jpegBytes(t, 780, 1170, color.RGBA{R: 30, G: 30, B: 200, A: 255})
	backdrop := jpegBytes(t, 1280, 720, color.RGBA{R: 200, G: 30, B: 120, A: 255})
	srv, stillDL, _ := fakeTVTMDB(t, still, poster, backdrop)
	fx := epFixtureSetup(t, srv.URL, true, true)

	// Episode 2 has no still: its backdrop is the series' local one.
	code, body, _ := getRaw(t, fx.h, imgURL(fx.ep2ID, "backdrop"))
	if code != 200 || !bytes.Equal(body, fx.localBd) {
		t.Fatalf("ep2 backdrop = %d, %d bytes; want series local backdrop", code, len(body))
	}
	// A season carries no art either: same series backdrop.
	code, body, _ = getRaw(t, fx.h, imgURL(fx.seasonID, "backdrop"))
	if code != 200 || !bytes.Equal(body, fx.localBd) {
		t.Fatalf("season backdrop = %d, %d bytes; want series local backdrop", code, len(body))
	}
	// Episode poster is the episode's own still (batch 18: clients
	// list episodes by Primary; the series poster made every episode
	// wear the same cover).
	code, body, _ = getRaw(t, fx.h, imgURL(fx.ep1ID, "poster"))
	if code != 200 || !bytes.Equal(body, still) {
		t.Fatalf("ep1 poster = %d, %d bytes; want episode still", code, len(body))
	}
	if n := atomic.LoadInt32(stillDL); n != 1 {
		t.Fatalf("still downloads = %d, want 1 (poster fetched the still)", n)
	}
	// Episode 2 has no still: its poster falls back to the series'.
	code, body, _ = getRaw(t, fx.h, imgURL(fx.ep2ID, "poster"))
	if code != 200 || !bytes.Equal(body, fx.localPoster) {
		t.Fatalf("ep2 poster = %d, %d bytes; want series local poster", code, len(body))
	}
}

// ② An episode with a TMDB still gets the still as its backdrop,
// cached under the episode id; a repeat request does not refetch.
func TestEpisodeBackdropUsesTMDBStill(t *testing.T) {
	still := jpegBytes(t, 780, 439, color.RGBA{R: 200, G: 200, B: 30, A: 255})
	poster := jpegBytes(t, 780, 1170, color.RGBA{R: 30, G: 30, B: 200, A: 255})
	backdrop := jpegBytes(t, 1280, 720, color.RGBA{R: 200, G: 30, B: 120, A: 255})
	srv, stillDL, _ := fakeTVTMDB(t, still, poster, backdrop)
	fx := epFixtureSetup(t, srv.URL, true, true)

	code, body, _ := getRaw(t, fx.h, imgURL(fx.ep1ID, "backdrop"))
	if code != 200 || !bytes.Equal(body, still) {
		t.Fatalf("ep1 backdrop = %d, %d bytes; want the TMDB still", code, len(body))
	}
	artFile := filepath.Join(fx.dir, "cache", "tmdb-art", fmt.Sprintf("%d-backdrop.jpg", fx.ep1ID))
	cached, err := os.ReadFile(artFile)
	if err != nil || !bytes.Equal(cached, still) {
		t.Fatalf("episode still cache = %v, %d bytes", err, len(cached))
	}
	if n := atomic.LoadInt32(stillDL); n != 1 {
		t.Fatalf("still downloads = %d, want 1", n)
	}
	code, body, _ = getRaw(t, fx.h, imgURL(fx.ep1ID, "backdrop"))
	if code != 200 || !bytes.Equal(body, still) {
		t.Fatalf("ep1 backdrop (cached) = %d", code)
	}
	if n := atomic.LoadInt32(stillDL); n != 1 {
		t.Fatalf("still downloads after repeat = %d, want 1 (cache hit)", n)
	}
	// A scaled request is generated from the cached still.
	code, body, _ = getRaw(t, fx.h, imgURL(fx.ep1ID, "backdrop")+"?w=100")
	if code != 200 {
		t.Fatalf("ep1 backdrop?w=100 = %d", code)
	}
	dec, err := jpeg.Decode(bytes.NewReader(body))
	if err != nil || dec.Bounds().Dx() != 100 {
		t.Fatalf("scaled still: err=%v bounds=%v", err, dec.Bounds())
	}
}

// ③ With no local art and no still, the episode inherits the series'
// TMDB art (downloaded once, cached under the series).
func TestEpisodeImageFallsBackToSeriesTMDBArt(t *testing.T) {
	still := jpegBytes(t, 780, 439, color.RGBA{R: 200, G: 200, B: 30, A: 255})
	poster := jpegBytes(t, 780, 1170, color.RGBA{R: 30, G: 30, B: 200, A: 255})
	backdrop := jpegBytes(t, 1280, 720, color.RGBA{R: 200, G: 30, B: 120, A: 255})
	srv, _, artDL := fakeTVTMDB(t, still, poster, backdrop)
	fx := epFixtureSetup(t, srv.URL, false, false)

	// Episode 2 (no still) backdrop → series TMDB backdrop.
	code, body, _ := getRaw(t, fx.h, imgURL(fx.ep2ID, "backdrop"))
	if code != 200 || !bytes.Equal(body, backdrop) {
		t.Fatalf("ep2 backdrop = %d, %d bytes; want series TMDB backdrop", code, len(body))
	}
	seriesArt := filepath.Join(fx.dir, "cache", "tmdb-art", fmt.Sprintf("%d-backdrop.jpg", fx.seriesID))
	if cached, err := os.ReadFile(seriesArt); err != nil || !bytes.Equal(cached, backdrop) {
		t.Fatalf("series backdrop cache = %v, %d bytes", err, len(cached))
	}
	// Episode poster → the episode's own still (batch 18), not the
	// series' TMDB poster.
	code, body, _ = getRaw(t, fx.h, imgURL(fx.ep1ID, "poster"))
	if code != 200 || !bytes.Equal(body, still) {
		t.Fatalf("ep1 poster = %d, %d bytes; want episode still", code, len(body))
	}
	if n := atomic.LoadInt32(artDL); n != 1 {
		t.Fatalf("series art downloads = %d, want 1 (backdrop only; poster used the still)", n)
	}
	// Repeats are cache hits.
	_, _, _ = getRaw(t, fx.h, imgURL(fx.ep2ID, "backdrop"))
	_, _, _ = getRaw(t, fx.h, imgURL(fx.ep1ID, "poster"))
	if n := atomic.LoadInt32(artDL); n != 1 {
		t.Fatalf("series art downloads after repeats = %d, want 1", n)
	}
}
