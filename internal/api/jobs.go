package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"yingku/internal/meta"
	"yingku/internal/store"
)

// Scheduled jobs (batch 28): an in-process minute ticker runs four
// maintenance jobs — full scan, TMDB metadata backfill, media-info
// completion and cover generation. Configuration and last-run state
// live in app_settings under "job:<name>" as JSON, so the schedule
// survives restarts. Every job is anti-reentrant: a job already
// running is never started twice.

type jobConfig struct {
	Enabled    bool   `json:"enabled"`
	EveryHours int    `json:"everyHours"`
	LastRun    int64  `json:"lastRun"`
	LastResult string `json:"lastResult"`
}

var jobOrder = []string{"scan", "metadata", "mediainfo", "covers"}

var jobDefaults = map[string]jobConfig{
	"scan":      {Enabled: true, EveryHours: 6},
	"metadata":  {Enabled: true, EveryHours: 24},
	"mediainfo": {Enabled: true, EveryHours: 24},
	"covers":    {Enabled: false, EveryHours: 168},
}

var jobLabels = map[string][2]string{
	"scan":      {"定时扫库", "按间隔全库扫描，新入库与删除自动同步"},
	"metadata":  {"元数据回填", "给缺中文标题、缺海报的条目从 TMDB 补齐"},
	"mediainfo": {"媒体信息补齐", "同名 -mediainfo.json 已出现时重扫升级；仍缺的用 ffprobe 实测"},
	"covers":    {"封面生成", "按当前风格与排序为全部媒体库重新生成封面"},
}

func jobKnown(name string) bool {
	_, ok := jobDefaults[name]
	return ok
}

func (s *Server) jobConfig(name string) jobConfig {
	cfg := jobDefaults[name]
	raw, err := s.st.GetSetting("job:" + name)
	if err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &cfg)
	}
	if cfg.EveryHours <= 0 {
		cfg.EveryHours = jobDefaults[name].EveryHours
	}
	return cfg
}

func (s *Server) saveJobConfig(name string, cfg jobConfig) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return s.st.SetSetting("job:"+name, string(raw))
}

// ---- run locking (shared by jobs and cover generation) ----

func (s *Server) tryAcquire(key string) bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.running[key] {
		return false
	}
	s.running[key] = true
	return true
}

func (s *Server) release(key string) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	delete(s.running, key)
}

func (s *Server) isRunning(key string) bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	return s.running[key]
}

// ---- scheduler ----

// jobLoop ticks once a minute and starts every enabled job whose
// interval has elapsed. A job that has never run (lastRun = 0) is
// armed instead of fired, so its first run happens one interval
// after the feature first boots.
func (s *Server) jobLoop() {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for range tick.C {
		now := time.Now().Unix()
		for _, name := range jobOrder {
			cfg := s.jobConfig(name)
			if !cfg.Enabled {
				continue
			}
			if cfg.LastRun == 0 {
				cfg.LastRun = now
				_ = s.saveJobConfig(name, cfg)
				continue
			}
			if now-cfg.LastRun >= int64(cfg.EveryHours)*3600 {
				go s.runJob(name)
			}
		}
	}
}

// runJob executes one job synchronously and records the outcome.
// Callers run it in a goroutine; it no-ops when the job is already
// running.
func (s *Server) runJob(name string) {
	if !s.tryAcquire("job:" + name) {
		return
	}
	defer s.release("job:" + name)
	var result string
	switch name {
	case "scan":
		result = s.jobScan()
	case "metadata":
		result = s.jobMetadata()
	case "mediainfo":
		result = s.jobMediainfo()
	case "covers":
		result = s.runCoverGeneration(0)
	}
	cfg := s.jobConfig(name)
	cfg.LastRun = time.Now().Unix()
	cfg.LastResult = result
	if err := s.saveJobConfig(name, cfg); err != nil {
		log.Printf("job %s: save state: %v", name, err)
	}
	log.Printf("job %s done: %s", name, result)
}

func (s *Server) jobScan() string {
	if err := s.scanner.StartAll(s.defaultRoot); err != nil {
		return "已有扫描在运行，本轮跳过"
	}
	return "已触发全库扫描"
}

func (s *Server) jobMetadata() string {
	if !s.tryStartBackfill() {
		return "元数据回填已在运行，本轮跳过"
	}
	s.runBackfill()
	st := s.backfillSnapshot()
	result := ""
	if st.Note != "" && st.TitlesFixed == 0 && st.DoubanFixed == 0 {
		result = st.Note
	} else {
		result = fmt.Sprintf("处理完成：标题修正 %d · 海报补齐 %d · 无海报 %d · 错误 %d",
			st.TitlesFixed, st.ArtFound, st.ArtMissing, st.Errors)
		if st.DoubanFixed > 0 {
			result += fmt.Sprintf(" · 豆瓣补齐 %d", st.DoubanFixed)
		}
	}
	if _, stopped := s.doubanGate(); stopped != "" {
		result += " · 豆瓣辅助本轮已停：" + stopped
	}
	return result
}

// jobMediainfo completes missing media info in two stages: items
// whose -mediainfo.json sidecar has appeared since their scan get an
// incremental rescan of their library (the scan fingerprint covers
// the sidecar, so the rescan upgrades them to 'json'); whatever is
// still missing afterwards is probed with ffprobe, capped per run.
func (s *Server) jobMediainfo() string {
	pending, err := s.st.PendingMediaInfoSources(100000)
	if err != nil {
		return "查询待补条目失败：" + err.Error()
	}
	libSet := map[int64]bool{}
	for _, p := range pending {
		if _, err := os.Stat(meta.SidecarPath(p.StrmPath)); err == nil {
			libSet[p.LibraryID] = true
		}
	}
	scanned := 0
	for libID := range libSet {
		if err := s.scanner.StartLibrary(libID); err == nil {
			scanned++
		}
	}
	if scanned > 0 {
		s.waitScanIdle(15 * time.Minute)
	}
	if !meta.FFprobeAvailable() {
		return fmt.Sprintf("阶段①：触发 %d 个库增量扫描；阶段②跳过：ffprobe 不在 PATH", scanned)
	}
	rest, err := s.st.PendingMediaInfoSources(30)
	if err != nil {
		return "查询待补条目失败：" + err.Error()
	}
	probed, failed := 0, 0
	for i, p := range rest {
		if i > 0 {
			time.Sleep(1500 * time.Millisecond)
		}
		if s.probeSource(p) {
			probed++
		} else {
			failed++
		}
	}
	return fmt.Sprintf("阶段①：触发 %d 个库增量扫描；阶段②：ffprobe 实测补齐 %d 条，失败 %d 条",
		scanned, probed, failed)
}

// waitScanIdle polls the scanner until it leaves the running state
// or the timeout elapses.
func (s *Server) waitScanIdle(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.scanner.Status().State != "running" {
			return
		}
		time.Sleep(2 * time.Second)
	}
}

// probeSource ffprobes one pending source through its STRM target
// URL and writes the result back with provenance 'probe'.
func (s *Server) probeSource(p store.PendingSource) bool {
	target := readStrmTarget(p.StrmPath)
	if target == "" {
		target = p.TargetURL
	}
	if target == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mi, err := meta.ProbeURL(ctx, target)
	if err != nil {
		log.Printf("mediainfo probe %s: %v", p.StrmPath, err)
		return false
	}
	streamsJSON := ""
	if len(mi.Streams) > 0 {
		if raw, err := json.Marshal(mi.Streams); err == nil {
			streamsJSON = string(raw)
		}
	}
	err = s.st.UpdateSourceMediaInfo(p.ID, store.MediaSource{
		Width: mi.Width, Height: mi.Height, VideoCodec: mi.VideoCodec,
		HDR: mi.HDR, AudioSummary: mi.AudioSummary, StreamsJSON: streamsJSON,
		SizeBytes: mi.Size, Bitrate: mi.Bitrate,
	})
	return err == nil
}

// metadataPending counts what the metadata job would work on:
// top-level TMDB items with a non-Chinese title, plus top-level
// items with no poster at all.
func (s *Server) metadataPending() int {
	n := 0
	if items, err := s.st.TopLevelTmdbItems(); err == nil {
		for _, it := range items {
			if (it.Type == "movie" || it.Type == "series") && !hasHan(it.Title) {
				n++
			}
		}
	}
	if ids, err := s.st.TopLevelMissingPosterIDs(); err == nil {
		n += len(ids)
	}
	return n
}

// ---- HTTP API ----

type jobDTO struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	EveryHours  int    `json:"everyHours"`
	LastRun     int64  `json:"lastRun"`
	LastResult  string `json:"lastResult"`
	Running     bool   `json:"running"`
	Pending     *int   `json:"pending,omitempty"`
}

func (s *Server) jobDTO(name string) jobDTO {
	cfg := s.jobConfig(name)
	d := jobDTO{
		Name: name, Label: jobLabels[name][0], Description: jobLabels[name][1],
		Enabled: cfg.Enabled, EveryHours: cfg.EveryHours,
		LastRun: cfg.LastRun, LastResult: cfg.LastResult,
		Running: s.isRunning("job:" + name),
	}
	switch name {
	case "mediainfo":
		if n, err := s.st.CountPendingMediaInfo(); err == nil {
			d.Pending = &n
		}
	case "metadata":
		n := s.metadataPending()
		d.Pending = &n
	}
	return d
}

func (s *Server) handleJobsList(w http.ResponseWriter, r *http.Request) {
	out := make([]jobDTO, 0, len(jobOrder))
	for _, name := range jobOrder {
		out = append(out, s.jobDTO(name))
	}
	writeJSON(w, http.StatusOK, map[string]any{"Jobs": out})
}

func (s *Server) handleJobUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !jobKnown(name) {
		writeErr(w, http.StatusNotFound, "unknown job")
		return
	}
	var p struct {
		Enabled    *bool `json:"enabled"`
		EveryHours *int  `json:"everyHours"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	cfg := s.jobConfig(name)
	if p.Enabled != nil {
		cfg.Enabled = *p.Enabled
	}
	if p.EveryHours != nil {
		if *p.EveryHours < 1 || *p.EveryHours > 8760 {
			writeErr(w, http.StatusBadRequest, "everyHours out of range")
			return
		}
		cfg.EveryHours = *p.EveryHours
	}
	if err := s.saveJobConfig(name, cfg); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.jobDTO(name))
}

func (s *Server) handleJobRun(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !jobKnown(name) {
		writeErr(w, http.StatusNotFound, "unknown job")
		return
	}
	if s.isRunning("job:" + name) {
		writeErr(w, http.StatusConflict, "job already running")
		return
	}
	go s.runJob(name)
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}
