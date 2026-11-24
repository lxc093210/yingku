package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ffprobe fallback (batch 28): when an item has no -mediainfo.json
// sidecar and filename inference is all there is, the mediainfo job
// probes the real file through its STRM target URL (the 8115 resolver
// address) with ffprobe's header-only read and stores the result with
// Source="probe". The mapping below produces exactly the MediaInfo /
// StreamInfo shapes ParseSidecar produces from the NanShare plugin's
// JSON, so downstream consumers cannot tell the two apart.

type ffprobeStream struct {
	Index          int    `json:"index"`
	CodecName      string `json:"codec_name"`
	CodecType      string `json:"codec_type"`
	CodecTagString string `json:"codec_tag_string"`
	Profile        string `json:"profile"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	PixFmt         string `json:"pix_fmt"`
	ColorSpace     string `json:"color_space"`
	ColorTransfer  string `json:"color_transfer"`
	ColorPrimaries string `json:"color_primaries"`
	BitRate        string `json:"bit_rate"`
	AvgFrameRate   string `json:"avg_frame_rate"`
	RFrameRate     string `json:"r_frame_rate"`
	SampleRate     string `json:"sample_rate"`
	Channels       int    `json:"channels"`
	ChannelLayout  string `json:"channel_layout"`
	Tags           struct {
		Language string `json:"language"`
	} `json:"tags"`
	SideDataList []struct {
		SideDataType string `json:"side_data_type"`
	} `json:"side_data_list"`
}

type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		Size       string `json:"size"`
		BitRate    string `json:"bit_rate"`
	} `json:"format"`
}

// FFprobeAvailable reports whether an ffprobe binary is on PATH.
func FFprobeAvailable() bool {
	_, err := exec.LookPath("ffprobe")
	return err == nil
}

// ProbeURL runs ffprobe against url (a media file URL, typically the
// STRM target) and maps the result into MediaInfo with Source
// "probe". The context bounds the whole probe; callers pass a ~30s
// timeout.
func ProbeURL(ctx context.Context, url string) (MediaInfo, error) {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error", "-show_streams", "-show_format", "-of", "json", url)
	out, err := cmd.Output()
	if err != nil {
		return MediaInfo{}, fmt.Errorf("ffprobe: %w", err)
	}
	return ParseFFprobe(out)
}

// ParseFFprobe maps ffprobe's JSON output into MediaInfo. Split from
// ProbeURL so the mapping is unit-testable without a real file.
func ParseFFprobe(raw []byte) (MediaInfo, error) {
	var doc ffprobeOutput
	if err := json.Unmarshal(raw, &doc); err != nil {
		return MediaInfo{}, err
	}
	if len(doc.Streams) == 0 {
		return MediaInfo{}, fmt.Errorf("ffprobe: no streams")
	}
	mi := MediaInfo{Source: "probe"}
	mi.Size, _ = strconv.ParseInt(doc.Format.Size, 10, 64)
	mi.Bitrate, _ = strconv.Atoi(doc.Format.BitRate)
	if dur, err := strconv.ParseFloat(doc.Format.Duration, 64); err == nil && dur > 0 {
		mi.RuntimeMin = int((int64(dur) + 30) / 60)
	}
	if name := doc.Format.FormatName; name != "" {
		mi.Container = strings.Split(name, ",")[0]
	}
	for _, fs := range doc.Streams {
		st := StreamInfo{
			Index:            fs.Index,
			Codec:            fs.CodecName,
			CodecTag:         fs.CodecTagString,
			Language:         fs.Tags.Language,
			Width:            fs.Width,
			Height:           fs.Height,
			Profile:          fs.Profile,
			AverageFrameRate: parseFrameRate(fs.AvgFrameRate),
			RealFrameRate:    parseFrameRate(fs.RFrameRate),
			Channels:         fs.Channels,
			ChannelLayout:    fs.ChannelLayout,
			ColorPrimaries:   fs.ColorPrimaries,
			ColorSpace:       fs.ColorSpace,
			ColorTransfer:    fs.ColorTransfer,
			PixelFormat:      fs.PixFmt,
		}
		st.BitRate, _ = strconv.Atoi(fs.BitRate)
		st.SampleRate, _ = strconv.Atoi(fs.SampleRate)
		switch fs.CodecType {
		case "video":
			st.Type = "Video"
			st.VideoRange, st.ExtendedVideoType = probeHDR(fs)
		case "audio":
			st.Type = "Audio"
		case "subtitle":
			st.Type = "Subtitle"
		default:
			st.Type = fs.CodecType
		}
		mi.Streams = append(mi.Streams, st)
	}
	// Summary fields, assembled exactly like ParseSidecar's.
	var audios []string
	subs := 0
	for _, st := range mi.Streams {
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

// probeHDR derives the (VideoRange, ExtendedVideoType) pair the
// sidecar would carry, from ffprobe's color metadata and side data:
//
//   - a DOVI configuration record (or a Dolby Vision profile) means
//     Dolby Vision;
//   - SMPTE ST 2094 side data on an smpte2084 stream means HDR10+;
//   - smpte2084 (PQ) alone means HDR10; arib-std-b67 means HLG;
//   - anything else is SDR, which hdrMarker normalizes to "".
func probeHDR(fs ffprobeStream) (videoRange, extended string) {
	dovi := strings.Contains(strings.ToLower(fs.Profile), "dolby vision")
	hdr10plus := false
	for _, sd := range fs.SideDataList {
		t := strings.ToLower(sd.SideDataType)
		if strings.Contains(t, "dovi") || strings.Contains(t, "dolby") {
			dovi = true
		}
		if strings.Contains(t, "2094") {
			hdr10plus = true
		}
	}
	switch {
	case dovi:
		return "DolbyVision", ""
	case hdr10plus && fs.ColorTransfer == "smpte2084":
		return "HDR10Plus", ""
	case fs.ColorTransfer == "smpte2084":
		return "HDR10", ""
	case fs.ColorTransfer == "arib-std-b67":
		return "HLG", ""
	}
	return "SDR", ""
}

// parseFrameRate converts ffprobe's "num/den" frame rate to a float.
func parseFrameRate(s string) float64 {
	if s == "" {
		return 0
	}
	num, den, ok := strings.Cut(s, "/")
	if !ok {
		f, _ := strconv.ParseFloat(s, 64)
		return f
	}
	n, err1 := strconv.ParseFloat(num, 64)
	d, err2 := strconv.ParseFloat(den, 64)
	if err1 != nil || err2 != nil || d == 0 {
		return 0
	}
	return n / d
}
