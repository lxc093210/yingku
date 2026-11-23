package meta

import (
	"path/filepath"
	"strings"
	"testing"
)

func fixture(parts ...string) string {
	base := []string{"..", "..", "fixtures", "library"}
	return filepath.Join(append(base, parts...)...)
}

var movieDir = fixture("电影", "动画电影", "蜘蛛侠：平行宇宙 (2018) {tmdbid-324857}")
var seriesDir = fixture("电视剧", "国产剧", "上海女子图鉴 (2018) {tmdbid-80607}")

func TestParseFolderName(t *testing.T) {
	fi := ParseFolderName("蜘蛛侠：平行宇宙 (2018) {tmdbid-324857}")
	if fi.Tmdb != 324857 || fi.Year != 2018 || fi.Title != "蜘蛛侠：平行宇宙" {
		t.Fatalf("got %+v", fi)
	}
	fi = ParseFolderName("上海女子图鉴 (2018) {tmdbid-80607}")
	if fi.Tmdb != 80607 || fi.Year != 2018 || fi.Title != "上海女子图鉴" {
		t.Fatalf("got %+v", fi)
	}
	fi = ParseFolderName("Plain Title")
	if fi.Tmdb != 0 || fi.Year != 0 || fi.Title != "Plain Title" {
		t.Fatalf("got %+v", fi)
	}
}

func TestParseSeasonDirAndSxExx(t *testing.T) {
	if n, ok := ParseSeasonDir("Season 1"); !ok || n != 1 {
		t.Fatalf("Season 1 -> %d %v", n, ok)
	}
	if n, ok := ParseSeasonDir("Specials"); !ok || n != 0 {
		t.Fatalf("Specials -> %d %v", n, ok)
	}
	if _, ok := ParseSeasonDir("Extras"); ok {
		t.Fatal("Extras must not parse as season")
	}
	s, e, ok := ParseSxExx("Women of Shanghai.2018.S01E09.WEB-DL.strm")
	if !ok || s != 1 || e != 9 {
		t.Fatalf("S01E09 -> %d %d %v", s, e, ok)
	}
}

func TestParseMovieNFO(t *testing.T) {
	matches, _ := filepath.Glob(filepath.Join(movieDir, "*.nfo"))
	if len(matches) != 1 {
		t.Fatalf("movie nfo files: %v", matches)
	}
	nfo, err := ParseNFOFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if nfo.Title != "蜘蛛侠：平行宇宙" {
		t.Errorf("title = %q", nfo.Title)
	}
	if nfo.OriginalTitle != "Spider-Man: Into the Spider-Verse" {
		t.Errorf("originaltitle = %q", nfo.OriginalTitle)
	}
	if nfo.RatingValue() != 8.6 {
		t.Errorf("rating = %v", nfo.RatingValue())
	}
	if nfo.Year != 2018 || nfo.Runtime != 117 {
		t.Errorf("year/runtime = %d/%d", nfo.Year, nfo.Runtime)
	}
	if len(nfo.Genres) != 4 || nfo.Genres[0] != "动画" {
		t.Errorf("genres = %v", nfo.Genres)
	}
	if !strings.Contains(nfo.Plot, "迈尔斯") {
		t.Errorf("plot missing CDATA content: %.40q", nfo.Plot)
	}
}

func TestParseTVShowNFO(t *testing.T) {
	nfo, err := ParseNFOFile(filepath.Join(seriesDir, "tvshow.nfo"))
	if err != nil {
		t.Fatal(err)
	}
	if nfo.Title != "上海女子图鉴" || nfo.TmdbID != 80607 {
		t.Errorf("title/tmdb = %q/%d", nfo.Title, nfo.TmdbID)
	}
	if nfo.RatingValue() != 6 || nfo.Year != 2018 {
		t.Errorf("rating/year = %v/%d", nfo.RatingValue(), nfo.Year)
	}
}

func TestParseEpisodeNFO(t *testing.T) {
	p := filepath.Join(seriesDir, "Season 1", "Women of Shanghai.2018.S01E09.WEB-DL.2160p.H265.AAC.10bit.25fps-LeagueWEB.nfo")
	nfo, err := ParseNFOFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if nfo.SeasonNumber() != 1 || nfo.Episode != 9 {
		t.Errorf("season/episode = %d/%d", nfo.SeasonNumber(), nfo.Episode)
	}
	if nfo.Title == "" || !strings.Contains(nfo.Plot, "罗海燕") {
		t.Errorf("episode title/plot empty: %q", nfo.Title)
	}
}

func TestParseSidecarMovie(t *testing.T) {
	matches, _ := filepath.Glob(filepath.Join(movieDir, "*-mediainfo.json"))
	if len(matches) != 1 {
		t.Fatalf("sidecars: %v", matches)
	}
	mi, ok := SidecarProvider{}.Lookup(strings.TrimSuffix(matches[0], "-mediainfo.json") + ".strm")
	if !ok {
		t.Fatal("sidecar lookup failed")
	}
	if mi.Width != 3840 || mi.Height != 2160 {
		t.Errorf("size = %dx%d", mi.Width, mi.Height)
	}
	if mi.VideoCodec != "hevc" {
		t.Errorf("codec = %q", mi.VideoCodec)
	}
	if !strings.Contains(mi.HDR, "Dolby") {
		t.Errorf("hdr = %q, want a Dolby Vision marker", mi.HDR)
	}
	if mi.Source != "json" || mi.RuntimeMin != 117 {
		t.Errorf("source/runtime = %q/%d", mi.Source, mi.RuntimeMin)
	}
	if !strings.Contains(mi.AudioSummary, "truehd") {
		t.Errorf("audio summary = %q", mi.AudioSummary)
	}
}

func TestParseSidecarEpisodeSDR(t *testing.T) {
	strm := filepath.Join(seriesDir, "Season 1", "Women of Shanghai.2018.S01E09.WEB-DL.2160p.H265.AAC.10bit.25fps-LeagueWEB.strm")
	mi, ok := SidecarProvider{}.Lookup(strm)
	if !ok {
		t.Fatal("sidecar lookup failed")
	}
	if mi.Width != 3840 || mi.Height != 1904 {
		t.Errorf("size = %dx%d", mi.Width, mi.Height)
	}
	if mi.HDR != "" {
		t.Errorf("SDR episode hdr = %q, want empty", mi.HDR)
	}
	if mi.RuntimeMin != 31 {
		t.Errorf("runtime = %d, want 31", mi.RuntimeMin)
	}
}

func TestInferFromFilename(t *testing.T) {
	mi := InferFromFilename("Some.Movie.2024.2160p.WEB-DL.H265.DV HDR.mkv.strm")
	if mi.Source != "filename" || mi.Width != 3840 || mi.VideoCodec != "hevc" || mi.HDR != "DolbyVision" {
		t.Errorf("got %+v", mi)
	}
	mi = InferFromFilename("plain.strm")
	if mi.Source != "none" {
		t.Errorf("got %+v, want source none", mi)
	}
}
