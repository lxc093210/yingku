package api

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"yingku/internal/store"
)

// ---- Emby client mount surface (batch 11) ----
//
// The endpoints Emby clients (SenPlayer, Infuse) call right after
// AuthenticateByName when mounting a server: Views (the client's first
// stop), Items/Latest, Items/Resume, Shows/NextUp, Search/Hints and
// System/Endpoint, plus the FavoriteItems/PlayedItems write surface.
// Without Views the clients report a bare 404 on mount even though the
// batch-9 browse surface is complete.

// embyParent decodes an Emby ParentId parameter into the scope the
// store filters understand. ok=false means the value is present but
// unparseable (callers answer with an empty result, like Items does).
type embyParent struct {
	set       bool
	isLibrary bool
	libraryID int64
	itemID    int64
}

func parseEmbyParent(q url.Values) embyParent {
	raw := queryParam(q, "ParentId", "parentId")
	if raw == "" {
		return embyParent{}
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return embyParent{set: true, itemID: -1}
	}
	if v >= embyLibraryIDOffset {
		return embyParent{set: true, isLibrary: true, libraryID: v - embyLibraryIDOffset}
	}
	return embyParent{set: true, itemID: v}
}

// embyTypesParam parses IncludeItemTypes into internal type names.
// nil means "no filter"; an empty non-nil slice means the parameter
// named only unknown types, so nothing can match.
func embyTypesParam(q url.Values) []string {
	inc := queryParam(q, "IncludeItemTypes", "includeItemTypes")
	if inc == "" {
		return nil
	}
	known := map[string]bool{}
	for _, tok := range strings.Split(inc, ",") {
		switch t := strings.ToLower(strings.TrimSpace(tok)); t {
		case "movie", "series", "season", "episode":
			known[t] = true
		}
	}
	out := make([]string, 0, len(known))
	for t := range known {
		out = append(out, t)
	}
	return out
}

func embyPageParams(q url.Values) (start, limit int) {
	start, _ = strconv.Atoi(queryParam(q, "StartIndex", "startIndex"))
	if start < 0 {
		start = 0
	}
	limit, _ = strconv.Atoi(queryParam(q, "Limit", "limit"))
	return start, limit
}

// handleViews is the client's first stop after login: the same
// CollectionFolder listing as Library/MediaFolders.
func (s *Server) handleViews(w http.ResponseWriter, r *http.Request) {
	dtos, total, err := s.libraryEmbyDTOs()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, embyItemsResponse{Items: dtos, TotalRecordCount: total, StartIndex: 0})
}

// handleEmbyLatest answers Items/Latest with a BARE array of DTOs
// (Emby's Latest is not wrapped in a query-result object), newest
// additions first. Scope: no ParentId = top-level items everywhere; a
// library = its top-level items; a series = its episodes; a season =
// its episodes; anything else = just that item.
func (s *Server) handleEmbyLatest(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	_, limit := embyPageParams(q)
	if limit <= 0 {
		limit = 20
	}
	if limit > 500 {
		limit = 500
	}
	filter := store.ItemsFilter{SortField: "date", Desc: true, Limit: limit}
	types := embyTypesParam(q)

	parent := parseEmbyParent(q)
	switch {
	case !parent.set:
		filter.TopLevel = true
	case parent.itemID < 0:
		writeJSON(w, http.StatusOK, []embyItemDTO{})
		return
	case parent.isLibrary:
		filter.HasParent = true
		filter.ParentIsLibrary = true
		filter.ParentLibraryID = parent.libraryID
	default:
		it, err := s.st.GetItem(parent.itemID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if it == nil {
			writeJSON(w, http.StatusOK, []embyItemDTO{})
			return
		}
		switch it.Type {
		case "series":
			filter.HasParent = true
			filter.ParentItemID = it.ID
			filter.Recursive = true
			if types == nil {
				types = []string{"episode"}
			}
		case "season":
			filter.HasParent = true
			filter.ParentItemID = it.ID
		default:
			filter.IDs = []int64{it.ID}
		}
	}
	if types != nil {
		if len(types) == 0 {
			writeJSON(w, http.StatusOK, []embyItemDTO{})
			return
		}
		filter.Types = types
	}
	items, _, err := s.st.QueryItems(filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	dtos, err := s.embyDTOs(s.currentUserID(r), items)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dtos)
}

// handleEmbyResume answers Items/Resume: in-progress movies and
// episodes, wrapped in a query-result object like real Emby.
func (s *Server) handleEmbyResume(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	start, limit := embyPageParams(q)
	filter := store.ResumeFilter{Start: start, Limit: limit}
	parent := parseEmbyParent(q)
	if parent.set {
		if parent.itemID < 0 {
			writeJSON(w, http.StatusOK, embyItemsResponse{Items: []embyItemDTO{}, TotalRecordCount: 0, StartIndex: start})
			return
		}
		filter.HasParent = true
		filter.ParentIsLibrary = parent.isLibrary
		filter.ParentLibraryID = parent.libraryID
		filter.ParentItemID = parent.itemID
	}
	items, total, err := s.st.ResumeItems(s.currentUserID(r), filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	dtos, err := s.embyDTOs(s.currentUserID(r), items)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, embyItemsResponse{Items: dtos, TotalRecordCount: total, StartIndex: start})
}

// handleNextUp answers Shows/NextUp. The optional ParentId narrows to
// one library (encoded id) or one series (item id); the UserId
// parameter is accepted, but attribution follows the request's
// credential, like every other user-data surface.
func (s *Server) handleNextUp(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	_, limit := embyPageParams(q)
	if limit <= 0 {
		limit = 25
	}
	var onlySeries, onlyLibrary int64
	if parent := parseEmbyParent(q); parent.set && parent.itemID >= 0 {
		if parent.isLibrary {
			onlyLibrary = parent.libraryID
		} else {
			onlySeries = parent.itemID
		}
	}
	items, err := s.st.NextUpEpisodes(s.currentUserID(r), onlySeries, onlyLibrary, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	dtos, err := s.embyDTOs(s.currentUserID(r), items)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, embyItemsResponse{Items: dtos, TotalRecordCount: len(dtos), StartIndex: 0})
}

// ---- Search/Hints ----

type searchHintDTO struct {
	ItemId          string `json:"ItemId"`
	Id              string `json:"Id"`
	Name            string `json:"Name"`
	Type            string `json:"Type"`
	ProductionYear  int    `json:"ProductionYear,omitempty"`
	MatchedTerm     string `json:"MatchedTerm"`
	IsFolder        bool   `json:"IsFolder,omitempty"`
	RunTimeTicks    int64  `json:"RunTimeTicks,omitempty"`
	PrimaryImageTag string `json:"PrimaryImageTag,omitempty"`
}

type searchHintsResponse struct {
	SearchHints      []searchHintDTO `json:"SearchHints"`
	TotalRecordCount int             `json:"TotalRecordCount"`
}

// handleSearchHints answers Search/Hints with top-level movies and
// series whose title contains the term (the store's LIKE match is
// case-insensitive for ASCII, exact substring otherwise).
func (s *Server) handleSearchHints(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	term := queryParam(q, "searchTerm", "SearchTerm")
	_, limit := embyPageParams(q)
	if limit <= 0 {
		limit = 20
	}
	resp := searchHintsResponse{SearchHints: []searchHintDTO{}}
	if term == "" {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	items, total, err := s.st.QueryItems(store.ItemsFilter{
		Search:   term,
		Types:    []string{"movie", "series"},
		TopLevel: true,
		Limit:    limit,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	dtos, err := s.embyDTOs(s.currentUserID(r), items)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, d := range dtos {
		h := searchHintDTO{
			ItemId:         d.Id,
			Id:             d.Id,
			Name:           d.Name,
			Type:           d.Type,
			ProductionYear: d.ProductionYear,
			MatchedTerm:    term,
			IsFolder:       d.IsFolder,
			RunTimeTicks:   d.RunTimeTicks,
		}
		if d.ImageTags != nil {
			h.PrimaryImageTag = d.ImageTags["Primary"]
		}
		resp.SearchHints = append(resp.SearchHints, h)
	}
	resp.TotalRecordCount = total
	writeJSON(w, http.StatusOK, resp)
}

// ---- System/Endpoint ----

type systemEndpointDTO struct {
	LocalAddress  string `json:"LocalAddress"`
	RemoteAddress string `json:"RemoteAddress"`
	IsLocal       bool   `json:"IsLocal"`
	IsInNetwork   bool   `json:"IsInNetwork"`
}

func (s *Server) handleSystemEndpoint(w http.ResponseWriter, r *http.Request) {
	addr := "http://" + r.Host
	writeJSON(w, http.StatusOK, systemEndpointDTO{
		LocalAddress:  addr,
		RemoteAddress: addr,
		IsLocal:       true,
		IsInNetwork:   true,
	})
}

// ---- FavoriteItems / PlayedItems write surface ----

// embyUserDataOf reads back the item's state for the given user as an
// Emby UserData DTO (the shape FavoriteItems/PlayedItems answer with).
func (s *Server) embyUserDataOf(userID string, it *store.Item) (embyUserDataDTO, error) {
	ud, err := s.st.UserDataOf(it.ID, userID)
	if err != nil {
		return embyUserDataDTO{}, err
	}
	d := embyUserDataDTO{
		IsFavorite:            ud.Favorite,
		Played:                ud.Played,
		PlaybackPositionTicks: ud.ProgressTicks,
	}
	if rt := int64(it.RuntimeMin) * 60 * 10_000_000; rt > 0 && ud.ProgressTicks > 0 {
		d.PlayedPercentage = float64(ud.ProgressTicks) / float64(rt) * 100
	}
	return d, nil
}

// userItemForWrite resolves the {itemId} path value of a write route:
// library-encoded ids and unknown items are a JSON 404.
func (s *Server) userItemForWrite(w http.ResponseWriter, r *http.Request) *store.Item {
	id, err := strconv.ParseInt(r.PathValue("itemId"), 10, 64)
	if err != nil || id >= embyLibraryIDOffset {
		writeErr(w, http.StatusNotFound, "item not found")
		return nil
	}
	it, err := s.st.GetItem(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return nil
	}
	if it == nil {
		writeErr(w, http.StatusNotFound, "item not found")
		return nil
	}
	return it
}

// handleFavoriteItems serves POST (favorite) and DELETE (unfavorite)
// on Users/{userId}/FavoriteItems/{itemId}.
func (s *Server) handleFavoriteItems(w http.ResponseWriter, r *http.Request) {
	it := s.userItemForWrite(w, r)
	if it == nil {
		return
	}
	if err := s.st.SetFavorite(it.ID, s.currentUserID(r), r.Method == http.MethodPost); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	d, err := s.embyUserDataOf(s.currentUserID(r), it)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// handlePlayedItems serves POST (mark played) and DELETE (mark
// unplayed) on Users/{userId}/PlayedItems/{itemId}. Only the played
// flag flips; the recorded position is preserved both ways (the web
// UI's progress/reset remains the way to clear a position entirely).
func (s *Server) handlePlayedItems(w http.ResponseWriter, r *http.Request) {
	it := s.userItemForWrite(w, r)
	if it == nil {
		return
	}
	if err := s.st.SetPlayed(it.ID, s.currentUserID(r), r.Method == http.MethodPost); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	d, err := s.embyUserDataOf(s.currentUserID(r), it)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, d)
}
