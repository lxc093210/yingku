package store

import (
	"path/filepath"
	"testing"
)

func TestTitleInitials(t *testing.T) {
	cases := map[string]string{
		"钢铁侠":      "gtx",
		"钢铁侠3":     "gtx",
		"Iron Man": "",
		"流浪地球2":    "lldq",
		"三体 Three": "st",
		"":         "",
		"庆余年第二季":   "qyndej",
	}
	for title, want := range cases {
		if got := TitleInitials(title); got != want {
			t.Errorf("TitleInitials(%q) = %q, want %q", title, got, want)
		}
	}
}

func TestLibrarySortOrder(t *testing.T) {
	st := openTempStore(t)
	a, _ := st.CreateLibrary("甲库", "movie", []string{"/o/a"})
	b, _ := st.CreateLibrary("乙库", "movie", []string{"/o/b"})
	c, _ := st.CreateLibrary("丙库", "movie", []string{"/o/c"})

	libs, err := st.ListLibraries()
	if err != nil {
		t.Fatal(err)
	}
	if len(libs) != 3 || libs[0].ID != a || libs[1].ID != b || libs[2].ID != c {
		t.Fatalf("initial order = %+v", libs)
	}
	// New libraries get ascending sort_order in creation order.
	if !(libs[0].SortOrder < libs[1].SortOrder && libs[1].SortOrder < libs[2].SortOrder) {
		t.Fatalf("initial sort orders = %d %d %d", libs[0].SortOrder, libs[1].SortOrder, libs[2].SortOrder)
	}
	if err := st.SetLibraryOrder([]int64{c, a, b}); err != nil {
		t.Fatal(err)
	}
	libs, _ = st.ListLibraries()
	if libs[0].ID != c || libs[1].ID != a || libs[2].ID != b {
		t.Fatalf("after reorder = %d %d %d, want %d %d %d", libs[0].ID, libs[1].ID, libs[2].ID, c, a, b)
	}
	if libs[0].SortOrder != 1 || libs[1].SortOrder != 2 || libs[2].SortOrder != 3 {
		t.Fatalf("sort orders after reorder = %d %d %d", libs[0].SortOrder, libs[1].SortOrder, libs[2].SortOrder)
	}
}

func insertSearchItem(t *testing.T, st *Store, libID int64, path, title, original string) int64 {
	t.Helper()
	tx, err := st.Begin()
	if err != nil {
		t.Fatal(err)
	}
	id, err := tx.UpsertItem(&Item{
		LibraryID: libID, Type: "movie", Path: path, Title: title,
		OriginalTitle: original, SortTitle: title,
		SearchInitials: TitleInitials(title),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSearchFuzzyRoutes(t *testing.T) {
	st := openTempStore(t)
	libID, err := st.CreateLibrary("搜索库", "movie", []string{"/s/root"})
	if err != nil {
		t.Fatal(err)
	}
	hero := insertSearchItem(t, st, libID, "/s/root/钢铁侠 (2008)", "钢铁侠", "Iron Man")
	insertSearchItem(t, st, libID, "/s/root/钢之炼金术师 (2003)", "钢之炼金术师", "Hagane")

	contains := func(items []*Item, id int64) bool {
		for _, it := range items {
			if it.ID == id {
				return true
			}
		}
		return false
	}

	// Exact substring (FTS / multi-term route).
	items, err := st.Search("钢铁", 10)
	if err != nil || !contains(items, hero) {
		t.Errorf("search 钢铁 = %v, err %v; want hero", items, err)
	}
	// Subsequence: 钢侠 skips 铁.
	items, err = st.Search("钢侠", 10)
	if err != nil || !contains(items, hero) {
		t.Errorf("search 钢侠 (subsequence) = %v, err %v; want hero", items, err)
	}
	// Pinyin initials.
	items, err = st.Search("gtx", 10)
	if err != nil || !contains(items, hero) {
		t.Errorf("search gtx (initials) = %v, err %v; want hero", items, err)
	}
	// Multi-term: every term must hit title or original title.
	items, err = st.Search("钢铁 iron", 10)
	if err != nil || !contains(items, hero) {
		t.Errorf("search '钢铁 iron' = %v, err %v; want hero", items, err)
	}
	// A term that hits nothing yields nothing via the fuzzy routes.
	items, err = st.Search("不存在的词", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("search 不存在的词 = %v, want empty", items)
	}
}

func TestPendingMediaInfoAndProbeUpdate(t *testing.T) {
	st := openTempStore(t)
	libID, err := st.CreateLibrary("信息库", "movie", []string{"/p/root"})
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := st.Begin()
	mk := func(path, src string) int64 {
		id, err := tx.UpsertItem(&Item{LibraryID: libID, Type: "movie", Path: path, Title: path, SortTitle: path})
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.ReplaceSources(id, []MediaSource{{
			StrmPath: path + "/f.strm", TargetURL: "http://x/f",
			MediaInfoSource: src, Width: 1920, Height: 1080,
		}}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	fnID := mk("/p/root/F1", "filename")
	noneID := mk("/p/root/F2", "none")
	mk("/p/root/F3", "json")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if n, err := st.CountPendingMediaInfo(); err != nil || n != 2 {
		t.Fatalf("pending = %d, err %v; want 2", n, err)
	}
	pend, err := st.PendingMediaInfoSources(10)
	if err != nil || len(pend) != 2 {
		t.Fatalf("pending sources = %v, err %v", pend, err)
	}
	seen := map[int64]bool{}
	for _, p := range pend {
		seen[p.ItemID] = true
		if p.StrmPath == "" || p.TargetURL == "" {
			t.Errorf("pending row missing fields: %+v", p)
		}
	}
	if !seen[fnID] || !seen[noneID] {
		t.Errorf("pending items = %v, want %d and %d", seen, fnID, noneID)
	}

	// Probe result flips the source to 'probe' with real values.
	srcs, _ := st.SourcesOf(fnID)
	if len(srcs) != 1 {
		t.Fatalf("sources = %v", srcs)
	}
	upd := srcs[0]
	upd.Width, upd.Height, upd.VideoCodec = 3840, 2160, "hevc"
	upd.HDR, upd.MediaInfoSource = "HDR10", "probe"
	if err := st.UpdateSourceMediaInfo(upd.ID, upd); err != nil {
		t.Fatal(err)
	}
	srcs, _ = st.SourcesOf(fnID)
	if srcs[0].MediaInfoSource != "probe" || srcs[0].Width != 3840 || srcs[0].HDR != "HDR10" {
		t.Errorf("after probe update = %+v", srcs[0])
	}
	if n, _ := st.CountPendingMediaInfo(); n != 1 {
		t.Errorf("pending after probe = %d, want 1", n)
	}
}

func TestCoverCandidates(t *testing.T) {
	st := openTempStore(t)
	libID, err := st.CreateLibrary("封面库", "movie", []string{"/cv/root"})
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := st.Begin()
	mk := func(path string, rating float64, backdrop bool) int64 {
		id, err := tx.UpsertItem(&Item{LibraryID: libID, Type: "movie", Path: path, Title: path, SortTitle: path, Rating: rating})
		if err != nil {
			t.Fatal(err)
		}
		imgs := map[string]ImageFile{"poster": {Path: path + "/poster.jpg", Width: 500, Height: 750}}
		if backdrop {
			imgs["backdrop"] = ImageFile{Path: path + "/backdrop.jpg", Width: 1920, Height: 1080}
		}
		if err := tx.ReplaceImages(id, imgs); err != nil {
			t.Fatal(err)
		}
		return id
	}
	low := mk("/cv/root/Low", 5.0, true)
	high := mk("/cv/root/High", 9.0, true)
	mk("/cv/root/NoBackdrop", 10.0, false)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	byRating, err := st.CoverCandidates(libID, "rating", 12)
	if err != nil || len(byRating) != 2 {
		t.Fatalf("rating candidates = %v, err %v", byRating, err)
	}
	if byRating[0].ID != high || byRating[1].ID != low {
		t.Errorf("rating order = %d, %d; want %d, %d", byRating[0].ID, byRating[1].ID, high, low)
	}
	byDate, err := st.CoverCandidates(libID, "datecreated", 12)
	if err != nil || len(byDate) != 2 {
		t.Fatalf("date candidates = %v, err %v", byDate, err)
	}
	_ = filepath.Join // keep import if unused in future edits
}
