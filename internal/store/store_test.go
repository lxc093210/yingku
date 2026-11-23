package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func openTempStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestLibraryCRUD(t *testing.T) {
	st := openTempStore(t)

	id, err := st.CreateLibrary("测试库", "mixed", []string{"/data/a", "/data/b"})
	if err != nil {
		t.Fatal(err)
	}
	lib, err := st.GetLibrary(id)
	if err != nil || lib == nil {
		t.Fatalf("get created library: %v %+v", err, lib)
	}
	if lib.Name != "测试库" || lib.Kind != "mixed" || len(lib.Paths) != 2 {
		t.Fatalf("library = %+v", lib)
	}

	// A path owned by another library is rejected.
	if _, err := st.CreateLibrary("冲突库", "movie", []string{"/data/a"}); !errors.Is(err, ErrPathOwned) {
		t.Fatalf("create with owned path err = %v, want ErrPathOwned", err)
	}

	// Update replaces name/kind/paths.
	if err := st.UpdateLibrary(id, "改名库", "movie", []string{"/data/c"}); err != nil {
		t.Fatal(err)
	}
	lib, _ = st.GetLibrary(id)
	if lib.Name != "改名库" || lib.Kind != "movie" || len(lib.Paths) != 1 || lib.Paths[0] != "/data/c" {
		t.Fatalf("after update = %+v", lib)
	}
	// The freed path can be reused.
	if _, err := st.CreateLibrary("新库", "series", []string{"/data/a"}); err != nil {
		t.Fatalf("reuse freed path: %v", err)
	}
	// Updating a missing library reports ErrNoRows.
	if err := st.UpdateLibrary(9999, "x", "movie", []string{"/data/z"}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("update missing err = %v", err)
	}

	// Delete removes the library and cascades to its items.
	tx, err := st.Begin()
	if err != nil {
		t.Fatal(err)
	}
	itemID, err := tx.UpsertItem(&Item{
		LibraryID: id, Type: "movie", Path: "/data/c/Film (2020)",
		Title: "Film", SortTitle: "Film",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.ReplaceSources(itemID, []MediaSource{{StrmPath: "/data/c/Film (2020)/f.strm", TargetURL: "http://x/y"}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteLibrary(id); err != nil {
		t.Fatal(err)
	}
	if lib, _ := st.GetLibrary(id); lib != nil {
		t.Errorf("library still present after delete")
	}
	if it, _ := st.GetItem(itemID); it != nil {
		t.Errorf("item still present after library delete")
	}
	if srcs, _ := st.SourcesOf(itemID); len(srcs) != 0 {
		t.Errorf("sources still present after library delete: %v", srcs)
	}
	if err := st.DeleteLibrary(id); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("second delete err = %v, want ErrNoRows", err)
	}
}

func insertRatedItem(t *testing.T, st *Store, libID int64, path string, rating float64, backdrop bool) int64 {
	t.Helper()
	tx, err := st.Begin()
	if err != nil {
		t.Fatal(err)
	}
	id, err := tx.UpsertItem(&Item{
		LibraryID: libID, Type: "movie", Path: path,
		Title: path, SortTitle: path, Rating: rating,
	})
	if err != nil {
		t.Fatal(err)
	}
	if backdrop {
		if err := tx.ReplaceImages(id, map[string]ImageFile{"backdrop": {Path: "/img/backdrop.jpg"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestHeroesRatingFilter(t *testing.T) {
	st := openTempStore(t)
	libID, err := st.CreateLibrary("评分库", "movie", []string{"/rated"})
	if err != nil {
		t.Fatal(err)
	}
	insertRatedItem(t, st, libID, "/rated/fake10", 10.0, true)
	insertRatedItem(t, st, libID, "/rated/unrated", 0.0, true)
	good := insertRatedItem(t, st, libID, "/rated/good85", 8.5, true)
	good2 := insertRatedItem(t, st, libID, "/rated/good70", 7.0, true)
	insertRatedItem(t, st, libID, "/rated/edge995", 9.95, true)
	insertRatedItem(t, st, libID, "/rated/low64", 6.4, true)
	insertRatedItem(t, st, libID, "/rated/nobd", 8.8, false)

	heroes, err := st.Heroes(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(heroes) != 2 || heroes[0].ID != good || heroes[1].ID != good2 {
		var got []int64
		for _, h := range heroes {
			got = append(got, h.ID)
		}
		t.Fatalf("heroes = %v, want [%d %d] (only plausible ratings with backdrop)", got, good, good2)
	}
}

func insertHeroItem(t *testing.T, st *Store, libID int64, path string, rating float64, backdropWidth int) int64 {
	t.Helper()
	tx, err := st.Begin()
	if err != nil {
		t.Fatal(err)
	}
	id, err := tx.UpsertItem(&Item{
		LibraryID: libID, Type: "movie", Path: path,
		Title: path, SortTitle: path, Rating: rating,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.ReplaceImages(id, map[string]ImageFile{
		"backdrop": {Path: "/img/" + path + ".jpg", Width: backdropWidth, Height: backdropWidth * 9 / 16},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestHeroesBackdropWidthTiers(t *testing.T) {
	st := openTempStore(t)
	libID, err := st.CreateLibrary("主打库", "movie", []string{"/heroes"})
	if err != nil {
		t.Fatal(err)
	}
	w1920 := insertHeroItem(t, st, libID, "/heroes/fullhd", 7.0, 1920)
	w1280 := insertHeroItem(t, st, libID, "/heroes/hd", 9.0, 1280)
	w800 := insertHeroItem(t, st, libID, "/heroes/sd", 9.5, 800)
	// Unknown width shares the lowest tier with sub-1280 widths; inside
	// the tier rating decides, so this one ranks below the 800px item.
	w0 := insertHeroItem(t, st, libID, "/heroes/unknown", 9.0, 0)

	heroes, err := st.Heroes(10)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{w1920, w1280, w800, w0}
	if len(heroes) != len(want) {
		t.Fatalf("heroes = %d, want %d", len(heroes), len(want))
	}
	for i, id := range want {
		if heroes[i].ID != id {
			t.Errorf("hero[%d] = %d, want %d (width tier before rating)", i, heroes[i].ID, id)
		}
	}
}

func TestImagesDimensionMigration(t *testing.T) {
	// Simulate a pre-dimensions database: images without width/height.
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE images (
		id INTEGER PRIMARY KEY, item_id INTEGER NOT NULL,
		kind TEXT NOT NULL, local_path TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, col := range []string{"width", "height"} {
		has, err := columnExists(st.DB(), "images", col)
		if err != nil {
			t.Fatal(err)
		}
		if !has {
			t.Errorf("images.%s missing after migration", col)
		}
	}
	// Reopening stays clean (idempotent).
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st2.Close()
}

func TestRatingSortSinksBogusScores(t *testing.T) {
	st := openTempStore(t)
	libID, err := st.CreateLibrary("排序库", "movie", []string{"/sort"})
	if err != nil {
		t.Fatal(err)
	}
	// Insertion order deliberately scrambles the expected output order.
	insertRatedItem(t, st, libID, "/sort/fake10", 10.0, false)
	insertRatedItem(t, st, libID, "/sort/zero", 0.0, false)
	insertRatedItem(t, st, libID, "/sort/g85", 8.5, false)
	insertRatedItem(t, st, libID, "/sort/g70", 7.0, false)
	insertRatedItem(t, st, libID, "/sort/g88", 8.8, false)
	insertRatedItem(t, st, libID, "/sort/e995", 9.95, false)
	insertRatedItem(t, st, libID, "/sort/l64", 6.4, false)

	want := []float64{8.8, 8.5, 7.0, 10.0, 9.95, 6.4, 0.0}
	var got []float64
	cursor := ""
	for {
		items, next, err := st.ListTopLevel(libID, "rating", cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			got = append(got, it.Rating)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(got) != len(want) {
		t.Fatalf("ratings paged = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ratings paged = %v, want %v", got, want)
		}
	}
}

func TestPeopleSettingsAndUserData(t *testing.T) {
	st := openTempStore(t)
	libID, err := st.CreateLibrary("数据库", "mixed", []string{"/pd"})
	if err != nil {
		t.Fatal(err)
	}
	// Settings roundtrip; empty value clears.
	if v, _ := st.GetSetting("tmdb_api_key"); v != "" {
		t.Fatalf("unset setting = %q", v)
	}
	if err := st.SetSetting("tmdb_api_key", "secret"); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.GetSetting("tmdb_api_key"); v != "secret" {
		t.Fatalf("setting = %q", v)
	}
	if err := st.SetSetting("tmdb_api_key", ""); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.GetSetting("tmdb_api_key"); v != "" {
		t.Fatalf("cleared setting = %q", v)
	}

	tx, err := st.Begin()
	if err != nil {
		t.Fatal(err)
	}
	seriesID, err := tx.UpsertItem(&Item{
		LibraryID: libID, Type: "series", Path: "/pd/show",
		Title: "某剧", SortTitle: "某剧",
	})
	if err != nil {
		t.Fatal(err)
	}
	sn, en := 1, 3
	epID, err := tx.UpsertItem(&Item{
		LibraryID: libID, Type: "episode", Path: "/pd/show/e3.strm",
		Title: "第三集", SortTitle: "第三集",
		SeriesID: &seriesID, SeasonNum: &sn, EpisodeNum: &en,
	})
	if err != nil {
		t.Fatal(err)
	}
	movieID, err := tx.UpsertItem(&Item{
		LibraryID: libID, Type: "movie", Path: "/pd/film",
		Title: "某片", SortTitle: "某片",
	})
	if err != nil {
		t.Fatal(err)
	}
	// People: wholesale replace semantics.
	if err := tx.ReplacePeople(movieID, []Person{
		{Name: "甲", Role: "主角", Kind: "Actor", Ord: 0},
		{Name: "乙", Role: "配角", Kind: "Actor", Thumb: "https://x/y.jpg", Ord: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	people, err := st.PeopleOf(movieID)
	if err != nil || len(people) != 2 || people[0].Name != "甲" || people[1].Thumb != "https://x/y.jpg" {
		t.Fatalf("people = %+v err %v", people, err)
	}
	if err := st.ReplacePeople(movieID, []Person{{Name: "丙", Kind: "Actor", Ord: 0}}); err != nil {
		t.Fatal(err)
	}
	if people, _ := st.PeopleOf(movieID); len(people) != 1 || people[0].Name != "丙" {
		t.Fatalf("people after replace = %+v", people)
	}

	// Progress -> ContinueWatching (with series name) -> played drops it.
	if err := st.UpsertProgress(epID, 1, 500_000_000, false, 1700); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertProgress(movieID, 1, 900_000_000, false, 1800); err != nil {
		t.Fatal(err)
	}
	cont, err := st.ContinueWatching(1, 12)
	if err != nil || len(cont) != 2 {
		t.Fatalf("continue watching = %+v err %v", cont, err)
	}
	// Most recently played first: the movie (1800) precedes the episode.
	if cont[0].Item.ID != movieID || cont[1].Item.ID != epID {
		t.Fatalf("continue order = %d, %d", cont[0].Item.ID, cont[1].Item.ID)
	}
	if cont[1].SeriesName != "某剧" || cont[1].ProgressTicks != 500_000_000 {
		t.Errorf("episode entry = %+v", cont[1])
	}
	if err := st.UpsertProgress(epID, 1, 3_500_000_000, true, 1900); err != nil {
		t.Fatal(err)
	}
	if cont, _ := st.ContinueWatching(1, 12); len(cont) != 1 || cont[0].Item.ID != movieID {
		t.Fatalf("continue after played = %+v", cont)
	}
	ud, err := st.UserDataOf(epID, 1)
	if err != nil || !ud.Played || ud.ProgressTicks != 3_500_000_000 {
		t.Errorf("user data = %+v err %v", ud, err)
	}
	// Reset clears progress but keeps the row readable.
	if err := st.ResetProgress(epID, 1); err != nil {
		t.Fatal(err)
	}
	if ud, _ := st.UserDataOf(epID, 1); ud.Played || ud.ProgressTicks != 0 {
		t.Errorf("user data after reset = %+v", ud)
	}
	// Favorite is independent of progress.
	if err := st.SetFavorite(movieID, 1, true); err != nil {
		t.Fatal(err)
	}
	if ud, _ := st.UserDataOf(movieID, 1); !ud.Favorite || ud.ProgressTicks != 900_000_000 {
		t.Errorf("movie user data = %+v", ud)
	}
}

func TestLibraryPathsMigration(t *testing.T) {
	// Simulate a phase-1 database: a library row whose path exists only in
	// the legacy libraries.path column.
	path := filepath.Join(t.TempDir(), "legacy.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO libraries(name, kind, path) VALUES ('旧库', 'movie', '/legacy/root')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DELETE FROM library_paths`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	libs, err := st2.ListLibraries()
	if err != nil {
		t.Fatal(err)
	}
	if len(libs) != 1 || len(libs[0].Paths) != 1 || libs[0].Paths[0] != "/legacy/root" {
		t.Fatalf("migrated libraries = %+v", libs)
	}
}
