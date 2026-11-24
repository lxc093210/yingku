package api

import (
	"log"
	"net/http"
	"time"
)

// backfillState is the progress of the TMDB metadata backfill job
// (batch 25): phase "titles" applies the Chinese-title preference to
// top-level items whose current title has no Chinese characters,
// phase "art" warms the TMDB artwork cache for items with no local
// poster. The job is a maintenance pass over data the scanner and
// the lazy fills cannot reach on their own: titles are never filled
// lazily in list views, and artwork only warms when a client happens
// to request that exact item.
type backfillState struct {
	Running     bool   `json:"running"`
	Phase       string `json:"phase"`
	Processed   int    `json:"processed"`
	TitlesFixed int    `json:"titlesFixed"`
	ArtFound    int    `json:"artFound"`
	ArtMissing  int    `json:"artMissing"`
	Errors      int    `json:"errors"`
	// DoubanFixed counts items whose Chinese title/overview came
	// from the Douban assist (batch 28).
	DoubanFixed int    `json:"doubanFixed"`
	Note        string `json:"note,omitempty"`
	StartedAt   int64  `json:"startedAt"`
	FinishedAt  int64  `json:"finishedAt"`
}

func (s *Server) backfillSnapshot() backfillState {
	s.bfMu.Lock()
	defer s.bfMu.Unlock()
	return s.bf
}

func (s *Server) backfillUpdate(fn func(*backfillState)) {
	s.bfMu.Lock()
	defer s.bfMu.Unlock()
	fn(&s.bf)
}

// tryStartBackfill atomically marks the backfill as running and
// reports whether the caller won the slot. Shared by the maintenance
// endpoint and the metadata scheduled job (batch 28).
func (s *Server) tryStartBackfill() bool {
	started := false
	s.backfillUpdate(func(b *backfillState) {
		if !b.Running {
			*b = backfillState{Running: true, Phase: "titles", StartedAt: time.Now().Unix()}
			started = true
		}
	})
	return started
}

func (s *Server) handleBackfillStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(r) {
		writeErr(w, http.StatusForbidden, "admin required")
		return
	}
	if !s.tryStartBackfill() {
		writeErr(w, http.StatusConflict, "backfill already running")
		return
	}
	go s.runBackfill()
	writeJSON(w, http.StatusAccepted, s.backfillSnapshot())
}

func (s *Server) handleBackfillStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.backfillSnapshot())
}

// runBackfill executes the two phases with a polite pause between
// items (the TMDB proxy is the user's own Worker; ~5 items/s is
// plenty for a one-off pass over ~1.5k items).
func (s *Server) runBackfill() {
	defer s.backfillUpdate(func(b *backfillState) {
		b.Running = false
		b.Phase = "done"
		b.FinishedAt = time.Now().Unix()
	})
	// Each backfill run is one Douban round: fresh 50-item budget.
	s.beginDoubanRound()
	cfg, err := s.tmdbCfg()
	tmdbOK := err == nil && cfg.available()
	if !tmdbOK {
		// TMDB being unavailable does not block the Douban pass
		// below: those items are exactly the "TMDB has no Chinese
		// data" set. Only when Douban is also off is there nothing
		// to do in phase 1.
		s.backfillUpdate(func(b *backfillState) { b.Note = "tmdb not configured" })
		log.Printf("tmdb backfill: TMDB not configured, Douban-only phase 1")
	}
	// Phase 1: Chinese titles (and, via Douban, Chinese overviews).
	items, err := s.st.TopLevelTmdbItems()
	if err != nil {
		s.backfillUpdate(func(b *backfillState) { b.Note = "candidate query failed" })
		log.Printf("tmdb backfill: candidates: %v", err)
		return
	}
	for _, it := range items {
		if it.Type != "movie" && it.Type != "series" {
			continue
		}
		needTitle := !hasHan(it.Title)
		needOverview := it.Overview == ""
		if !needTitle && !needOverview {
			continue
		}
		tmdbFixedTitle := false
		if needTitle && tmdbOK {
			kind := "movie"
			if it.Type == "series" {
				kind = "tv"
			}
			title, _, _, _, _, ferr := fetchTMDBDetails(cfg, kind, it.TmdbID)
			s.backfillUpdate(func(b *backfillState) {
				b.Processed++
				if ferr != nil {
					b.Errors++
				}
			})
			if ferr == nil && title != "" && title != it.Title && hasHan(title) {
				if uerr := s.st.FillItemTitle(it.ID, title); uerr == nil {
					tmdbFixedTitle = true
					s.backfillUpdate(func(b *backfillState) { b.TitlesFixed++ })
				}
			}
		}
		// Douban assist: only for what TMDB could not provide in
		// Chinese (no Chinese title, or no overview at all).
		if (!needTitle || tmdbFixedTitle) && !needOverview {
			// TMDB covered everything this item needs.
		} else if s.applyDouban(it) {
			s.backfillUpdate(func(b *backfillState) { b.DoubanFixed++ })
		}
		time.Sleep(150 * time.Millisecond)
	}
	// Phase 2: artwork warm-up for items with no local poster.
	s.backfillUpdate(func(b *backfillState) { b.Phase = "art"; b.Processed = 0 })
	ids, err := s.st.TopLevelMissingPosterIDs()
	if err != nil {
		log.Printf("tmdb backfill: art candidates: %v", err)
		return
	}
	for _, id := range ids {
		p := s.lazyTMDBArt(id, "poster")
		_ = s.lazyTMDBArt(id, "backdrop")
		s.backfillUpdate(func(b *backfillState) {
			b.Processed++
			if p != "" {
				b.ArtFound++
			} else {
				b.ArtMissing++
			}
		})
		time.Sleep(150 * time.Millisecond)
	}
	st := s.backfillSnapshot()
	log.Printf("tmdb backfill done: titles fixed %d, art found %d, art missing %d, errors %d",
		st.TitlesFixed, st.ArtFound, st.ArtMissing, st.Errors)
}
