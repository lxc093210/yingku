package scan

import (
	"path/filepath"
	"testing"
)

// TestScanSeasonPosters covers the batch-28 season artwork chain at
// scan level: a season directory's own poster.jpg wins, then the
// series root's season{NN}-poster.jpg; a season with neither gets no
// registered poster (the API layer falls back to TMDB / the series
// poster at serve time).
func TestScanSeasonPosters(t *testing.T) {
	base := t.TempDir()
	seriesDir := filepath.Join(base, "深夜食堂 (2009) {tmdbid-47008}")
	writeFile(t, filepath.Join(seriesDir, "tvshow.nfo"),
		"<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<tvshow>\n  <title>深夜食堂</title>\n  <year>2009</year>\n  <tmdbid>47008</tmdbid>\n</tvshow>\n")
	writeFile(t, filepath.Join(seriesDir, "poster.jpg"), "fake-series-poster")
	writeFile(t, filepath.Join(seriesDir, "season02-poster.jpg"), "fake-root-season2")
	writeFile(t, filepath.Join(seriesDir, "season03-poster.jpg"), "fake-root-season3")
	writeFile(t, filepath.Join(seriesDir, "Season 1", "Show.S01E01.strm"), "http://example.com/s1e1.mkv\n")
	writeFile(t, filepath.Join(seriesDir, "Season 2", "poster.jpg"), "fake-dir-season2")
	writeFile(t, filepath.Join(seriesDir, "Season 2", "Show.S02E01.strm"), "http://example.com/s2e1.mkv\n")
	writeFile(t, filepath.Join(seriesDir, "Season 3", "Show.S03E01.strm"), "http://example.com/s3e1.mkv\n")

	st := openTemp(t)
	id, err := st.CreateLibrary("季封面库", "series", []string{seriesDir})
	if err != nil {
		t.Fatal(err)
	}
	sc := New(st)
	if err := sc.ScanLibraryByID(id); err != nil {
		t.Fatal(err)
	}
	series, err := st.GetItemByPath(seriesDir)
	if err != nil || series == nil {
		t.Fatalf("series = %+v, err %v", series, err)
	}
	// Search initials are written at scan time (batch 28): the
	// pinyin-initials search route finds the series by "syst".
	found, err := st.Search("syst", 10)
	if err != nil || len(found) != 1 || found[0].ID != series.ID {
		t.Errorf("search syst = %v, err %v; want the series", found, err)
	}
	seasons, _ := st.Children(series.ID)
	if len(seasons) != 3 {
		t.Fatalf("seasons = %d, want 3", len(seasons))
	}
	byNum := map[int]int64{}
	for _, sn := range seasons {
		if sn.SeasonNum != nil {
			byNum[*sn.SeasonNum] = sn.ID
		}
	}
	posterOf := func(seasonID int64) string {
		p, err := st.ImagePath(seasonID, "poster")
		if err != nil {
			return ""
		}
		return p
	}
	if got := posterOf(byNum[1]); got != "" {
		t.Errorf("season 1 poster = %q, want none registered", got)
	}
	if got, want := posterOf(byNum[2]), filepath.Join(seriesDir, "Season 2", "poster.jpg"); got != want {
		t.Errorf("season 2 poster = %q, want %q (season dir wins)", got, want)
	}
	if got, want := posterOf(byNum[3]), filepath.Join(seriesDir, "season03-poster.jpg"); got != want {
		t.Errorf("season 3 poster = %q, want %q (series root seasonNN-poster)", got, want)
	}
}
