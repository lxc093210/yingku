package scan

import (
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"yingku/internal/store"
)

func fixturesRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "..", "fixtures", "library")
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("fixtures missing: %v", err)
	}
	abs, _ := filepath.Abs(root)
	return abs
}

func openTemp(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestScanFixtures(t *testing.T) {
	st := openTemp(t)
	sc := New(st)
	root := fixturesRoot(t)
	if err := sc.Scan(root); err != nil {
		t.Fatal(err)
	}
	stats, err := st.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Libraries) != 2 {
		t.Fatalf("libraries = %d, want 2", len(stats.Libraries))
	}
	if stats.TotalMovies != 1 || stats.TotalSeries != 1 {
		t.Fatalf("movies/series = %d/%d", stats.TotalMovies, stats.TotalSeries)
	}
	if stats.TotalEpisodes != 20 {
		t.Fatalf("episodes = %d, want 20", stats.TotalEpisodes)
	}
	if stats.MediaInfoSource["json"] != 21 {
		t.Fatalf("mediainfo json count = %d, want 21", stats.MediaInfoSource["json"])
	}

	// Movie assertions.
	libs, _ := st.ListLibraries()
	var movieLib, seriesLib *store.Library
	for i := range libs {
		if libs[i].Kind == "movie" {
			movieLib = &libs[i]
		} else {
			seriesLib = &libs[i]
		}
	}
	items, _, err := st.ListTopLevel(movieLib.ID, "title", "", 60)
	if err != nil || len(items) != 1 {
		t.Fatalf("movie items = %v, err %v", len(items), err)
	}
	movie := items[0]
	if movie.TmdbID != 324857 {
		t.Errorf("movie tmdb = %d, want 324857", movie.TmdbID)
	}
	if movie.Title != "蜘蛛侠：平行宇宙" || movie.Rating != 8.6 {
		t.Errorf("movie title/rating = %q/%v", movie.Title, movie.Rating)
	}
	if !movie.HasPoster || !movie.HasBackdrop {
		t.Errorf("movie images poster/backdrop = %v/%v", movie.HasPoster, movie.HasBackdrop)
	}
	srcs, _ := st.SourcesOf(movie.ID)
	if len(srcs) != 1 {
		t.Fatalf("movie sources = %d", len(srcs))
	}
	if srcs[0].Width != 3840 || srcs[0].MediaInfoSource != "json" {
		t.Errorf("movie source width/source = %d/%q", srcs[0].Width, srcs[0].MediaInfoSource)
	}
	if srcs[0].TargetURL == "" || srcs[0].TargetURL[:4] != "http" {
		t.Errorf("movie target url = %q", srcs[0].TargetURL)
	}
	subs, _ := st.SubtitlesOf(movie.ID)
	if len(subs) != 3 {
		t.Errorf("movie subtitles = %d, want 3", len(subs))
	}

	// Series assertions.
	sitems, _, err := st.ListTopLevel(seriesLib.ID, "title", "", 60)
	if err != nil || len(sitems) != 1 {
		t.Fatalf("series items = %v, err %v", len(sitems), err)
	}
	series := sitems[0]
	if series.TmdbID != 80607 {
		t.Errorf("series tmdb = %d, want 80607", series.TmdbID)
	}
	seasons, _ := st.Children(series.ID)
	if len(seasons) != 1 {
		t.Fatalf("seasons = %d", len(seasons))
	}
	eps, _ := st.Children(seasons[0].ID)
	if len(eps) != 20 {
		t.Fatalf("episodes under season = %d, want 20", len(eps))
	}
	ep9 := eps[8]
	if ep9.EpisodeNum == nil || *ep9.EpisodeNum != 9 {
		t.Errorf("eps[8] episode_num = %v", ep9.EpisodeNum)
	}
	epSrcs, _ := st.SourcesOf(ep9.ID)
	if len(epSrcs) != 1 || epSrcs[0].Width != 3840 {
		t.Errorf("ep9 sources = %+v", epSrcs)
	}

	// Incremental rescan: everything skipped, nothing rewritten.
	sc2 := New(st)
	if err := sc2.Scan(root); err != nil {
		t.Fatal(err)
	}
	st2 := sc2.Status()
	if st2.Items != 0 || st2.Skipped != 23 {
		t.Errorf("rescan items/skipped = %d/%d, want 0/23", st2.Items, st2.Skipped)
	}
}

func TestScanVanishedDeleted(t *testing.T) {
	// Copy fixtures to a temp tree, scan, remove one episode, rescan.
	src := fixturesRoot(t)
	dst := filepath.Join(t.TempDir(), "library")
	if err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o644)
	}); err != nil {
		t.Fatal(err)
	}
	st := openTemp(t)
	sc := New(st)
	if err := sc.Scan(dst); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(dst, "电视剧", "国产剧", "上海女子图鉴 (2018) {tmdbid-80607}", "Season 1",
		"Women of Shanghai.2018.S01E09.WEB-DL.2160p.H265.AAC.10bit.25fps-LeagueWEB.strm")
	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}
	if err := sc.Scan(dst); err != nil {
		t.Fatal(err)
	}
	stats, _ := st.Stats()
	if stats.TotalEpisodes != 19 {
		t.Errorf("episodes after delete = %d, want 19", stats.TotalEpisodes)
	}
}

func TestScanMultiPathMixedLibrary(t *testing.T) {
	st := openTemp(t)
	root := fixturesRoot(t)
	movieRoot := filepath.Join(root, "电影", "动画电影")
	seriesRoot := filepath.Join(root, "电视剧", "国产剧")
	id, err := st.CreateLibrary("混合库", "mixed", []string{movieRoot, seriesRoot})
	if err != nil {
		t.Fatal(err)
	}
	sc := New(st)
	if err := sc.ScanLibraryByID(id); err != nil {
		t.Fatal(err)
	}
	stats, err := st.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalMovies != 1 || stats.TotalSeries != 1 || stats.TotalEpisodes != 20 {
		t.Fatalf("movies/series/episodes = %d/%d/%d, want 1/1/20",
			stats.TotalMovies, stats.TotalSeries, stats.TotalEpisodes)
	}
	if stats.MediaInfoSource["json"] != 21 {
		t.Fatalf("mediainfo json = %d, want 21", stats.MediaInfoSource["json"])
	}
	items, _, err := st.ListTopLevel(id, "title", "", 60)
	if err != nil || len(items) != 2 {
		t.Fatalf("top-level items = %v, err %v", len(items), err)
	}
	for _, it := range items {
		if it.LibraryID != id {
			t.Errorf("item %q library_id = %d, want %d", it.Title, it.LibraryID, id)
		}
	}
	// Per-library rescan is incremental too.
	sc2 := New(st)
	if err := sc2.ScanLibraryByID(id); err != nil {
		t.Fatal(err)
	}
	if got := sc2.Status(); got.Items != 0 || got.Skipped != 23 {
		t.Errorf("rescan items/skipped = %d/%d, want 0/23", got.Items, got.Skipped)
	}
}

func TestScanLongestPrefixOwnership(t *testing.T) {
	base := t.TempDir()
	mkMovie := func(dir string) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "film.strm"), []byte("http://example.com/f.mkv\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	filmA := filepath.Join(base, "FilmA (2020) {tmdbid-1}")
	sub := filepath.Join(base, "Sub")
	filmB := filepath.Join(sub, "FilmB (2021) {tmdbid-2}")
	mkMovie(filmA)
	mkMovie(filmB)

	st := openTemp(t)
	outerID, err := st.CreateLibrary("外层", "movie", []string{base})
	if err != nil {
		t.Fatal(err)
	}
	innerID, err := st.CreateLibrary("内层", "movie", []string{sub})
	if err != nil {
		t.Fatal(err)
	}
	sc := New(st)
	if err := sc.ScanAll(""); err != nil {
		t.Fatal(err)
	}
	a, err := st.GetItemByPath(filmA)
	if err != nil || a == nil {
		t.Fatalf("filmA = %+v, err %v", a, err)
	}
	if a.LibraryID != outerID {
		t.Errorf("filmA library = %d, want outer %d", a.LibraryID, outerID)
	}
	b, err := st.GetItemByPath(filmB)
	if err != nil || b == nil {
		t.Fatalf("filmB = %+v, err %v", b, err)
	}
	if b.LibraryID != innerID {
		t.Errorf("filmB library = %d, want inner %d (longest prefix)", b.LibraryID, innerID)
	}
	// Scanning the outer library alone must not delete the inner item.
	if err := sc.ScanLibraryByID(outerID); err != nil {
		t.Fatal(err)
	}
	if b, _ := st.GetItemByPath(filmB); b == nil {
		t.Errorf("filmB vanished after outer-only scan")
	}
}

// writeJPEG renders a solid-color JPEG of the given pixel size.
func writeJPEG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
}

// writePNG renders a solid-color PNG of the given pixel size.
func writePNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

func TestImageDims(t *testing.T) {
	dir := t.TempDir()
	jpg := filepath.Join(dir, "a.jpg")
	writeJPEG(t, jpg, 64, 48)
	if w, h, ok := imageDims(jpg); !ok || w != 64 || h != 48 {
		t.Errorf("jpeg dims = %dx%d ok=%v, want 64x48", w, h, ok)
	}
	pngPath := filepath.Join(dir, "a.png")
	writePNG(t, pngPath, 30, 17)
	if w, h, ok := imageDims(pngPath); !ok || w != 30 || h != 17 {
		t.Errorf("png dims = %dx%d ok=%v, want 30x17", w, h, ok)
	}
	bogus := filepath.Join(dir, "b.jpg")
	if err := os.WriteFile(bogus, []byte("not an image at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := imageDims(bogus); ok {
		t.Errorf("bogus file reported dims")
	}
	// Unreadable dimensions fall back to byte size (non-zero here).
	if imageScore(bogus) != int64(len("not an image at all")) {
		t.Errorf("fallback score = %d", imageScore(bogus))
	}
}

// TestFolderImagesPrefersLargest mirrors the NAS finding: a small
// backdrop.jpg next to a full-size background.jpg must lose.
func TestFolderImagesPrefersLargest(t *testing.T) {
	dir := t.TempDir()
	backdrop := filepath.Join(dir, "backdrop.jpg")
	background := filepath.Join(dir, "background.jpg")
	fanart := filepath.Join(dir, "fanart.jpg")
	poster := filepath.Join(dir, "poster.jpg")
	cover := filepath.Join(dir, "cover.png")
	writeJPEG(t, backdrop, 8, 5)
	writeJPEG(t, background, 128, 72)
	writeJPEG(t, fanart, 16, 9)
	writeJPEG(t, poster, 10, 15)
	writePNG(t, cover, 40, 60)

	imgs := folderImages([]string{backdrop, background, fanart, poster, cover})
	if imgs["backdrop"] != background {
		t.Errorf("backdrop = %q, want the larger background.jpg", imgs["backdrop"])
	}
	if imgs["poster"] != cover {
		t.Errorf("poster = %q, want the larger cover.png", imgs["poster"])
	}
}

func TestReadStrm(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.strm")
	if err := os.WriteFile(p, []byte("http://example.com/x.mkv\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readStrm(p); got != "http://example.com/x.mkv" {
		t.Errorf("readStrm = %q", got)
	}
}

// writeFile is a small fixture helper for the item-folder-root tests.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestScanItemFolderRoots mirrors the NAS trial failure: a mixed library
// whose roots point at one movie's folder and one series' folder
// themselves (not at containers). The movie must be found, and the series
// must assemble as series+season+episodes instead of one bogus "movie"
// built out of an episode.
func TestScanItemFolderRoots(t *testing.T) {
	base := t.TempDir()
	movieDir := filepath.Join(base, "测试电影 (2020) {tmdbid-111}")
	writeFile(t, filepath.Join(movieDir, "测试电影.2020.1080p.strm"), "http://example.com/m.mkv\n")
	writeFile(t, filepath.Join(movieDir, "测试电影.2020.1080p.nfo"),
		"<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<movie>\n  <title>测试电影</title>\n  <year>2020</year>\n  <rating>7.5</rating>\n  <plot>测试剧情</plot>\n</movie>\n")
	writeFile(t, filepath.Join(movieDir, "poster.jpg"), "fake-jpeg")

	seriesDir := filepath.Join(base, "测试剧集 (2021) {tmdbid-222}")
	writeFile(t, filepath.Join(seriesDir, "tvshow.nfo"),
		"<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<tvshow>\n  <title>测试剧集</title>\n  <year>2021</year>\n  <tmdbid>222</tmdbid>\n</tvshow>\n")
	writeFile(t, filepath.Join(seriesDir, "poster.jpg"), "fake-jpeg")
	seasonDir := filepath.Join(seriesDir, "Season 1")
	writeFile(t, filepath.Join(seasonDir, "Show.S01E01.strm"), "http://example.com/e1.mkv\n")
	writeFile(t, filepath.Join(seasonDir, "Show.S01E01.nfo"),
		"<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<episodedetails>\n  <title>第一集标题</title>\n  <season>1</season>\n  <episode>1</episode>\n</episodedetails>\n")
	writeFile(t, filepath.Join(seasonDir, "Show.S01E02.strm"), "http://example.com/e2.mkv\n")
	writeFile(t, filepath.Join(seasonDir, "Show.S01E02.nfo"),
		"<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<episodedetails>\n  <title>第二集标题</title>\n  <season>1</season>\n  <episode>2</episode>\n</episodedetails>\n")

	st := openTemp(t)
	id, err := st.CreateLibrary("条目根库", "mixed", []string{movieDir, seriesDir})
	if err != nil {
		t.Fatal(err)
	}
	sc := New(st)
	if err := sc.ScanLibraryByID(id); err != nil {
		t.Fatal(err)
	}
	stats, err := st.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalMovies != 1 || stats.TotalSeries != 1 || stats.TotalEpisodes != 2 {
		t.Fatalf("movies/series/episodes = %d/%d/%d, want 1/1/2",
			stats.TotalMovies, stats.TotalSeries, stats.TotalEpisodes)
	}

	// Exactly two top-level items: the movie and the series. In
	// particular there must be no bogus movie assembled from an episode.
	items, _, err := st.ListTopLevel(id, "title", "", 60)
	if err != nil || len(items) != 2 {
		t.Fatalf("top-level items = %v, err %v", len(items), err)
	}
	movie, err := st.GetItemByPath(movieDir)
	if err != nil || movie == nil {
		t.Fatalf("movie by path = %+v, err %v", movie, err)
	}
	if movie.Type != "movie" || movie.TmdbID != 111 || movie.Title != "测试电影" {
		t.Errorf("movie = type %q tmdb %d title %q", movie.Type, movie.TmdbID, movie.Title)
	}
	for _, it := range items {
		if it.Path == movieDir && !it.HasPoster {
			t.Errorf("movie root item has no poster registered")
		}
	}

	series, err := st.GetItemByPath(seriesDir)
	if err != nil || series == nil {
		t.Fatalf("series by path = %+v, err %v", series, err)
	}
	if series.Type != "series" || series.TmdbID != 222 || series.Title != "测试剧集" {
		t.Errorf("series = type %q tmdb %d title %q", series.Type, series.TmdbID, series.Title)
	}
	seasons, _ := st.Children(series.ID)
	if len(seasons) != 1 {
		t.Fatalf("seasons = %d, want 1", len(seasons))
	}
	eps, _ := st.Children(seasons[0].ID)
	if len(eps) != 2 {
		t.Fatalf("episodes = %d, want 2", len(eps))
	}
	for i, ep := range eps {
		if ep.Type != "episode" {
			t.Errorf("ep %d type = %q, want episode", i, ep.Type)
		}
		if ep.EpisodeNum == nil || *ep.EpisodeNum != i+1 {
			t.Errorf("ep %d episode_num = %v, want %d", i, ep.EpisodeNum, i+1)
		}
	}
	if eps[0].Title != "第一集标题" || eps[1].Title != "第二集标题" {
		t.Errorf("episode titles = %q / %q", eps[0].Title, eps[1].Title)
	}

	// Incremental rescan: movie + series + season + 2 episodes skipped.
	sc2 := New(st)
	if err := sc2.ScanLibraryByID(id); err != nil {
		t.Fatal(err)
	}
	if got := sc2.Status(); got.Items != 0 || got.Skipped != 5 {
		t.Errorf("rescan items/skipped = %d/%d, want 0/5", got.Items, got.Skipped)
	}
}

// TestScanSeriesFolderRootNoTvNFO covers the second half of the series
// classification: no tvshow.nfo, but a Season dir holding .strm files.
func TestScanSeriesFolderRootNoTvNFO(t *testing.T) {
	base := t.TempDir()
	seriesDir := filepath.Join(base, "无NFO剧集 (2022) {tmdbid-333}")
	writeFile(t, filepath.Join(seriesDir, "Season 1", "Show2.S01E01.strm"), "http://example.com/x1.mkv\n")

	st := openTemp(t)
	id, err := st.CreateLibrary("无NFO库", "series", []string{seriesDir})
	if err != nil {
		t.Fatal(err)
	}
	sc := New(st)
	if err := sc.ScanLibraryByID(id); err != nil {
		t.Fatal(err)
	}
	stats, _ := st.Stats()
	if stats.TotalMovies != 0 || stats.TotalSeries != 1 || stats.TotalEpisodes != 1 {
		t.Fatalf("movies/series/episodes = %d/%d/%d, want 0/1/1",
			stats.TotalMovies, stats.TotalSeries, stats.TotalEpisodes)
	}
	series, err := st.GetItemByPath(seriesDir)
	if err != nil || series == nil {
		t.Fatalf("series by path = %+v, err %v", series, err)
	}
	if series.Type != "series" || series.TmdbID != 333 || series.Title != "无NFO剧集" || series.Year != 2022 {
		t.Errorf("series = type %q tmdb %d title %q year %d",
			series.Type, series.TmdbID, series.Title, series.Year)
	}
}
