package meta

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// MediaInfo is the normalized media description attached to a media source.
// Source records provenance: "json" (sidecar), "filename" (inferred from
// file-name tokens) or "none".
type MediaInfo struct {
	Width        int
	Height       int
	VideoCodec   string
	HDR          string
	AudioSummary string
	RuntimeMin   int
	Source       string
}

// MediaInfoProvider resolves media info for a .strm file. Phase 1 ships only
// the local sidecar implementation; a later phase may add a provider that
// fetches missing info from NanShare's central server for items whose local
// lookup yields Source="none". Providers are tried in order; the first one
// returning ok=true wins.
type MediaInfoProvider interface {
	Lookup(strmPath string) (MediaInfo, bool)
}

// SidecarProvider reads the "<strm stem>-mediainfo.json" sidecar written by
// the NanShare MediaInfo plugin (NanShare.MediaInfo.dll). The file is a JSON
// array of Emby-style MediaSourceInfo objects.
type SidecarProvider struct{}

// SidecarPath returns the mediainfo sidecar path for a .strm file.
func SidecarPath(strmPath string) string {
	stem := strings.TrimSuffix(strmPath, filepath.Ext(strmPath))
	return stem + "-mediainfo.json"
}

type sidecarFile []struct {
	MediaSourceInfo struct {
		RunTimeTicks int64 `json:"RunTimeTicks"`
		MediaStreams []struct {
			Type              string `json:"Type"`
			Codec             string `json:"Codec"`
			Width             int    `json:"Width"`
			Height            int    `json:"Height"`
			VideoRange        string `json:"VideoRange"`
			ExtendedVideoType string `json:"ExtendedVideoType"`
			Language          string `json:"Language"`
			Channels          int    `json:"Channels"`
			ChannelLayout     string `json:"ChannelLayout"`
			DisplayTitle      string `json:"DisplayTitle"`
		} `json:"MediaStreams"`
	} `json:"MediaSourceInfo"`
}

func (SidecarProvider) Lookup(strmPath string) (MediaInfo, bool) {
	raw, err := os.ReadFile(SidecarPath(strmPath))
	if err != nil {
		return MediaInfo{}, false
	}
	mi, err := ParseSidecar(raw)
	if err != nil {
		return MediaInfo{}, false
	}
	return mi, true
}

// ParseSidecar decodes a NanShare MediaInfo plugin sidecar document.
func ParseSidecar(raw []byte) (MediaInfo, error) {
	var doc sidecarFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		return MediaInfo{}, err
	}
	if len(doc) == 0 {
		return MediaInfo{}, fmt.Errorf("empty mediainfo sidecar")
	}
	mi := MediaInfo{Source: "json"}
	src := doc[0].MediaSourceInfo
	if src.RunTimeTicks > 0 {
		mi.RuntimeMin = int((src.RunTimeTicks/10_000_000 + 30) / 60)
	}
	var audios []string
	subs := 0
	for _, st := range src.MediaStreams {
		switch st.Type {
		case "Video":
			if mi.VideoCodec == "" {
				mi.VideoCodec = strings.ToLower(st.Codec)
				mi.Width = st.Width
				mi.Height = st.Height
				mi.HDR = hdrMarker(st.VideoRange, st.ExtendedVideoType)
			}
		case "Audio":
			audios = append(audios, audioSummary(st.Codec, st.ChannelLayout, st.Channels, st.Language))
		case "Subtitle":
			subs++
		}
	}
	mi.AudioSummary = strings.Join(audios, ", ")
	if subs > 0 {
		if mi.AudioSummary != "" {
			mi.AudioSummary += " "
		}
		mi.AudioSummary += fmt.Sprintf("· %d subs", subs)
	}
	return mi, nil
}

// hdrMarker normalizes the HDR marker: VideoRange wins unless it is SDR,
// in which case a non-None ExtendedVideoType may still carry the marker.
func hdrMarker(videoRange, extended string) string {
	if videoRange != "" && !strings.EqualFold(videoRange, "SDR") {
		return videoRange
	}
	if extended != "" && !strings.EqualFold(extended, "None") {
		return extended
	}
	return ""
}

func audioSummary(codec, layout string, channels int, lang string) string {
	s := strings.ToLower(codec)
	if layout == "" {
		switch channels {
		case 8:
			layout = "7.1"
		case 6:
			layout = "5.1"
		case 2:
			layout = "2.0"
		case 1:
			layout = "1.0"
		}
	}
	if layout != "" {
		s += " " + layout
	}
	if lang != "" {
		s += " (" + lang + ")"
	}
	return s
}

var (
	resTokenRe = regexp.MustCompile(`(?i)\b(2160p|1080p|720p|576p|480p|4k|uhd)\b`)
	hdrDVRe    = regexp.MustCompile(`(?i)dolby\s*vision|\bdv\b|dovi`)
	hdr10pRe   = regexp.MustCompile(`(?i)hdr10\+`)
	hdr10Re    = regexp.MustCompile(`(?i)hdr10`)
	hdrRe      = regexp.MustCompile(`(?i)\bhdr\b`)
)

// InferFromFilename guesses resolution/codec/HDR from release-name tokens.
// Used only when no sidecar exists; the result carries Source="filename",
// or Source="none" when nothing could be inferred.
func InferFromFilename(name string) MediaInfo {
	mi := MediaInfo{Source: "none"}
	switch strings.ToLower(resTokenRe.FindString(name)) {
	case "2160p", "4k", "uhd":
		mi.Width, mi.Height = 3840, 2160
	case "1080p":
		mi.Width, mi.Height = 1920, 1080
	case "720p":
		mi.Width, mi.Height = 1280, 720
	case "576p":
		mi.Width, mi.Height = 1024, 576
	case "480p":
		mi.Width, mi.Height = 854, 480
	}
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "hevc"), strings.Contains(lower, "h.265"), strings.Contains(lower, "h265"), strings.Contains(lower, "x265"):
		mi.VideoCodec = "hevc"
	case strings.Contains(lower, "av1"):
		mi.VideoCodec = "av1"
	case strings.Contains(lower, "h.264"), strings.Contains(lower, "h264"), strings.Contains(lower, "x264"), strings.Contains(lower, "avc"):
		mi.VideoCodec = "h264"
	}
	switch {
	case hdrDVRe.MatchString(name):
		mi.HDR = "DolbyVision"
	case hdr10pRe.MatchString(name):
		mi.HDR = "HDR10Plus"
	case hdr10Re.MatchString(name):
		mi.HDR = "HDR10"
	case hdrRe.MatchString(name):
		mi.HDR = "HDR"
	}
	if mi.Width > 0 || mi.VideoCodec != "" || mi.HDR != "" {
		mi.Source = "filename"
	}
	return mi
}

// ResolveMediaInfo tries providers in order, then filename inference.
func ResolveMediaInfo(strmPath string, providers []MediaInfoProvider) MediaInfo {
	for _, p := range providers {
		if mi, ok := p.Lookup(strmPath); ok {
			return mi
		}
	}
	return InferFromFilename(filepath.Base(strmPath))
}
