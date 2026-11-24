package meta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMatchDoubanCandidate(t *testing.T) {
	cands := []DoubanCandidate{
		{ID: "1", Title: "钢铁侠", SubTitle: "Iron Man", Year: "2008", Type: "movie"},
		{ID: "2", Title: "钢铁侠（电视剧）", SubTitle: "Iron Man", Year: "2008", Type: "tv"},
		{ID: "3", Title: "钢铁侠2", SubTitle: "Iron Man 2", Year: "2010", Type: "movie"},
	}
	// Exact original title + year + type wins.
	got := MatchDoubanCandidate(cands, "Iron Man", 2008, "movie")
	if got == nil || got.ID != "1" {
		t.Fatalf("match = %+v, want id 1", got)
	}
	// TV kind picks the TV candidate.
	got = MatchDoubanCandidate(cands, "Iron Man", 2008, "tv")
	if got == nil || got.ID != "2" {
		t.Fatalf("tv match = %+v, want id 2", got)
	}
	// Year mismatch is rejected even with an exact title.
	if got := MatchDoubanCandidate(cands, "Iron Man", 1999, "movie"); got != nil {
		t.Fatalf("year-mismatch match = %+v, want nil", got)
	}
	// No title relation at all: no match.
	if got := MatchDoubanCandidate(cands, "Avatar", 2008, "movie"); got != nil {
		t.Fatalf("unrelated match = %+v, want nil", got)
	}
	// Containment (original title inside a longer candidate) matches.
	got = MatchDoubanCandidate(
		[]DoubanCandidate{{ID: "9", Title: "流浪地球", SubTitle: "流浪地球 The Wandering Earth", Year: "2019", Type: "movie"}},
		"流浪地球", 2019, "movie")
	if got == nil || got.ID != "9" {
		t.Fatalf("containment match = %+v, want id 9", got)
	}
}

func TestExtractDoubanSummary(t *testing.T) {
	page := []byte(`<html><body><span property="v:summary" class="">  地球  遭遇危机，
 人类建造发动机。(展开全部)</span></body></html>`)
	if got := ExtractDoubanSummary(page); got != "地球 遭遇危机， 人类建造发动机。" {
		t.Errorf("summary = %q", got)
	}
	og := []byte(`<html><head><meta property="og:description" content="简介来自 og 标签。"></head></html>`)
	if got := ExtractDoubanSummary(og); got != "简介来自 og 标签。" {
		t.Errorf("og summary = %q", got)
	}
	if got := ExtractDoubanSummary([]byte(`<html></html>`)); got != "" {
		t.Errorf("empty summary = %q", got)
	}
}

func fakeDouban(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/j/subject_suggest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"1851857","title":"流浪地球","sub_title":"流浪地球","year":"2019","type":"movie"}]`))
	})
	mux.HandleFunc("/subject/1851857/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><body><span property="v:summary">太阳即将毁灭，人类开启流浪地球计划。</span></body></html>`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDoubanFetchInfoFakeServer(t *testing.T) {
	srv := fakeDouban(t)
	c := NewDoubanClient()
	c.SuggestURL = srv.URL + "/j/subject_suggest"
	c.SubjectBase = srv.URL + "/subject/"
	c.Interval = time.Millisecond

	info, err := c.FetchInfo(context.Background(), "流浪地球", 2019, "movie")
	if err != nil {
		t.Fatal(err)
	}
	if info == nil || info.DoubanID != "1851857" || info.Title != "流浪地球" {
		t.Fatalf("info = %+v", info)
	}
	if info.Overview != "太阳即将毁灭，人类开启流浪地球计划。" {
		t.Errorf("overview = %q", info.Overview)
	}
	// No candidate for an unknown title -> (nil, nil).
	c2 := NewDoubanClient()
	c2.SuggestURL = srv.URL + "/j/subject_suggest"
	c2.SubjectBase = srv.URL + "/subject/"
	c2.Interval = time.Millisecond
	info, err = c2.FetchInfo(context.Background(), "不存在的片子", 1900, "movie")
	if err != nil || info != nil {
		t.Errorf("unknown fetch = %+v, %v; want nil, nil", info, err)
	}
}

func TestDoubanRateLimitStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c := NewDoubanClient()
	c.SuggestURL = srv.URL + "/j/subject_suggest"
	c.SubjectBase = srv.URL + "/subject/"
	c.Interval = time.Millisecond
	_, err := c.FetchInfo(context.Background(), "流浪地球", 2019, "movie")
	if !IsDoubanRateLimited(err) {
		t.Fatalf("err = %v, want rate-limit error", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %v, want status mentioned", err)
	}
}

func TestDoubanLimiterSerializes(t *testing.T) {
	srv := fakeDouban(t)
	c := NewDoubanClient()
	c.SuggestURL = srv.URL + "/j/subject_suggest"
	c.SubjectBase = srv.URL + "/subject/"
	c.Interval = 150 * time.Millisecond
	start := time.Now()
	for i := 0; i < 2; i++ {
		if _, err := c.Suggest(context.Background(), "流浪地球"); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("two suggests took %v, want >= interval 150ms", elapsed)
	}
}
