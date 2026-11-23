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
