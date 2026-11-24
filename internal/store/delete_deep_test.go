package store

import (
	"testing"
)

// buildDeepTree creates series -> season -> episode plus a movie,
// each with a source, an image, a subtitle, a person and user data.
func buildDeepTree(t *testing.T, st *Store, libID int64) (seriesID, seasonID, epID, movieID int64) {
	t.Helper()
	tx, err := st.Begin()
	if err != nil {
		t.Fatal(err)
	}
	seriesID, err = tx.UpsertItem(&Item{LibraryID: libID, Type: "series", Path: "/d/剧集", Title: "深删剧集", SortTitle: "深删剧集"})
	if err != nil {
		t.Fatal(err)
	}
	one := 1
	seasonID, err = tx.UpsertItem(&Item{LibraryID: libID, Type: "season", Path: "/d/剧集/Season 1", Title: "第 1 季", SortTitle: "第 1 季", ParentID: &seriesID, SeriesID: &seriesID, SeasonNum: &one})
	if err != nil {
		t.Fatal(err)
	}
	epNum := 1
	epID, err = tx.UpsertItem(&Item{LibraryID: libID, Type: "episode", Path: "/d/剧集/Season 1/e1.strm", Title: "第一集", SortTitle: "第一集", ParentID: &seasonID, SeriesID: &seriesID, SeasonNum: &one, EpisodeNum: &epNum})
	if err != nil {
		t.Fatal(err)
	}
	movieID, err = tx.UpsertItem(&Item{LibraryID: libID, Type: "movie", Path: "/d/电影", Title: "深删电影", SortTitle: "深删电影"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{seriesID, seasonID, epID, movieID} {
		if err := tx.ReplaceSources(id, []MediaSource{{StrmPath: "/d/x.strm", TargetURL: "http://x/y", MediaInfoSource: "json"}}); err != nil {
			t.Fatal(err)
		}
		if err := tx.ReplaceImages(id, map[string]ImageFile{"poster": {Path: "/d/poster.jpg", Width: 10, Height: 10}}); err != nil {
			t.Fatal(err)
		}
		if err := tx.ReplaceSubtitles(id, []Subtitle{{Path: "/d/x.srt", Lang: "zh"}}); err != nil {
			t.Fatal(err)
		}
		if err := tx.ReplacePeople(id, []Person{{Name: "演员甲", Kind: "Actor", Ord: 0}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{seriesID, epID, movieID} {
		if err := st.SetFavorite(id, "u1", true); err != nil {
			t.Fatal(err)
		}
	}
	return
}

func countRows(t *testing.T, st *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func assertDeepGone(t *testing.T, st *Store, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		if it, _ := st.GetItem(id); it != nil {
			t.Errorf("item %d still present", id)
		}
		for _, table := range []string{"media_sources", "images", "subtitles", "people", "user_data"} {
			if n := countRows(t, st, `SELECT COUNT(*) FROM `+table+` WHERE item_id = ?`, id); n != 0 {
				t.Errorf("%s rows for item %d = %d, want 0", table, id, n)
			}
		}
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM items_fts WHERE items_fts MATCH '深删'`); n != 0 {
		t.Errorf("fts rows matching 深删 = %d, want 0", n)
	}
}

func TestDeleteItemDeep(t *testing.T) {
	st := openTempStore(t)
	libID, err := st.CreateLibrary("深删库", "mixed", []string{"/d"})
	if err != nil {
		t.Fatal(err)
	}
	seriesID, seasonID, epID, movieID := buildDeepTree(t, st, libID)

	removed, err := st.DeleteItemDeep(seriesID)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 3 {
		t.Fatalf("removed = %v, want series+season+episode", removed)
	}
	assertDeepGone(t, st, seriesID, seasonID, epID)
	// The unrelated movie is untouched.
	if it, _ := st.GetItem(movieID); it == nil {
		t.Fatalf("movie wrongly deleted")
	}
	if srcs, _ := st.SourcesOf(movieID); len(srcs) != 1 {
		t.Errorf("movie sources = %v", srcs)
	}
	// Deleting a missing item returns no ids, no error.
	removed, err = st.DeleteItemDeep(99999)
	if err != nil || len(removed) != 0 {
		t.Errorf("delete missing = %v, %v", removed, err)
	}
	// DeleteItem keeps its ErrNoRows contract.
	if err := st.DeleteItem(99999); err == nil {
		t.Errorf("DeleteItem missing: want error")
	}
	if err := st.DeleteItem(movieID); err != nil {
		t.Fatal(err)
	}
	assertDeepGone(t, st, movieID)
}

func TestDeleteVanishedDeep(t *testing.T) {
	st := openTempStore(t)
	libID, err := st.CreateLibrary("剪除库", "mixed", []string{"/d"})
	if err != nil {
		t.Fatal(err)
	}
	seriesID, seasonID, epID, movieID := buildDeepTree(t, st, libID)
	seen := map[string]struct{}{"/d/电影": {}}
	n, removed, err := st.DeleteVanishedDeep(libID, seen)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 || len(removed) != 3 {
		t.Fatalf("pruned = %d (%v), want 3", n, removed)
	}
	assertDeepGone(t, st, seriesID, seasonID, epID)
	if it, _ := st.GetItem(movieID); it == nil {
		t.Errorf("kept movie was pruned")
	}
}

func TestDeleteLibraryDeep(t *testing.T) {
	st := openTempStore(t)
	libID, err := st.CreateLibrary("整库删", "mixed", []string{"/d"})
	if err != nil {
		t.Fatal(err)
	}
	seriesID, seasonID, epID, movieID := buildDeepTree(t, st, libID)
	removed, err := st.DeleteLibraryDeep(libID)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 4 {
		t.Fatalf("removed = %v, want 4 items", removed)
	}
	assertDeepGone(t, st, seriesID, seasonID, epID, movieID)
	if lib, _ := st.GetLibrary(libID); lib != nil {
		t.Errorf("library still present")
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM library_paths WHERE library_id = ?`, libID); n != 0 {
		t.Errorf("library_paths rows = %d, want 0", n)
	}
	if _, err := st.DeleteLibraryDeep(libID); err == nil {
		t.Errorf("second DeleteLibraryDeep: want ErrNoRows")
	}
}
