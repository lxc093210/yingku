package meta

import (
	"bytes"
	"encoding/xml"
	"os"
	"strconv"
	"strings"
)

// NFO holds the subset of Kodi/MoviePilot NFO metadata yingku consumes.
// The same struct decodes <movie>, <tvshow>, <season> and <episodedetails>
// roots (no XMLName field, so the root element is not enforced).
type NFO struct {
	Title         string  `xml:"title"`
	OriginalTitle string  `xml:"originaltitle"`
	Plot          string  `xml:"plot"`
	Outline       string  `xml:"outline"`
	SortTitle     string  `xml:"sorttitle"`
	Rating        float64 `xml:"rating"`
	Ratings       struct {
		Rating []struct {
			Name  string  `xml:"name,attr"`
			Value float64 `xml:"value"`
		} `xml:"rating"`
	} `xml:"ratings"`
	Year      int      `xml:"year"`
	Premiered string   `xml:"premiered"`
	Release   string   `xml:"releasedate"`
	Aired     string   `xml:"aired"`
	Runtime   int      `xml:"runtime"`
	Genres    []string `xml:"genre"`
	TmdbID    int      `xml:"tmdbid"`
	Season    int      `xml:"season"`
	SeasonNum int      `xml:"seasonnumber"`
	Episode   int      `xml:"episode"`
	Actors    []Actor  `xml:"actor"`
}

// Actor is one <actor> entry of an NFO (cast member).
type Actor struct {
	Name  string `xml:"name"`
	Role  string `xml:"role"`
	Type  string `xml:"type"`
	Thumb string `xml:"thumb"`
}

// MaxActors caps how many cast entries are kept per item.
const MaxActors = 30

// Date returns the best available date string (premiered > releasedate > aired).
func (n *NFO) Date() string {
	for _, d := range []string{n.Premiered, n.Release, n.Aired} {
		if strings.TrimSpace(d) != "" {
			return strings.TrimSpace(d)
		}
	}
	return ""
}

// RatingValue returns the direct <rating> or the first <ratings> entry.
func (n *NFO) RatingValue() float64 {
	if n.Rating > 0 {
		return n.Rating
	}
	for _, r := range n.Ratings.Rating {
		if r.Value > 0 {
			return r.Value
		}
	}
	return 0
}

// SeasonNumber resolves season from <season> or <seasonnumber>.
func (n *NFO) SeasonNumber() int {
	if n.Season > 0 {
		return n.Season
	}
	return n.SeasonNum
}

// ParseNFOFile reads and decodes an NFO file (UTF-8 BOM tolerant, CDATA ok).
func ParseNFOFile(path string) (*NFO, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseNFO(raw)
}

// ParseNFO decodes NFO XML bytes.
func ParseNFO(raw []byte) (*NFO, error) {
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	var n NFO
	if err := xml.Unmarshal(raw, &n); err != nil {
		return nil, err
	}
	// Normalize the cast list: trimmed, nameless entries dropped, capped.
	actors := n.Actors[:0]
	for _, a := range n.Actors {
		a.Name = strings.TrimSpace(a.Name)
		if a.Name == "" {
			continue
		}
		a.Role = strings.TrimSpace(a.Role)
		a.Type = strings.TrimSpace(a.Type)
		a.Thumb = strings.TrimSpace(a.Thumb)
		actors = append(actors, a)
		if len(actors) >= MaxActors {
			break
		}
	}
	n.Actors = actors
	n.Title = strings.TrimSpace(n.Title)
	n.OriginalTitle = strings.TrimSpace(n.OriginalTitle)
	if strings.TrimSpace(n.Plot) == "" {
		n.Plot = n.Outline
	}
	n.Plot = strings.TrimSpace(n.Plot)
	// Some NFOs carry year only inside the premiered date.
	if n.Year == 0 {
		if d := n.Date(); len(d) >= 4 {
			if y, err := strconv.Atoi(d[:4]); err == nil {
				n.Year = y
			}
		}
	}
	return &n, nil
}
