package meta

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Alist folder-name parsing (batch 29). Xiaoya/Alist title folders
// look like `12勇士 4K REMUX（2018）`: a Chinese title, a quality
// tail and a full-width-parenthesised year — no {tmdbid}. The rules
// below are the ones validated against the live /电影/4K REMUX
// catalog (1,632 folders) by the batch-29 prototype:
//   - full-width parens/brackets/spaces are normalised first;
//   - the year comes ONLY from a (YYYY) parenthesis — bare leading
//     digits are part of the title (《2012》《1917》);
//   - trailing quality tokens are stripped repeatedly.

var alistYearRe = regexp.MustCompile(`\((\d{4})\)`)

var alistNormalizer = strings.NewReplacer(
	"（", "(", "）", ")", "【", "[", "】", "]", "　", " ",
)

// alistQualityTokens are stripped from the END of a folder name,
// longest/most specific first, repeatedly, until none matches.
var alistQualityTokens = []string{
	"4K REMUX", "REMUX", "BLURAY", "WEB-DL", "WEBDL", "杜比视界",
	"DOLBY VISION", "DV", "HDR10+", "HDR10", "HDR", "UHD",
	"2160P", "1080P", "720P", "HEVC", "X265", "H265", "X264", "H264",
	"ATMOS", "TRUEHD", "DTS-HD", "DTS", "DDP", "DD+", "4K", "BD",
	"HQ", "高清", "国语", "粤语", "中字", "双语",
}

// ParseAlistFolderName extracts (title, year) from an Alist title
// folder name. Year is 0 when no parenthesised year is present.
func ParseAlistFolderName(name string) (string, int) {
	s := alistNormalizer.Replace(name)
	year := 0
	if m := alistYearRe.FindStringSubmatchIndex(s); m != nil {
		y := atoi4(s[m[2]:m[3]])
		if y >= 1900 && y <= 2099 {
			year = y
			s = s[:m[0]] + s[m[1]:]
		}
	}
	s = strings.Trim(s, " ·-")
	for {
		next, ok := stripAlistQualitySuffix(s)
		if !ok {
			break
		}
		s = next
	}
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(s), year
}

func atoi4(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// stripAlistQualitySuffix removes one trailing quality token
// (case-insensitive) plus surrounding separators.
func stripAlistQualitySuffix(s string) (string, bool) {
	up := strings.ToUpper(s)
	for _, q := range alistQualityTokens {
		if !strings.HasSuffix(up, q) {
			continue
		}
		n := utf8.RuneCountInString(q)
		r := []rune(s)
		if len(r) < n {
			continue
		}
		return strings.Trim(string(r[:len(r)-n]), " ·-"), true
	}
	return s, false
}

// alistPackTailRe matches the "N部" pack suffix of multi-film
// bundles （哈利波特 8部， 叶问4部）: a digit run plus 部 at the very
// end of the (normalised) name.
var alistPackTailRe = regexp.MustCompile(`\d+\s*部\s*$`)

// IsAlistCollectionFolder reports whether a folder name denotes a
// collection （系列/合集/N部 packs) rather than a single title;
// remote scans skip those.
func IsAlistCollectionFolder(name string) bool {
	if strings.Contains(name, "系列") || strings.Contains(name, "合集") {
		return true
	}
	s := alistNormalizer.Replace(name)
	loc := alistPackTailRe.FindStringIndex(s)
	if loc == nil {
		return false
	}
	// 第N部 is ONE film (episode N of a series), not a pack.
	if loc[0] > 0 {
		if r, _ := utf8.DecodeLastRuneInString(s[:loc[0]]); r == '第' {
			return false
		}
	}
	return true
}
