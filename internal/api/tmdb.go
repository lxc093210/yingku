package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"yingku/internal/store"
)

// tmdbClient fetches cast data from TMDB as a fallback when local NFO
// metadata carries no cast (or no cast photos).
var tmdbClient = &http.Client{Timeout: 6 * time.Second}

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
func fetchTMDBCast(apiKey, kind string, tmdbID int) ([]store.Person, error) {
	url := fmt.Sprintf("https://api.themoviedb.org/3/%s/%d/credits?api_key=%s",
		kind, tmdbID, apiKey)
	resp, err := tmdbClient.Get(url)
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
			thumb = "https://image.tmdb.org/t/p/w185" + c.ProfilePath
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

// peopleWithThumbsMissing reports whether a people list is empty or has
// no entry with a thumbnail — the TMDB fallback trigger condition.
func peopleThumbsMissing(people []store.Person) bool {
	for _, p := range people {
		if p.Thumb != "" {
			return false
		}
	}
	return true
}
