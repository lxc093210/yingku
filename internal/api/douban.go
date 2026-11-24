package api

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"time"

	"yingku/internal/meta"
	"yingku/internal/store"
)

// Douban assist (batch 28): when TMDB has no Chinese title or no
// Chinese overview for an item, Douban fills exactly those two
// fields — nothing else. Discipline (see meta.DoubanClient for the
// transport half):
//
//   - the settings toggle "douban_assist" defaults to ON (anything
//     but the stored value "false" means on);
//   - a per-round budget (50 items) is armed by beginDoubanRound at
//     the start of every metadata backfill round; the detail-page
//     lazy fill shares the same gate, so a stopped round also stops
//     lazy assists until the next round re-arms it;
//   - a 403/429 stops the round immediately (gate records why);
//   - successful matches are cached per TMDB id in app_settings
//     (key douban_cache:<tmdbID>), so an item is never looked up
//     twice across rounds.

const (
	settingDoubanAssist = "douban_assist"
	doubanRoundBudget   = 50
)

func (s *Server) doubanAssistEnabled() bool {
	v, err := s.st.GetSetting(settingDoubanAssist)
	return err != nil || v != "false"
}

// beginDoubanRound re-arms the per-round budget and clears the stop
// flag. Called at the start of runBackfill (the metadata task).
func (s *Server) beginDoubanRound() {
	s.doubanMu.Lock()
	defer s.doubanMu.Unlock()
	s.doubanBudget = doubanRoundBudget
	s.doubanStopped = ""
}

// doubanGate reports the gate state (remaining budget, stop reason).
func (s *Server) doubanGate() (int, string) {
	s.doubanMu.Lock()
	defer s.doubanMu.Unlock()
	return s.doubanBudget, s.doubanStopped
}

type doubanCacheEntry struct {
	DoubanID string `json:"doubanId"`
	Title    string `json:"title"`
	Overview string `json:"overview"`
}

func (s *Server) doubanCacheGet(tmdbID int) (*meta.DoubanInfo, bool) {
	raw, err := s.st.GetSetting("douban_cache:" + strconv.Itoa(tmdbID))
	if err != nil || raw == "" {
		return nil, false
	}
	var e doubanCacheEntry
	if err := json.Unmarshal([]byte(raw), &e); err != nil || e.DoubanID == "" {
		return nil, false
	}
	return &meta.DoubanInfo{DoubanID: e.DoubanID, Title: e.Title, Overview: e.Overview}, true
}

func (s *Server) doubanCachePut(tmdbID int, info *meta.DoubanInfo) {
	raw, err := json.Marshal(doubanCacheEntry{
		DoubanID: info.DoubanID, Title: info.Title, Overview: info.Overview,
	})
	if err != nil {
		return
	}
	_ = s.st.SetSetting("douban_cache:"+strconv.Itoa(tmdbID), string(raw))
}

// doubanLookup resolves an item through the gate and the cache.
// Nil when the assist is off, the gate is stopped/exhausted, no
// match exists, or the lookup failed (rate limits stop the gate).
func (s *Server) doubanLookup(it *store.Item) *meta.DoubanInfo {
	if it == nil || it.TmdbID <= 0 || !s.doubanAssistEnabled() {
		return nil
	}
	if info, ok := s.doubanCacheGet(it.TmdbID); ok {
		return info
	}
	s.doubanMu.Lock()
	if s.doubanStopped != "" || s.doubanBudget <= 0 {
		s.doubanMu.Unlock()
		return nil
	}
	s.doubanBudget--
	s.doubanMu.Unlock()

	kind := "movie"
	if it.Type == "series" {
		kind = "tv"
	}
	orig := it.OriginalTitle
	if orig == "" {
		orig = it.Title
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info, err := s.douban.FetchInfo(ctx, orig, it.Year, kind)
	if err != nil {
		if meta.IsDoubanRateLimited(err) {
			s.doubanMu.Lock()
			s.doubanStopped = err.Error()
			s.doubanMu.Unlock()
			log.Printf("douban assist stopped for this round: %v", err)
		} else {
			log.Printf("douban assist lookup %q: %v", orig, err)
		}
		return nil
	}
	if info == nil {
		return nil
	}
	s.doubanCachePut(it.TmdbID, info)
	return info
}

// applyDouban fills the item's Chinese title and/or overview from
// Douban, honoring the Chinese-first rule: the title is replaced
// only when the current one has no Han characters and Douban's
// does; the overview goes through FillItemMeta, which only fills
// still-empty fields. Reports whether anything changed.
func (s *Server) applyDouban(it *store.Item) bool {
	info := s.doubanLookup(it)
	if info == nil {
		return false
	}
	changed := false
	if info.Title != "" && !hasHan(it.Title) && hasHan(info.Title) {
		if err := s.st.FillItemTitle(it.ID, info.Title); err == nil {
			changed = true
		}
	}
	if info.Overview != "" && it.Overview == "" {
		if err := s.st.FillItemMeta(it.ID, info.Overview, 0, []string{}, 0); err == nil {
			changed = true
		}
	}
	return changed
}
