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

// Batch 29c: the mediainfo job's pending set must never include
// Alist sources — stage ① would rescan the library and stage ②
// would ffprobe a remote path.
func TestPendingMediaInfoExcludesAlist(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	localID, err := st.CreateLibrary("本地", "movie", []string{"/data/m"})
	if err != nil {
		t.Fatal(err)
	}
	alistID, err := st.CreateLibraryWithType("远程", "movie", SourceTypeAlist, []string{"/电影/4K REMUX"})
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := st.Begin()
	mk := func(libID int64, path string) int64 {
		id, err := tx.UpsertItem(&Item{LibraryID: libID, Type: "movie", Path: path, Title: path, SortTitle: path})
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.ReplaceSources(id, []MediaSource{{
			StrmPath: path + "/f.strm", MediaInfoSource: "filename",
		}}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	localItem := mk(localID, "/data/m/F1")
	mk(alistID, "/电影/4K REMUX/片 4K REMUX（2020）")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if n, err := st.CountPendingMediaInfo(); err != nil || n != 1 {
		t.Fatalf("pending count = %d, err %v; want 1 (local only)", n, err)
	}
	pend, err := st.PendingMediaInfoSources(10)
	if err != nil || len(pend) != 1 || pend[0].ItemID != localItem {
		t.Fatalf("pending = %+v, err %v; want only the local item", pend, err)
	}
}

// Batch 29c: resolved flag round-trip — a folder-level source
// (resolved = 0) is invisible to ResolvedItemPaths until
// UpdateSourceResolved writes the concrete file back.
func TestSourceResolvedRoundtrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	libID, err := st.CreateLibraryWithType("远程", "movie", SourceTypeAlist, []string{"/电影/4K REMUX"})
	if err != nil {
		t.Fatal(err)
	}
	folder := "/电影/4K REMUX/信条 4K REMUX（2020）"
	tx, _ := st.Begin()
	id, err := tx.UpsertItem(&Item{LibraryID: libID, Type: "movie", Path: folder, Title: "信条", SortTitle: "信条"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.ReplaceSources(id, []MediaSource{{
		StrmPath: folder, MediaInfoSource: "filename", Resolved: false,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	srcs, _ := st.SourcesOf(id)
	if len(srcs) != 1 || srcs[0].Resolved {
		t.Fatalf("sources = %+v, want one unresolved", srcs)
	}
	if paths, _ := st.ResolvedItemPaths(libID); len(paths) != 0 {
		t.Errorf("resolved paths = %v, want empty", paths)
	}
	upd := srcs[0]
	upd.StrmPath = folder + "/Tenet.2020.2160p.REMUX.mkv"
	upd.Width, upd.Height, upd.VideoCodec = 3840, 2160, "hevc"
	upd.SizeBytes = 50_000_000_000
	if err := st.UpdateSourceResolved(upd); err != nil {
		t.Fatal(err)
	}
	srcs, _ = st.SourcesOf(id)
	if len(srcs) != 1 || !srcs[0].Resolved || srcs[0].StrmPath != upd.StrmPath ||
		srcs[0].Width != 3840 || srcs[0].SizeBytes != 50_000_000_000 {
		t.Errorf("after resolve = %+v", srcs)
	}
	if paths, _ := st.ResolvedItemPaths(libID); !paths[folder] {
		t.Errorf("resolved paths = %v, want %q in set", paths, folder)
	}
}
