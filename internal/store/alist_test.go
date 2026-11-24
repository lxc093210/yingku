package store

import (
	"path/filepath"
	"testing"
)

// Batch 29: libraries carry a source_type; alist libraries persist
// it, local ones default to "local", and reopening the DB (running
// the migration again) keeps the values.
func TestLibrarySourceType(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.CreateLibraryWithType("4K REMUX", "movie", SourceTypeAlist, []string{"/电影/4K REMUX"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateLibrary("本地电影", "movie", []string{"/data/movies"}); err != nil {
		t.Fatal(err)
	}
	check := func(st *Store) {
		t.Helper()
		libs, err := st.ListLibraries()
		if err != nil {
			t.Fatal(err)
		}
		byID := map[int64]Library{}
		for _, l := range libs {
			byID[l.ID] = l
		}
		if got := byID[id].SourceType; got != SourceTypeAlist {
			t.Errorf("alist library source_type = %q", got)
		}
		for _, l := range libs {
			if l.ID != id && l.SourceType != SourceTypeLocal {
				t.Errorf("library %d source_type = %q, want local", l.ID, l.SourceType)
			}
		}
	}
	check(st)
	st.Close()

	// Reopen: the idempotent migration must not clobber the column.
	st2, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	check(st2)

	// Update can flip the type both ways.
	if err := st2.UpdateLibraryWithType(id, "4K REMUX", "movie", SourceTypeLocal, []string{"/data/remux"}); err != nil {
		t.Fatal(err)
	}
	lib, err := st2.GetLibrary(id)
	if err != nil || lib == nil || lib.SourceType != SourceTypeLocal {
		t.Errorf("after update: %+v, %v", lib, err)
	}
}
