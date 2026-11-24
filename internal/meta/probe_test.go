package meta

import "testing"

const ffprobeHDR10 = `{
  "streams": [
    {"index":0,"codec_name":"hevc","codec_type":"video","codec_tag_string":"hvc1",
     "width":3840,"height":2160,"pix_fmt":"yuv420p10le","profile":"Main 10",
     "r_frame_rate":"24000/1001","avg_frame_rate":"24000/1001",
     "color_space":"bt2020nc","color_transfer":"smpte2084","color_primaries":"bt2020",
     "bit_rate":"25000000",
     "disposition":{"default":1}},
    {"index":1,"codec_name":"aac","codec_type":"audio","codec_tag_string":"mp4a",
     "sample_rate":"48000","channels":2,"channel_layout":"stereo","bit_rate":"192000",
     "tags":{"language":"chi"},"disposition":{"default":1}},
    {"index":2,"codec_name":"mov_text","codec_type":"subtitle",
     "tags":{"language":"chi"},"disposition":{"default":0}}
  ],
  "format": {"filename":"http://x/f.mkv","format_name":"matroska,webm",
             "duration":"6120.5","size":"19000000000","bit_rate":"24800000"}
}`

const ffprobeDV = `{
  "streams": [
    {"index":0,"codec_name":"hevc","codec_type":"video","width":3840,"height":1608,
     "pix_fmt":"yuv420p10le","color_transfer":"smpte2084",
     "side_data_list":[{"side_data_type":"DOVI configuration record","dv_profile":8}],
     "disposition":{"default":1}},
    {"index":1,"codec_name":"eac3","codec_type":"audio","sample_rate":"48000",
     "channels":6,"channel_layout":"5.1(side)","tags":{"language":"eng"},
     "disposition":{"default":1}}
  ],
  "format": {"format_name":"matroska,webm","duration":"3600","size":"9000000000","bit_rate":"20000000"}
}`

func TestParseFFprobeHDR10(t *testing.T) {
	info, err := ParseFFprobe([]byte(ffprobeHDR10))
	if err != nil {
		t.Fatal(err)
	}
	if info.Source != "probe" {
		t.Errorf("source = %q, want probe", info.Source)
	}
	if info.Width != 3840 || info.Height != 2160 || info.VideoCodec != "hevc" {
		t.Errorf("video = %dx%d %q", info.Width, info.Height, info.VideoCodec)
	}
	if info.HDR != "HDR10" {
		t.Errorf("hdr = %q, want HDR10", info.HDR)
	}
	if info.Container != "matroska" || info.RuntimeMin != 102 {
		t.Errorf("container/runtime = %q/%d", info.Container, info.RuntimeMin)
	}
	if info.Size != 19000000000 || info.Bitrate != 24800000 {
		t.Errorf("size/bitrate = %d/%d", info.Size, info.Bitrate)
	}
	if info.AudioSummary != "aac stereo (chi) · 1 subs" {
		t.Errorf("audio summary = %q", info.AudioSummary)
	}
	if len(info.Streams) != 3 {
		t.Fatalf("streams = %d, want 3", len(info.Streams))
	}
	v := info.Streams[0]
	if v.Type != "Video" || v.Codec != "hevc" || v.Width != 3840 || v.ColorTransfer != "smpte2084" || v.AverageFrameRate < 23 || v.AverageFrameRate > 24 {
		t.Errorf("video stream = %+v", v)
	}
	a := info.Streams[1]
	if a.Type != "Audio" || a.Language != "chi" || a.SampleRate != 48000 || a.Channels != 2 {
		t.Errorf("audio stream = %+v", a)
	}
	if info.Streams[2].Type != "Subtitle" {
		t.Errorf("subtitle stream = %+v", info.Streams[2])
	}
}

func TestParseFFprobeDolbyVision(t *testing.T) {
	info, err := ParseFFprobe([]byte(ffprobeDV))
	if err != nil {
		t.Fatal(err)
	}
	if info.HDR != "DolbyVision" {
		t.Errorf("hdr = %q, want DolbyVision", info.HDR)
	}
	if info.AudioSummary != "eac3 5.1(side) (eng)" {
		t.Errorf("audio summary = %q", info.AudioSummary)
	}
}

func TestParseFFprobeInvalid(t *testing.T) {
	if _, err := ParseFFprobe([]byte("not json")); err == nil {
		t.Errorf("want error for invalid json")
	}
	if _, err := ParseFFprobe([]byte(`{"streams":[]}`)); err == nil {
		t.Errorf("want error when no video stream")
	}
}
