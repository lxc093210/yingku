package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Douban assist (batch 28): a last-resort source for a Chinese title
// and a Chinese overview, consulted ONLY when TMDB has neither. It
// talks to Douban's public suggest endpoint and, best-effort, the
// subject page. Politeness is structural, not advisory:
//
//   - every request is serialized through one mutex and spaced at
//     least Interval (2s by default) from the previous one;
//   - a 403/429 answer surfaces as *DoubanRateLimitError and callers
//     stop the whole round — nothing here retries;
//   - per-round budgets live with the caller (see the api layer),
//     and results are cached by the caller per TMDB id.

const (
	DefaultDoubanSuggestURL  = "https://movie.douban.com/j/subject_suggest"
	DefaultDoubanSubjectBase = "https://movie.douban.com/subject/"
	doubanUserAgent          = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
)

// DoubanRateLimitError marks a 403/429 from Douban: the round must
// stop, not retry.
type DoubanRateLimitError struct{ Status int }

func (e *DoubanRateLimitError) Error() string {
	return fmt.Sprintf("douban rate limited (HTTP %d)", e.Status)
}

// IsDoubanRateLimited reports whether err is a Douban 403/429.
func IsDoubanRateLimited(err error) bool {
	var rl *DoubanRateLimitError
	return errors.As(err, &rl)
}

// DoubanCandidate is one row of the suggest endpoint's answer.
type DoubanCandidate struct {
	ID       string `json:"id"`
	Title    string `json:"title"`     // Chinese title
	SubTitle string `json:"sub_title"` // original title
	Year     string `json:"year"`
	Type     string `json:"type"` // movie | tv
}

// DoubanInfo is the matched result: Douban subject id, Chinese
// title, and (best-effort) the Chinese overview.
type DoubanInfo struct {
	DoubanID string
	Title    string
	Overview string
}

// DoubanClient queries Douban under the rate discipline above. The
// zero-value-friendly fields are exported so tests can point the
// client at a fake server and shrink the interval.
type DoubanClient struct {
	SuggestURL  string
	SubjectBase string
	HTTPClient  *http.Client
	Interval    time.Duration

	mu   sync.Mutex
	last time.Time
}

// NewDoubanClient builds a client with production endpoints and a
// 2-second minimum request interval.
func NewDoubanClient() *DoubanClient {
	return &DoubanClient{
		SuggestURL:  DefaultDoubanSuggestURL,
		SubjectBase: DefaultDoubanSubjectBase,
		HTTPClient:  &http.Client{Timeout: 12 * time.Second},
		Interval:    2 * time.Second,
	}
}

func (c *DoubanClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 12 * time.Second}
}

// get performs one serialized, spaced GET. The mutex is held for the
// whole request, so requests are strictly serial.
func (c *DoubanClient) get(ctx context.Context, rawURL string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.last.IsZero() && c.Interval > 0 {
		if wait := c.Interval - time.Since(c.last); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", doubanUserAgent)
	req.Header.Set("Referer", "https://movie.douban.com/")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	resp, err := c.httpClient().Do(req)
	c.last = time.Now()
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return nil, &DoubanRateLimitError{Status: resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("douban: status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// Suggest queries the public suggest endpoint for a title.
func (c *DoubanClient) Suggest(ctx context.Context, query string) ([]DoubanCandidate, error) {
	u := c.SuggestURL + "?q=" + url.QueryEscape(query)
	raw, err := c.get(ctx, u)
	if err != nil {
		return nil, err
	}
	var out []DoubanCandidate
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("douban suggest: %w", err)
	}
	return out, nil
}

// FetchInfo resolves one item to its Douban subject: suggest by the
// original title, match on (original title, year, kind), then read
// the subject page for the overview, best-effort — a page failure
// still returns the matched title, except a rate-limit answer,
// which always propagates so the caller stops the round. A clean
// no-match returns (nil, nil).
func (c *DoubanClient) FetchInfo(ctx context.Context, origTitle string, year int, kind string) (*DoubanInfo, error) {
	cands, err := c.Suggest(ctx, origTitle)
	if err != nil {
		return nil, err
	}
	cand := MatchDoubanCandidate(cands, origTitle, year, kind)
	if cand == nil {
		return nil, nil
	}
	info := &DoubanInfo{DoubanID: cand.ID, Title: strings.TrimSpace(cand.Title)}
	if raw, err := c.get(ctx, c.SubjectBase+cand.ID+"/"); err != nil {
		if IsDoubanRateLimited(err) {
			return nil, err
		}
		// Best-effort: the title alone is still a useful fill.
		return info, nil
	} else {
		info.Overview = ExtractDoubanSummary(raw)
	}
	return info, nil
}

// NormalizeTitleForMatch lowercases and keeps only letters, digits
// and Han characters, so punctuation/spacing differences between
// the library's original title and Douban's sub_title do not matter.
func NormalizeTitleForMatch(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// MatchDoubanCandidate picks the candidate for an item: the type
// must match (movie vs tv) when the candidate carries one, the year
// must match when both sides know it, and the candidate's original
// title (sub_title) — or, failing that, its Chinese title — must
// equal the item's original title after normalization (a containment
// match scores lower but still counts). Nil when nothing qualifies.
func MatchDoubanCandidate(cands []DoubanCandidate, origTitle string, year int, kind string) *DoubanCandidate {
	want := NormalizeTitleForMatch(origTitle)
	if want == "" {
		return nil
	}
	wantType := "movie"
	if kind == "tv" || kind == "series" {
		wantType = "tv"
	}
	best := -1
	var picked *DoubanCandidate
	for i := range cands {
		cand := cands[i]
		if cand.Type != "" && cand.Type != wantType {
			continue
		}
		if year > 0 && cand.Year != "" {
			cy, err := strconv.Atoi(strings.TrimSpace(cand.Year))
			if err == nil && cy != year {
				continue
			}
		}
		score := -1
		sub := NormalizeTitleForMatch(cand.SubTitle)
		tit := NormalizeTitleForMatch(cand.Title)
		switch {
		case sub != "" && sub == want:
			score = 100
		case tit != "" && tit == want:
			score = 90
		case sub != "" && (strings.Contains(sub, want) || strings.Contains(want, sub)):
			score = 40
		}
		if score < 0 {
			continue
		}
		if year > 0 && cand.Year != "" {
			if cy, err := strconv.Atoi(strings.TrimSpace(cand.Year)); err == nil && cy == year {
				score += 30
			}
		}
		if score > best {
			best = score
			c := cand
			picked = &c
		}
	}
	return picked
}

var (
	doubanSummaryRe = regexp.MustCompile(`(?s)<span[^>]*property="v:summary"[^>]*>(.*?)</span>`)
	doubanOGDescRe  = regexp.MustCompile(`<meta[^>]+property="og:description"[^>]+content="([^"]*)"`)
	doubanTagRe     = regexp.MustCompile(`<[^>]+>`)
	doubanSpaceRe   = regexp.MustCompile(`\s+`)
)

// ExtractDoubanSummary pulls the Chinese overview out of a subject
// page: the v:summary span first (tags stripped, entities decoded),
// the og:description meta as fallback. "" when neither is present.
func ExtractDoubanSummary(page []byte) string {
	if m := doubanSummaryRe.FindSubmatch(page); m != nil {
		s := doubanTagRe.ReplaceAllString(string(m[1]), " ")
		s = html.UnescapeString(s)
		s = doubanSpaceRe.ReplaceAllString(s, " ")
		s = strings.TrimSpace(s)
		s = strings.TrimSuffix(s, "(展开全部)")
		s = strings.TrimSpace(s)
		if s != "" {
			return s
		}
	}
	if m := doubanOGDescRe.FindSubmatch(page); m != nil {
		return strings.TrimSpace(html.UnescapeString(string(m[1])))
	}
	return ""
}
