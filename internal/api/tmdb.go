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
	resp, err := tmdbDownloadClient.Get(c.imageBase + "/t/p/original" + filePath)
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
