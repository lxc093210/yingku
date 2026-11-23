package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"yingku/internal/store"
)

// Subtitle tracks for the web player. Sidecar subtitle files (.srt/.ass/
// .ssa) registered in the subtitles table are converted to WebVTT on the
// fly so the browser's native CC menu can switch between them.

type subtitleTrackDTO struct {
	ID     int64  `json:"Id"`
	Lang   string `json:"Lang"`
	Label  string `json:"Label"`
	Format string `json:"Format"`
}

// langNames maps a BCP-47-ish code to the label shown in the CC menu.
var langNames = map[string]string{
	"zh-cn": "简体中文",
	"zh-tw": "繁體中文",
	"en":    "English",
	"ja":    "日本語",
	"ko":    "한국어",
	"und":   "未知语言",
}

// subtitleLang infers the track language from the file name: release
// names carry tokens like .chi/.chs/.cht/.zh-cn/.zh-tw/.eng/.jpn either
// dot- or dash-separated. Falls back to the scanner-recorded lang column
// (same token family), then "und".
func subtitleLang(sub store.Subtitle) string {
	if l := langFromTokens(sub.Path); l != "" {
		return l
	}
	switch strings.ToLower(sub.Lang) {
	case "zh-cn", "chs", "chi", "zho", "zh-hans":
		return "zh-cn"
	case "zh-tw", "cht", "zh-hant":
		return "zh-tw"
	case "eng":
		return "en"
	case "jpn":
		return "ja"
	case "kor":
		return "ko"
	}
	return "und"
}

func langFromTokens(path string) string {
	name := strings.ToLower(filepath.Base(path))
	name = strings.TrimSuffix(name, filepath.Ext(name))
	parts := strings.FieldsFunc(name, func(r rune) bool {
		return r == '.' || r == '-' || r == '_' || r == ' ' || r == '[' || r == ']'
	})
	set := map[string]bool{}
	for i, p := range parts {
		set[p] = true
		if i+1 < len(parts) {
			set[p+"-"+parts[i+1]] = true // re-join zh-cn / zh-tw style pairs
		}
	}
	switch {
	case set["zh-tw"] || set["cht"]:
		return "zh-tw"
	case set["zh-cn"] || set["chs"] || set["chi"] || set["zho"]:
		return "zh-cn"
	case set["eng"]:
		return "en"
	case set["jpn"]:
		return "ja"
	case set["kor"]:
		return "ko"
	}
	return ""
}

// subtitleFormat returns the lowercase extension without the dot.
func subtitleFormat(path string) string {
	return strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
}

func (s *Server) handleItemSubtitles(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad item id")
		return
	}
	it, err := s.st.GetItem(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if it == nil {
		writeErr(w, http.StatusNotFound, "item not found")
		return
	}
	subs, err := s.st.SubtitlesOf(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []subtitleTrackDTO{}
	seen := map[string]int{}
	for _, sub := range subs {
		lang := subtitleLang(sub)
		label := langNames[lang]
		seen[lang]++
		if seen[lang] > 1 {
			label = fmt.Sprintf("%s %d", label, seen[lang])
		}
		out = append(out, subtitleTrackDTO{
			ID: sub.ID, Lang: lang, Label: label, Format: subtitleFormat(sub.Path),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSubtitleTrack(w http.ResponseWriter, r *http.Request) {
	subID, err := strconv.ParseInt(r.PathValue("subId"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad subtitle id")
		return
	}
	sub, err := s.st.GetSubtitle(subID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sub == nil {
		writeErr(w, http.StatusNotFound, "subtitle not found")
		return
	}
	clean := filepath.Clean(sub.Path)
	allowed, err := s.pathWithinRoots(clean)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !allowed {
		writeErr(w, http.StatusForbidden, "path outside library")
		return
	}
	raw, err := os.ReadFile(clean)
	if err != nil {
		writeErr(w, http.StatusNotFound, "subtitle file missing")
		return
	}
	var vtt string
	switch subtitleFormat(clean) {
	case "srt":
		vtt = srtToVTT(string(raw))
	case "ass", "ssa":
		vtt, err = assToVTT(string(raw))
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	case "vtt":
		vtt = string(raw) // already WebVTT
	default:
		writeErr(w, http.StatusUnsupportedMediaType, "unsupported subtitle format")
		return
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	_, _ = w.Write([]byte(vtt))
}

// ---- SRT -> WebVTT ----

var srtTimeRe = regexp.MustCompile(`(\d{1,2}:\d{2}:\d{2}),(\d{1,3})`)

// srtToVTT converts SRT text to WebVTT: adds the header and rewrites the
// decimal comma in cue timestamps to a dot. Cue payloads pass through
// unchanged.
func srtToVTT(s string) string {
	s = strings.TrimPrefix(s, "\ufeff")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "-->") {
			line = srtTimeRe.ReplaceAllString(line, "$1.$2")
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// ---- ASS/SSA -> WebVTT ----

// assTimeToVTT converts an ASS timestamp (H:MM:SS.cc, centiseconds) to
// WebVTT form (HH:MM:SS.mmm).
func assTimeToVTT(t string) (string, error) {
	parts := strings.Split(strings.TrimSpace(t), ":")
	if len(parts) != 3 {
		return "", fmt.Errorf("bad ass time %q", t)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil {
		return "", fmt.Errorf("bad ass time %q", t)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", fmt.Errorf("bad ass time %q", t)
	}
	secParts := strings.SplitN(parts[2], ".", 2)
	sec, err := strconv.Atoi(secParts[0])
	if err != nil {
		return "", fmt.Errorf("bad ass time %q", t)
	}
	cs := 0
	if len(secParts) == 2 {
		if cs, err = strconv.Atoi(secParts[1]); err != nil {
			return "", fmt.Errorf("bad ass time %q", t)
		}
	}
	totalMS := ((h*60+m)*60+sec)*1000 + cs*10
	return fmt.Sprintf("%02d:%02d:%02d.%03d",
		totalMS/3600000, totalMS/60000%60, totalMS/1000%60, totalMS%1000), nil
}

var assOverrideRe = regexp.MustCompile(`\{[^}]*\}`)

// assTextToPlain strips override blocks and converts ASS escapes.
func assTextToPlain(t string) string {
	t = assOverrideRe.ReplaceAllString(t, "")
	t = strings.ReplaceAll(t, `\N`, "\n")
	t = strings.ReplaceAll(t, `\n`, "\n")
	t = strings.ReplaceAll(t, `\h`, " ")
	return strings.TrimSpace(t)
}

// assToVTT converts ASS/SSA text to WebVTT using the [Events] Format
// field order; only Dialogue lines become cues.
func assToVTT(s string) (string, error) {
	s = strings.TrimPrefix(s, "\ufeff")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	inEvents := false
	var format []string
	startIdx, endIdx, textIdx := 1, 2, 9 // ASS defaults if Format is absent
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	cues := 0
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			inEvents = strings.EqualFold(trimmed, "[Events]")
			continue
		}
		if !inEvents {
			continue
		}
		if strings.HasPrefix(trimmed, "Format:") {
			format = nil
			for _, f := range strings.Split(strings.TrimSpace(strings.TrimPrefix(trimmed, "Format:")), ",") {
				format = append(format, strings.ToLower(strings.TrimSpace(f)))
			}
			for i, f := range format {
				switch f {
				case "start":
					startIdx = i
				case "end":
					endIdx = i
				case "text":
					textIdx = i
				}
			}
			continue
		}
		if !strings.HasPrefix(trimmed, "Dialogue:") {
			continue
		}
		if len(format) == 0 {
			format = make([]string, 10) // defaults already in the indices
		}
		body := strings.TrimSpace(strings.TrimPrefix(trimmed, "Dialogue:"))
		// The first len(format)-1 commas are field separators; the Text
		// field itself may contain commas.
		fields := strings.SplitN(body, ",", len(format))
		if len(fields) <= startIdx || len(fields) <= endIdx || len(fields) <= textIdx {
			continue
		}
		start, err := assTimeToVTT(fields[startIdx])
		if err != nil {
			continue
		}
		end, err := assTimeToVTT(fields[endIdx])
		if err != nil {
			continue
		}
		text := assTextToPlain(fields[textIdx])
		if text == "" {
			continue
		}
		fmt.Fprintf(&b, "%s --> %s\n%s\n\n", start, end, text)
		cues++
	}
	if cues == 0 {
		return "", fmt.Errorf("no dialogue cues found")
	}
	return b.String(), nil
}
