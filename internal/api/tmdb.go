package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"

	"yingku/internal/store"
)

// TMDB access configuration. Requests go to the official API unless the
// user points tmdb_base_url at a proxy (their own TMDB reverse proxy
// injects the key server-side and also mirrors the /t/p image paths).
const (
	settingTMDBBaseURL = "tmdb_base_url"
	defaultTMDBBase    = "https://api.themoviedb.org/3"
	defaultTMDBImage   = "https://image.tmdb.org"
)

// tmdbClient fetches cast data from TMDB as a fallback when local NFO
// metadata carries no cast (or no cast photos).
var tmdbClient = &http.Client{Timeout: 6 * time.Second}

// tmdbDownloadClient fetches backdrop images (larger payloads, looser cap).
var tmdbDownloadClient = &http.Client{Timeout: 10 * time.Second}

type tmdbConfig struct {
	key       string // may be empty when a key-injecting proxy is used
	base      string // API base, no trailing slash
	custom    bool   // base was explicitly configured (non-official)
	imageBase string // image CDN base, no trailing slash
}

// available reports whether TMDB can be queried at all: either a key is
// configured, or a custom (key-injecting proxy) base URL is.
func (c tmdbConfig) available() bool { return c.key != "" || c.custom }

func (s *Server) tmdbCfg() (tmdbConfig, error) {
	key, err := s.st.GetSetting(settingTMDBKey)
	if err != nil {
		return tmdbConfig{}, err
	}
	rawBase, err := s.st.GetSetting(settingTMDBBaseURL)
	if err != nil {
		return tmdbConfig{}, err
	}
	cfg := tmdbConfig{key: key, base: defaultTMDBBase, imageBase: defaultTMDBImage}
	if b := strings.TrimRight(strings.TrimSpace(rawBase), "/"); b != "" && b != defaultTMDBBase {
		cfg.base = b
		cfg.custom = true
		if u, err := url.Parse(b); err == nil && u.Scheme != "" && u.Host != "" {
			cfg.imageBase = u.Scheme + "://" + u.Host
		}
	}
	return cfg, nil
}

// get performs one TMDB API GET, attaching the api_key parameter only
// when a key is configured (a custom proxy base injects it server-side;
// the official API without a key is never queried).
func (c tmdbConfig) get(client *http.Client, path string) (*http.Response, error) {
	u := c.base + path
	if c.key != "" {
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		u += sep + "api_key=" + url.QueryEscape(c.key)
	}
	return client.Get(u)
}

type tmdbCredits struct {
	Cast []struct {
		Name        string `json:"name"`
		Character   string `json:"character"`
		ProfilePath string `json:"profile_path"`
		Order       int    `json:"order"`
	} `json:"cast"`
}

// fetchTMDBCast returns up to 15 cast members for a movie/series TMDB id.
// Any error is returned for the caller to swallow silently.
func fetchTMDBCast(cfg tmdbConfig, kind string, tmdbID int) ([]store.Person, error) {
	resp, err := cfg.get(tmdbClient, fmt.Sprintf("/%s/%d/credits", kind, tmdbID))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tmdb credits: status %d", resp.StatusCode)
	}
	var credits tmdbCredits
	if err := json.NewDecoder(resp.Body).Decode(&credits); err != nil {
		return nil, err
	}
	sort.SliceStable(credits.Cast, func(i, j int) bool {
		return credits.Cast[i].Order < credits.Cast[j].Order
	})
	out := []store.Person{}
	for _, c := range credits.Cast {
		if strings.TrimSpace(c.Name) == "" {
			continue
		}
		thumb := ""
		if c.ProfilePath != "" {
			thumb = cfg.imageBase + "/t/p/w185" + c.ProfilePath
		}
		out = append(out, store.Person{
			Name: c.Name, Role: c.Character, Kind: "Actor",
			Thumb: thumb, Ord: len(out),
		})
		if len(out) >= 15 {
			break
		}
	}
	return out, nil
}

type tmdbImages struct {
	Backdrops []struct {
		FilePath string `json:"file_path"`
		Width    int    `json:"width"`
		Height   int    `json:"height"`
	} `json:"backdrops"`
	Posters []struct {
		FilePath    string  `json:"file_path"`
		Width       int     `json:"width"`
		VoteAverage float64 `json:"vote_average"`
	} `json:"posters"`
}

// fetchTMDBArt picks the best poster and backdrop file_paths in a single
// /images request: the poster with the highest vote average (ties broken
// by width), and the widest backdrop. Either may be "" when TMDB has no
// such artwork for the title.
func fetchTMDBArt(cfg tmdbConfig, kind string, tmdbID int) (poster, backdrop string, err error) {
	resp, err := cfg.get(tmdbClient, fmt.Sprintf("/%s/%d/images", kind, tmdbID))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("tmdb images: status %d", resp.StatusCode)
	}
	var imgs tmdbImages
	if err := json.NewDecoder(resp.Body).Decode(&imgs); err != nil {
		return "", "", err
	}
	bestVote, bestPW := -1.0, -1
	for _, p := range imgs.Posters {
		if p.FilePath == "" {
			continue
		}
		if p.VoteAverage > bestVote || (p.VoteAverage == bestVote && p.Width > bestPW) {
			poster, bestVote, bestPW = p.FilePath, p.VoteAverage, p.Width
		}
	}
	bestBW := -1
	for _, b := range imgs.Backdrops {
		if b.FilePath != "" && b.Width > bestBW {
			backdrop, bestBW = b.FilePath, b.Width
		}
	}
	return poster, backdrop, nil
}

type tmdbEpisode struct {
	StillPath string `json:"still_path"`
}

// fetchTMDBEpisodeStill returns the still file_path for one episode of
// a series ("" when TMDB has no still for it). Any error is returned
// for the caller to swallow silently.
func fetchTMDBEpisodeStill(cfg tmdbConfig, tmdbID, seasonNum, episodeNum int) (string, error) {
	resp, err := cfg.get(tmdbClient, fmt.Sprintf("/tv/%d/season/%d/episode/%d", tmdbID, seasonNum, episodeNum))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tmdb episode: status %d", resp.StatusCode)
	}
	var ep tmdbEpisode
	if err := json.NewDecoder(resp.Body).Decode(&ep); err != nil {
		return "", err
	}
	return ep.StillPath, nil
}

type tmdbSeason struct {
	PosterPath string `json:"poster_path"`
}

// fetchTMDBSeasonPoster returns the poster file_path for one season
// of a series ("" when TMDB has no season poster). Used by the
// per-season poster fallback chain (batch 28): season-dir poster →
// series-root seasonNN-poster → TMDB season poster → series poster.
func fetchTMDBSeasonPoster(cfg tmdbConfig, tmdbID, seasonNum int) (string, error) {
	resp, err := cfg.get(tmdbClient, fmt.Sprintf("/tv/%d/season/%d", tmdbID, seasonNum))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tmdb season: status %d", resp.StatusCode)
	}
	var sn tmdbSeason
	if err := json.NewDecoder(resp.Body).Decode(&sn); err != nil {
		return "", err
	}
	return sn.PosterPath, nil
}

type tmdbDetails struct {
	Title          string  `json:"title"` // movies
	Name           string  `json:"name"`  // series
	Overview       string  `json:"overview"`
	VoteAverage    float64 `json:"vote_average"`
	Runtime        int     `json:"runtime"`          // movies
	EpisodeRunTime []int   `json:"episode_run_time"` // series
	Genres         []struct {
		Name string `json:"name"`
	} `json:"genres"`
}

// fetchTMDBDetails returns the fill-in metadata for a movie/series:
// overview, community rating, genre names and runtime in minutes
// (episode runtime for series). Language is zh-CN so filled overviews
// and genre names match the library's predominantly Chinese metadata.
func fetchTMDBDetails(cfg tmdbConfig, kind string, tmdbID int) (title, overview string, rating float64, genres []string, runtimeMin int, err error) {
	resp, err := cfg.get(tmdbClient, fmt.Sprintf("/%s/%d?language=zh-CN", kind, tmdbID))
	if err != nil {
		return "", "", 0, nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", 0, nil, 0, fmt.Errorf("tmdb details: status %d", resp.StatusCode)
	}
	var d tmdbDetails
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return "", "", 0, nil, 0, err
	}
	title = d.Title
	if title == "" {
		title = d.Name
	}
	genres = []string{}
	for _, g := range d.Genres {
		if g.Name != "" {
			genres = append(genres, g.Name)
		}
	}
	runtimeMin = d.Runtime
	if runtimeMin == 0 && len(d.EpisodeRunTime) > 0 {
		runtimeMin = d.EpisodeRunTime[0]
	}
	return title, d.Overview, d.VoteAverage, genres, runtimeMin, nil
}

// hasHan reports whether s contains at least one Han (Chinese)
// character — the test for "this text is Chinese" in the metadata
// language preference (Chinese first, other languages as fallback).
func hasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// fetchTMDBBackdrop picks the widest backdrop file_path for a movie or
// series (language-agnostic: textless artwork travels best). "" when the
// title has no backdrops at all.
func fetchTMDBBackdrop(cfg tmdbConfig, kind string, tmdbID int) (string, error) {
	resp, err := cfg.get(tmdbClient, fmt.Sprintf("/%s/%d/images", kind, tmdbID))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tmdb images: status %d", resp.StatusCode)
	}
	var imgs tmdbImages
	if err := json.NewDecoder(resp.Body).Decode(&imgs); err != nil {
		return "", err
	}
	best := ""
	bestW := -1
	for _, b := range imgs.Backdrops {
		if b.FilePath != "" && b.Width > bestW {
			best, bestW = b.FilePath, b.Width
		}
	}
	return best, nil
}

// downloadTMDBImage fetches one image (original size) from the image CDN.
func (c tmdbConfig) downloadBackdrop(filePath string) ([]byte, error) {
	return c.downloadSized(filePath, "original")
}

// downloadSized fetches one image at a named TMDB size (w780, w1280,
// original, ...) from the image CDN.
func (c tmdbConfig) downloadSized(filePath, size string) ([]byte, error) {
	resp, err := tmdbDownloadClient.Get(c.imageBase + "/t/p/" + size + filePath)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tmdb image: status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

// peopleThumbsMissing reports whether a people list is empty or has
// no entry with a thumbnail — the TMDB fallback trigger condition.
func peopleThumbsMissing(people []store.Person) bool {
	for _, p := range people {
		if p.Thumb != "" {
			return false
		}
	}
	return true
}

// ---- TMDB movie search + match classification (batch 29) ----
//
// Used by the Alist remote scan to attach a TMDB id to a parsed
// folder title. The classification mirrors the validated prototype:
// only a normalised EXACT title/original_title hit among the first
// five candidates counts, and only when the year agrees (diff ≤ 1
// or either side unknown). A year conflict or a mere partial
// containment leaves the item unmatched (no TMDB id guessed).

type tmdbSearchHit struct {
	ID            int
	Title         string
	OriginalTitle string
	Year          int
}

// searchTMDBMovie queries /search/movie (zh-CN) for title,
// constrained to year when year > 0, and returns up to 5 hits.
func searchTMDBMovie(cfg tmdbConfig, title string, year int) ([]tmdbSearchHit, error) {
	q := url.Values{}
	q.Set("query", title)
	q.Set("language", "zh-CN")
	if year > 0 {
		q.Set("primary_release_year", fmt.Sprint(year))
	}
	resp, err := cfg.get(tmdbClient, "/search/movie?"+q.Encode())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tmdb search: status %d", resp.StatusCode)
	}
	var d struct {
		Results []struct {
			ID            int    `json:"id"`
			Title         string `json:"title"`
			OriginalTitle string `json:"original_title"`
			ReleaseDate   string `json:"release_date"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, err
	}
	hits := make([]tmdbSearchHit, 0, len(d.Results))
	for _, r := range d.Results {
		y := 0
		if len(r.ReleaseDate) >= 4 {
			fmt.Sscanf(r.ReleaseDate[:4], "%d", &y)
		}
		hits = append(hits, tmdbSearchHit{ID: r.ID, Title: r.Title, OriginalTitle: r.OriginalTitle, Year: y})
		if len(hits) >= 5 {
			break
		}
	}
	return hits, nil
}

// normTMDBTitle normalises a title for equality comparison:
// lowercased, with spaces and common punctuation removed.
func normTMDBTitle(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch r {
		case ' ', '\t', '·', '.', '-', '_', ':', '：', '(', ')', '（', '）', '[', ']', '【', '】', '\'', '"', ',', '，':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// classifyTMDBHits decides the match for (title, year) against the
// search candidates. It returns the matched TMDB id, or 0 when no
// candidate qualifies (unmatched: exact-title year conflict,
// partial-only containment, or no candidates at all).
func classifyTMDBHits(title string, year int, hits []tmdbSearchHit) int {
	want := normTMDBTitle(title)
	if want == "" {
		return 0
	}
	for _, h := range hits {
		if normTMDBTitle(h.Title) != want && normTMDBTitle(h.OriginalTitle) != want {
			continue
		}
		// Exact title: the year must agree (either side unknown is
		// tolerated, a real conflict is not).
		if year == 0 || h.Year == 0 || absInt(year-h.Year) <= 1 {
			return h.ID
		}
	}
	return 0
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// matchTMDBMovie searches TMDB for (title, year) and classifies the
// result. When a year-constrained search finds no exact match it
// retries once without the year (folder years are occasionally off
// by the release-vs-production year). 0 = unmatched.
func matchTMDBMovie(cfg tmdbConfig, title string, year int) (int, error) {
	hits, err := searchTMDBMovie(cfg, title, year)
	if err != nil {
		return 0, err
	}
	if id := classifyTMDBHits(title, year, hits); id > 0 {
		return id, nil
	}
	if year > 0 {
		hits, err = searchTMDBMovie(cfg, title, 0)
		if err != nil {
			return 0, err
		}
		if id := classifyTMDBHits(title, year, hits); id > 0 {
			return id, nil
		}
	}
	return 0, nil
}
