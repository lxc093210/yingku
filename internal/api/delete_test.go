package api

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"yingku/internal/scan"
	"yingku/internal/store"
)

// deleteFixture builds a small on-disk library (one movie, one series
// with two episodes) in a temp dir, scans it, and returns the handler,
// store and the item ids.
type deleteFixture struct {
	h         http.Handler
	st        *store.Store
	root      string
	movieDir  string
	seriesDir string
	ep1Stem   string // episode 1 file stem (no extension)
	movieID   int64
	seriesID  int64
	seasonID  int64
	ep1ID     int64
	ep2ID     int64
}

func writeFileBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newDeleteFixture(t *testing.T) *deleteFixture {
	t.Helper()
	root := t.TempDir()
	f := &deleteFixture{root: root}

	f.movieDir = filepath.Join(root, "movies", "Test Film (2020) {tmdbid-111}")
	writeFileBytes(t, filepath.Join(f.movieDir, "film.strm"), []byte("http://example.com/film.mkv\n"))
	writeFileBytes(t, filepath.Join(f.movieDir, "film.nfo"),
		[]byte(`<?xml version="1.0"?><movie><title>Test Film</title></movie>`))
	writeFileBytes(t, filepath.Join(f.movieDir, "poster.jpg"), []byte("poster-bytes"))

	f.seriesDir = filepath.Join(root, "shows", "Test Show (2021) {tmdbid-222}")
	writeFileBytes(t, filepath.Join(f.seriesDir, "tvshow.nfo"),
		[]byte(`<?xml version="1.0"?><tvshow><title>Test Show</title></tvshow>`))
	s1 := filepath.Join(f.seriesDir, "Season 1")
	f.ep1Stem = filepath.Join(s1, "Show S01E01")
	writeFileBytes(t, f.ep1Stem+".strm", []byte("http://example.com/e1.mkv\n"))
	writeFileBytes(t, f.ep1Stem+".nfo",
		[]byte(`<?xml version="1.0"?><episodedetails><title>Ep One</title><season>1</season><episode>1</episode></episodedetails>`))
	writeFileBytes(t, f.ep1Stem+"-mediainfo.json", []byte(`[]`))
	writeFileBytes(t, f.ep1Stem+".chi.srt", []byte("1\n00:00:01,000 --> 00:00:02,000\nhi\n"))
	ep2 := filepath.Join(s1, "Show S01E02.strm")
	writeFileBytes(t, ep2, []byte("http://example.com/e2.mkv\n"))

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f.st = st
	movieLib, err := st.CreateLibrary("电影库", "movie", []string{filepath.Join(root, "movies")})
	if err != nil {
		t.Fatal(err)
	}
	seriesLib, err := st.CreateLibrary("剧集库", "series", []string{filepath.Join(root, "shows")})
	if err != nil {
		t.Fatal(err)
	}
	sc := scan.New(st)
	if err := sc.ScanLibraryByID(movieLib); err != nil {
		t.Fatal(err)
	}
	if err := sc.ScanLibraryByID(seriesLib); err != nil {
		t.Fatal(err)
	}
	f.h = New(st, sc, "")

	mustItem := func(path string) *store.Item {
		it, err := st.GetItemByPath(path)
		if err != nil || it == nil {
			t.Fatalf("item at %s: %v %+v", path, err, it)
		}
		return it
	}
	f.movieID = mustItem(f.movieDir).ID
	f.seriesID = mustItem(f.seriesDir).ID
	f.ep1ID = mustItem(f.ep1Stem + ".strm").ID
	f.ep2ID = mustItem(ep2).ID
	kids, err := st.Children(f.seriesID)
	if err != nil || len(kids) != 1 {
		t.Fatalf("series children = %v %v", kids, err)
	}
	f.seasonID = kids[0].ID
	return f
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestDeleteMovie(t *testing.T) {
	f := newDeleteFixture(t)

	var prev deletePreviewDTO
	if code := get(t, f.h, "/api/items/"+itoa(f.movieID)+"/delete-preview", &prev); code != 200 {
		t.Fatalf("preview = %d", code)
	}
	if prev.Name != "Test Film" || prev.Type != "Movie" {
		t.Errorf("preview = %+v", prev)
	}
	if len(prev.Paths) != 1 || prev.Paths[0] != f.movieDir {
		t.Errorf("preview paths = %v, want [%s]", prev.Paths, f.movieDir)
	}
	if prev.FileCount != 3 || prev.TotalBytes <= 0 {
		t.Errorf("preview counts = %d files %d bytes", prev.FileCount, prev.TotalBytes)
	}

	if code, body := doReq(t, f.h, http.MethodDelete, "/api/items/"+itoa(f.movieID), ""); code != 200 {
		t.Fatalf("delete = %d %s", code, body)
	}
	if exists(f.movieDir) {
		t.Errorf("movie folder still on disk")
	}
	if it, _ := f.st.GetItem(f.movieID); it != nil {
		t.Errorf("movie row still in DB")
	}
	if srcs, _ := f.st.SourcesOf(f.movieID); len(srcs) != 0 {
		t.Errorf("movie sources still in DB: %v", srcs)
	}
	if code := get(t, f.h, "/api/items/"+itoa(f.movieID), nil); code != 404 {
		t.Errorf("deleted movie detail = %d, want 404", code)
	}
}

func TestDeleteEpisode(t *testing.T) {
	f := newDeleteFixture(t)

	var prev deletePreviewDTO
	if code := get(t, f.h, "/api/items/"+itoa(f.ep1ID)+"/delete-preview", &prev); code != 200 {
		t.Fatalf("preview = %d", code)
	}
	if prev.Type != "Episode" || len(prev.Paths) != 4 {
		t.Fatalf("episode preview = %+v, want 4 same-stem paths", prev)
	}

	if code, body := doReq(t, f.h, http.MethodDelete, "/api/items/"+itoa(f.ep1ID), ""); code != 200 {
		t.Fatalf("delete episode = %d %s", code, body)
	}
	for _, suffix := range []string{".strm", ".nfo", "-mediainfo.json", ".chi.srt"} {
		if exists(f.ep1Stem + suffix) {
			t.Errorf("episode file %s still on disk", suffix)
		}
	}
	// Sibling episode and the series folder survive.
	if !exists(filepath.Join(f.seriesDir, "Season 1", "Show S01E02.strm")) {
		t.Errorf("episode 2 was deleted too")
	}
	if !exists(f.seriesDir) {
		t.Errorf("series folder was deleted")
	}
	if it, _ := f.st.GetItem(f.ep1ID); it != nil {
		t.Errorf("episode row still in DB")
	}
	if subs, _ := f.st.SubtitlesOf(f.ep1ID); len(subs) != 0 {
		t.Errorf("episode subtitles still in DB")
	}
	if it, _ := f.st.GetItem(f.seriesID); it == nil {
		t.Errorf("series row was deleted")
	}
}

func TestDeleteSeries(t *testing.T) {
	f := newDeleteFixture(t)

	if code, body := doReq(t, f.h, http.MethodDelete, "/api/items/"+itoa(f.seriesID), ""); code != 200 {
		t.Fatalf("delete series = %d %s", code, body)
	}
	if exists(f.seriesDir) {
		t.Errorf("series folder still on disk")
	}
	for _, id := range []int64{f.seriesID, f.seasonID, f.ep1ID, f.ep2ID} {
		if it, _ := f.st.GetItem(id); it != nil {
			t.Errorf("item %d still in DB after series delete", id)
		}
	}
	if srcs, _ := f.st.SourcesOf(f.ep2ID); len(srcs) != 0 {
		t.Errorf("episode sources still in DB")
	}
}

func TestDeleteSeasonRejected(t *testing.T) {
	f := newDeleteFixture(t)
	if code := get(t, f.h, "/api/items/"+itoa(f.seasonID)+"/delete-preview", nil); code != 400 {
		t.Errorf("season preview = %d, want 400", code)
	}
	if code, _ := doReq(t, f.h, http.MethodDelete, "/api/items/"+itoa(f.seasonID), ""); code != 400 {
		t.Errorf("season delete = %d, want 400", code)
	}
	if !exists(f.seriesDir) {
		t.Errorf("series folder touched by rejected season delete")
	}
}

func TestDeleteOutsideLibraryForbidden(t *testing.T) {
	f := newDeleteFixture(t)

	// An item whose path was moved outside every library root (as if the
	// row was hand-edited): DELETE must refuse and keep the files.
	outside := filepath.Join(t.TempDir(), "outside")
	keep := filepath.Join(outside, "keep.txt")
	writeFileBytes(t, keep, []byte("precious"))
	libs, err := f.st.ListLibraries()
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.st.Begin()
	if err != nil {
		t.Fatal(err)
	}
	evilID, err := tx.UpsertItem(&store.Item{
		LibraryID: libs[0].ID, Type: "movie", Path: outside,
		Title: "Evil", SortTitle: "Evil",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if code, body := doReq(t, f.h, http.MethodDelete, "/api/items/"+itoa(evilID), ""); code != 403 {
		t.Fatalf("outside delete = %d %s, want 403", code, body)
	}
	if !exists(keep) {
		t.Errorf("outside file was deleted despite 403")
	}
	if it, _ := f.st.GetItem(evilID); it == nil {
		t.Errorf("outside item row was deleted despite 403")
	}
}
