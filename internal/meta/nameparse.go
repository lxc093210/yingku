package meta

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	tmdbBraceRe = regexp.MustCompile(`\{tmdbid[-=](\d+)\}`)
	tmdbAltRe   = regexp.MustCompile(`\[tmdbid[-=](\d+)\]`)
	yearRe      = regexp.MustCompile(`\((\d{4})\)`)
	seasonDirRe = regexp.MustCompile(`(?i)^season\s*(\d+)$`)
	seasonAltRe = regexp.MustCompile(`(?i)^s(\d{1,2})$`)
	sxxExxRe    = regexp.MustCompile(`(?i)S(\d{1,2})E(\d{1,3})`)
)

// FolderInfo is the identity parsed from an item folder name such as
// "蜘蛛侠：平行宇宙 (2018) {tmdbid-324857}".
type FolderInfo struct {
	Title string
	Year  int
	Tmdb  int
}

// ParseFolderName extracts {tmdbid-N}, (YYYY) and the clean title from a
// folder or file base name.
func ParseFolderName(name string) FolderInfo {
	var fi FolderInfo
	if m := tmdbBraceRe.FindStringSubmatch(name); m != nil {
		fi.Tmdb, _ = strconv.Atoi(m[1])
		name = tmdbBraceRe.ReplaceAllString(name, "")
	} else if m := tmdbAltRe.FindStringSubmatch(name); m != nil {
		fi.Tmdb, _ = strconv.Atoi(m[1])
		name = tmdbAltRe.ReplaceAllString(name, "")
	}
	if m := yearRe.FindStringSubmatch(name); m != nil {
		fi.Year, _ = strconv.Atoi(m[1])
		name = yearRe.ReplaceAllString(name, "")
	}
	fi.Title = strings.TrimSpace(strings.Join(strings.Fields(name), " "))
	return fi
}

// ParseSeasonDir returns the season number for folder names like
// "Season 1", "Season 01", "S01", "Specials" (0). ok=false if not a season dir.
func ParseSeasonDir(name string) (num int, ok bool) {
	if strings.EqualFold(name, "Specials") {
		return 0, true
	}
	if m := seasonDirRe.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n, true
	}
	if m := seasonAltRe.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n, true
	}
	return 0, false
}

// ParseSxExx extracts season/episode numbers from a file name (S01E09).
func ParseSxExx(name string) (season, episode int, ok bool) {
	m := sxxExxRe.FindStringSubmatch(name)
	if m == nil {
		return 0, 0, false
	}
	season, _ = strconv.Atoi(m[1])
	episode, _ = strconv.Atoi(m[2])
	return season, episode, true
}
