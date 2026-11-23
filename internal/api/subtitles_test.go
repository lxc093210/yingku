package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yingku/internal/store"
)

func rawGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSRTToVTT(t *testing.T) {
	in := "\ufeff1\r\n00:01:18,240 --> 00:01:20,410\r\n第一行\r\n第二行\r\n\r\n2\r\n00:01:20,410 --> 00:01:22,510\r\n下一句\r\n"
	out := srtToVTT(in)
	if !strings.HasPrefix(out, "WEBVTT\n\n") {
		t.Fatalf("missing WEBVTT header: %q", out[:20])
	}
	if !strings.Contains(out, "00:01:18.240 --> 00:01:20.410") {
		t.Errorf("timestamp commas not converted:\n%s", out)
	}
	if strings.Contains(out, ",240") || strings.Contains(out, "\r") {
		t.Errorf("leftover comma/CR:\n%q", out)
	}
	if !strings.Contains(out, "第一行\n第二行") {
		t.Errorf("payload changed:\n%s", out)
	}
}

func TestASSToVTT(t *testing.T) {
	in := `[Script Info]
Title: test

[V4+ Styles]
Format: Name, Fontname, Fontsize

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:01:18.24,0:01:20.41,Default,,0,0,0,,{\pos(320,240)}你好，世界\N第二行\h空格
Comment: 0,0:01:21.00,0:01:22.00,Default,,0,0,0,,注释不算
Dialogue: 0,1:02:03.05,1:02:05.00,Default,,0,0,0,,含,逗号,的文本
`
	out, err := assToVTT(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "WEBVTT\n\n") {
		t.Fatalf("missing header:\n%s", out)
	}
	if !strings.Contains(out, "00:01:18.240 --> 00:01:20.410") {
		t.Errorf("centisecond conversion wrong:\n%s", out)
	}
	if !strings.Contains(out, "01:02:03.050 --> 01:02:05.000") {
		t.Errorf("hour conversion wrong:\n%s", out)
	}
	if strings.Contains(out, "{") || strings.Contains(out, `\N`) || strings.Contains(out, `\h`) {
		t.Errorf("override/escape residue:\n%s", out)
	}
	if !strings.Contains(out, "你好，世界\n第二行 空格") {
		t.Errorf("text cleanup wrong:\n%s", out)
	}
	if !strings.Contains(out, "含,逗号,的文本") {
		t.Errorf("commas inside Text were split:\n%s", out)
	}
	if strings.Contains(out, "注释不算") {
		t.Errorf("Comment lines must not become cues:\n%s", out)
	}
}

func TestASSToVTTCustomFormatOrder(t *testing.T) {
	// Text is not the last field here; the parser must follow Format.
	in := "[Events]\nFormat: Start, Text, End\nDialogue: 0:00:01.00,先说的话,0:00:02.50\n"
	out, err := assToVTT(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "00:00:01.000 --> 00:00:02.500\n先说的话") {
		t.Errorf("custom format order mishandled:\n%s", out)
	}
}

func TestSubtitleLangInference(t *testing.T) {
	cases := []struct {
		path string
		lang string
		want string
	}{
		{"x/movie.chi.zh-cn.ass", "", "zh-cn"},
		{"x/movie.chs.srt", "", "zh-cn"},
		{"x/movie.cht.ass", "", "zh-tw"},
		{"x/movie.zh-tw.ass", "", "zh-tw"},
		{"x/movie.eng.srt", "", "en"},
		{"x/movie.jpn.srt", "", "ja"},
		{"x/movie.srt", "", "und"},
		{"x/movie.srt", "chs", "zh-cn"}, // scanner column fallback
		{"x/movie.srt", "eng", "en"},
	}
	for _, c := range cases {
		got := subtitleLang(store.Subtitle{Path: c.path, Lang: c.lang})
		if got != c.want {
			t.Errorf("subtitleLang(%q, %q) = %q, want %q", c.path, c.lang, got, c.want)
		}
	}
}

func TestSubtitleEndpoints(t *testing.T) {
	h := testHandler(t)
	movieID, _ := fixtureIDs(t, h)

	var subs []subtitleTrackDTO
	if code := get(t, h, "/api/items/"+itoa(movieID)+"/subtitles", &subs); code != 200 {
		t.Fatalf("subtitles = %d", code)
	}
	if len(subs) != 3 {
		t.Fatalf("subtitle tracks = %+v, want 3", subs)
	}
	byFormat := map[string][]subtitleTrackDTO{}
	for _, s := range subs {
		byFormat[s.Format] = append(byFormat[s.Format], s)
	}
	if len(byFormat["ass"]) != 2 || len(byFormat["srt"]) != 1 {
		t.Fatalf("formats = %+v", byFormat)
	}
	for _, s := range byFormat["ass"] {
		if s.Lang != "zh-cn" {
			t.Errorf("ass lang = %q, want zh-cn", s.Lang)
		}
	}
	labels := map[string]bool{}
	for _, s := range subs {
		labels[s.Label] = true
	}
	if !labels["简体中文"] || !labels["简体中文 2"] || !labels["未知语言"] {
		t.Errorf("labels = %v", labels)
	}

	// SRT track converts to WebVTT with dotted timestamps.
	srtID := byFormat["srt"][0].ID
	rec := rawGet(t, h, "/api/subtitles/"+itoa(srtID)+"/track")
	if rec.Code != 200 {
		t.Fatalf("srt track = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/vtt") {
		t.Errorf("track content type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "WEBVTT") || !strings.Contains(body, "00:01:18.240 --> 00:01:20.410") {
		t.Errorf("srt track body wrong:\n%.200s", body)
	}

	// ASS track converts too (fixture has 3402 dialogues).
	assID := byFormat["ass"][0].ID
	rec = rawGet(t, h, "/api/subtitles/"+itoa(assID)+"/track")
	if rec.Code != 200 {
		t.Fatalf("ass track = %d %s", rec.Code, rec.Body.String()[:100])
	}
	body = rec.Body.String()
	if !strings.HasPrefix(body, "WEBVTT") || !strings.Contains(body, " --> ") {
		t.Errorf("ass track body wrong:\n%.200s", body)
	}
	if strings.Contains(body, "Dialogue:") {
		t.Errorf("ass track leaked raw Dialogue lines")
	}

	if code := get(t, h, "/api/subtitles/999999/track", nil); code != 404 {
		t.Errorf("missing subtitle track = %d, want 404", code)
	}
	if code := get(t, h, "/api/items/999999/subtitles", nil); code != 404 {
		t.Errorf("missing item subtitles = %d, want 404", code)
	}
}

func TestBackdropHiFallsBackToLocal(t *testing.T) {
	h := testHandler(t)
	movieID, seriesID := fixtureIDs(t, h)

	// No TMDB key/base configured: the endpoint must serve the local
	// backdrop the scanner picked (the 3840px background.jpg wins).
	rec := rawGet(t, h, "/api/items/"+itoa(movieID)+"/image/backdrop-hi")
	if rec.Code != 200 {
		t.Fatalf("backdrop-hi = %d", rec.Code)
	}
	matches, _ := filepath.Glob(filepath.Join(fixturesAbs(t), "电影", "动画电影", "*"))
	if len(matches) != 1 {
		t.Fatalf("fixture movie dirs = %v", matches)
	}
	want, err := os.ReadFile(filepath.Join(matches[0], "background.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Body.Len() != len(want) {
		t.Errorf("backdrop-hi bytes = %d, want local background.jpg %d", rec.Body.Len(), len(want))
	}

	// The series has no images at all: 404, same as the plain endpoint.
	if code := get(t, h, "/api/items/"+itoa(seriesID)+"/image/backdrop-hi", nil); code != 404 {
		t.Errorf("series backdrop-hi = %d, want 404", code)
	}
}
