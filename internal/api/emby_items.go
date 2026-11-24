package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"yingku/internal/meta"
	"yingku/internal/store"
)

// ---- Emby core read surface (Nan integration) ----
//
// The endpoints Nan's Emby helper needs to browse and play the library:
// MediaFolders, Items (list + detail), item images, PlaybackInfo,
// Videos stream redirect, session progress reporting and the by-name
// authentication handshake. yingku stays a pure protocol server: item
// paths handed out are the STRM targets themselves (Nan's 8115 URLs);
// the actual 302 to the 115 CDN happens in Nan, exactly as with a real
// Emby server. The /Videos/{id}/stream endpoints exist for direct
// clients and mirror the web UI's own 302.
//
// Library ids and item ids live in separate tables, so in the Emby
// surface a CollectionFolder's Id is the library id shifted by
// embyLibraryIDOffset — an id Nan passes back as ParentId can then be
// decoded unambiguously, and item ParentIds pointing at their library
// stay consistent with the MediaFolders listing.

const embyLibraryIDOffset = int64(1_000_000_000)

func embyLibraryID(libID int64) string {
	return strconv.FormatInt(libID+embyLibraryIDOffset, 10)
}

// ---- DTOs ----

type embyUserDataDTO struct {
	IsFavorite            bool    `json:"IsFavorite"`
	Played                bool    `json:"Played"`
	PlaybackPositionTicks int64   `json:"PlaybackPositionTicks"`
	PlayedPercentage      float64 `json:"PlayedPercentage,omitempty"`
}

type embyItemDTO struct {
	Id                      string            `json:"Id"`
	Name                    string            `json:"Name"`
	Type                    string            `json:"Type"`
	CollectionType          string            `json:"CollectionType,omitempty"`
	Overview                string            `json:"Overview,omitempty"`
	ProductionYear          int               `json:"ProductionYear,omitempty"`
	CommunityRating         float64           `json:"CommunityRating,omitempty"`
	RunTimeTicks            int64             `json:"RunTimeTicks,omitempty"`
	ProviderIds             map[string]string `json:"ProviderIds,omitempty"`
	SeriesName              string            `json:"SeriesName,omitempty"`
	SeriesId                string            `json:"SeriesId,omitempty"`
	SeasonId                string            `json:"SeasonId,omitempty"`
	ParentId                string            `json:"ParentId,omitempty"`
	IndexNumber             *int              `json:"IndexNumber,omitempty"`
	ParentIndexNumber       *int              `json:"ParentIndexNumber,omitempty"`
	ChildCount              *int              `json:"ChildCount,omitempty"`
	MediaType               string            `json:"MediaType,omitempty"`
	IsFolder                bool              `json:"IsFolder"`
	DateCreated             string            `json:"DateCreated,omitempty"`
	ImageTags               map[string]string `json:"ImageTags,omitempty"`
	BackdropImageTags       []string          `json:"BackdropImageTags,omitempty"`
	PrimaryImageAspectRatio *float64          `json:"PrimaryImageAspectRatio,omitempty"`
	UserData                *embyUserDataDTO  `json:"UserData,omitempty"`
}

type embyItemsResponse struct {
	Items            []embyItemDTO `json:"Items"`
	TotalRecordCount int           `json:"TotalRecordCount"`
	StartIndex       int           `json:"StartIndex"`
}

// hasArtwork is the lightweight availability check behind ImageTags: a
// local registered file or an already-cached TMDB download counts —
// crucially, unlike resolveImageFile it never triggers a TMDB fetch,
// so listing pages stay fast. Episodes/seasons follow the same series
// chain as the image endpoint (an episode's own still cache is its
// tmdb-art backdrop slot, covered by the cache check).
func (s *Server) hasArtwork(it *store.Item, kind string) bool {
	if s.localImageFile(it.ID, kind) != "" {
		return true
	}
	if fileExists(s.tmdbArtFile(it.ID, kind)) {
		return true
	}
	if it.Type == "episode" || it.Type == "season" {
		if series := s.seriesItemOf(it); series != nil && series.ID != it.ID {
			return s.hasArtwork(series, kind)
		}
	}
	return false
}

// embyDTOs converts items, batch-loading child counts, the given
// user's data and series names so a full page costs a constant number
// of queries.
func (s *Server) embyDTOs(userID string, items []*store.Item) ([]embyItemDTO, error) {
	out := make([]embyItemDTO, 0, len(items))
	if len(items) == 0 {
		return out, nil
	}
	ids := make([]int64, 0, len(items))
	seriesIDs := map[int64]bool{}
	for _, it := range items {
		ids = append(ids, it.ID)
		if it.SeriesID != nil {
			seriesIDs[*it.SeriesID] = true
		}
	}
	counts, err := s.st.ChildCounts(ids)
	if err != nil {
		return nil, err
	}
	uds, err := s.st.UserDataBatch(userID, ids)
	if err != nil {
		return nil, err
	}
	sids := make([]int64, 0, len(seriesIDs))
	for id := range seriesIDs {
		sids = append(sids, id)
	}
	titles, err := s.st.TitlesByID(sids)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		out = append(out, s.toEmbyDTO(it, counts, uds, titles))
	}
	return out, nil
}

func (s *Server) toEmbyDTO(it *store.Item, counts map[int64]int, uds map[int64]*store.UserData, seriesTitles map[int64]string) embyItemDTO {
	d := embyItemDTO{
		Id:             strconv.FormatInt(it.ID, 10),
		Name:           it.Title,
		Type:           embyType[it.Type],
		Overview:       it.Overview,
		ProductionYear: it.Year,
		IsFolder:       it.Type == "series" || it.Type == "season",
	}
	if it.Rating > 0 {
		d.CommunityRating = it.Rating
	}
	if it.RuntimeMin > 0 {
		d.RunTimeTicks = int64(it.RuntimeMin) * 60 * 10_000_000
	}
	if it.TmdbID > 0 {
		d.ProviderIds = map[string]string{"Tmdb": strconv.Itoa(it.TmdbID)}
	}
	if it.ParentID != nil {
		d.ParentId = strconv.FormatInt(*it.ParentID, 10)
	} else {
		d.ParentId = embyLibraryID(it.LibraryID)
	}
	if it.SeriesID != nil {
		d.SeriesId = strconv.FormatInt(*it.SeriesID, 10)
		d.SeriesName = seriesTitles[*it.SeriesID]
	}
	switch it.Type {
	case "episode":
		d.IndexNumber = it.EpisodeNum
		d.ParentIndexNumber = it.SeasonNum
		if it.ParentID != nil {
			d.SeasonId = strconv.FormatInt(*it.ParentID, 10)
		}
		d.MediaType = "Video"
	case "movie":
		d.MediaType = "Video"
	case "season":
		d.IndexNumber = it.SeasonNum
	}
	if it.Type == "series" || it.Type == "season" {
		n := counts[it.ID]
		d.ChildCount = &n
	}
	if ts := it.DateAdded; ts > 0 {
		d.DateCreated = time.Unix(ts, 0).UTC().Format(time.RFC3339)
	} else if it.CreatedAt > 0 {
		d.DateCreated = time.Unix(it.CreatedAt, 0).UTC().Format(time.RFC3339)
	}
	tag := "yk" + strconv.FormatInt(it.ID, 10)
	if s.hasArtwork(it, "poster") {
		d.ImageTags = map[string]string{"Primary": tag}
	}
	if s.hasArtwork(it, "backdrop") {
		d.BackdropImageTags = []string{tag}
	}
	if ud, ok := uds[it.ID]; ok && ud != nil {
		eu := &embyUserDataDTO{
			IsFavorite:            ud.Favorite,
			Played:                ud.Played,
			PlaybackPositionTicks: ud.ProgressTicks,
		}
		if d.RunTimeTicks > 0 && ud.ProgressTicks > 0 {
			eu.PlayedPercentage = float64(ud.ProgressTicks) / float64(d.RunTimeTicks) * 100
		}
		d.UserData = eu
	}
	return d
}

func (s *Server) embyLibraryDTO(l store.Library) embyItemDTO {
	ct := map[string]string{"movie": "movies", "series": "tvshows", "mixed": "mixed"}[l.Kind]
	n := l.ItemCount
	d := embyItemDTO{
		Id:             embyLibraryID(l.ID),
		Name:           l.Name,
		Type:           "CollectionFolder",
		CollectionType: ct,
		IsFolder:       true,
		ChildCount:     &n,
	}
	// Library cover: clients only request a CollectionFolder's image
	// when the DTO advertises one. The tag carries the signature of
	// the picked artwork set (a collage of the library's best-rated
	// backdrops — see libraryCover), so a changed pick makes clients
	// refetch instead of showing the stale cover forever.
	if sig := s.libraryUploadSig(l.ID); sig != "" {
		// Uploaded cover override (MediaCoverGenerator): advertise
		// its signature so clients fetch the generated cover.
		tag := "yklib" + strconv.FormatInt(l.ID, 10) + "-" + sig
		d.ImageTags = map[string]string{"Primary": tag}
		d.BackdropImageTags = []string{tag}
		ratio := 1.7777777777777777
		d.PrimaryImageAspectRatio = &ratio
	} else if items, err := s.st.LibraryArtworkItems(l.ID, 4); err == nil && len(items) > 0 {
		tag := "yklib" + strconv.FormatInt(l.ID, 10) + "-" + libraryArtSignature(items)
		d.ImageTags = map[string]string{"Primary": tag}
		d.BackdropImageTags = []string{tag}
		ratio := 1.7777777777777777
		d.PrimaryImageAspectRatio = &ratio
	}
	return d
}

func (s *Server) libraryEmbyDTOs() ([]embyItemDTO, int, error) {
	libs, err := s.st.ListLibraries()
	if err != nil {
		return nil, 0, err
	}
	out := make([]embyItemDTO, 0, len(libs))
	for _, l := range libs {
		out = append(out, s.embyLibraryDTO(l))
	}
	return out, len(out), nil
}

// ---- handlers ----

func (s *Server) handleMediaFolders(w http.ResponseWriter, r *http.Request) {
	dtos, total, err := s.libraryEmbyDTOs()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, embyItemsResponse{Items: dtos, TotalRecordCount: total, StartIndex: 0})
}

func (s *Server) handleUsersPublic(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, []any{})
}

// queryParam reads the first non-empty of the given parameter names
// (Emby's canonical casing first, then a lowercase variant).
func queryParam(q url.Values, names ...string) string {
	for _, n := range names {
		if v := q.Get(n); v != "" {
			return v
		}
	}
	return ""
}

func (s *Server) handleEmbyItems(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	idsParam := queryParam(q, "Ids", "ids")
	parentParam := queryParam(q, "ParentId", "parentId")

	start, _ := strconv.Atoi(queryParam(q, "StartIndex", "startIndex"))
	if start < 0 {
		start = 0
	}
	limit, _ := strconv.Atoi(queryParam(q, "Limit", "limit"))

	// No scope: the library list, as CollectionFolders.
	if idsParam == "" && parentParam == "" {
		dtos, total, err := s.libraryEmbyDTOs()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, embyItemsResponse{Items: dtos, TotalRecordCount: total, StartIndex: 0})
		return
	}

	filter := store.ItemsFilter{Start: start, Limit: limit}
	if idsParam != "" {
		for _, part := range strings.Split(idsParam, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			v, err := strconv.ParseInt(part, 10, 64)
			if err != nil {
				continue
			}
			if v >= embyLibraryIDOffset {
				// A CollectionFolder id inside Ids: resolve to the library.
				if lib, gerr := s.st.GetLibrary(v - embyLibraryIDOffset); gerr == nil && lib != nil {
					dtos, total, lerr := s.libraryEmbyDTOs()
					if lerr != nil {
						writeErr(w, http.StatusInternalServerError, lerr.Error())
						return
					}
					for _, d := range dtos {
						if d.Id == strconv.FormatInt(v, 10) {
							writeJSON(w, http.StatusOK, embyItemsResponse{
								Items: []embyItemDTO{d}, TotalRecordCount: 1, StartIndex: 0})
							return
						}
					}
					_ = total
				}
				continue
			}
			filter.IDs = append(filter.IDs, v)
		}
	} else {
		v, err := strconv.ParseInt(parentParam, 10, 64)
		if err != nil {
			writeJSON(w, http.StatusOK, embyItemsResponse{Items: []embyItemDTO{}, TotalRecordCount: 0, StartIndex: start})
			return
		}
		filter.HasParent = true
		if v >= embyLibraryIDOffset {
			filter.ParentIsLibrary = true
			filter.ParentLibraryID = v - embyLibraryIDOffset
		} else {
			filter.ParentItemID = v
		}
		filter.Recursive = strings.EqualFold(queryParam(q, "Recursive", "recursive"), "true")
	}

	if inc := queryParam(q, "IncludeItemTypes", "includeItemTypes"); inc != "" {
		known := map[string]bool{}
		for _, tok := range strings.Split(inc, ",") {
			switch strings.ToLower(strings.TrimSpace(tok)) {
			case "movie", "series", "season", "episode":
				known[strings.ToLower(strings.TrimSpace(tok))] = true
			}
		}
		if len(known) == 0 {
			// Only unknown types requested (e.g. Folder): nothing matches.
			writeJSON(w, http.StatusOK, embyItemsResponse{Items: []embyItemDTO{}, TotalRecordCount: 0, StartIndex: start})
			return
		}
		for t := range known {
			filter.Types = append(filter.Types, t)
		}
	}
	filter.Search = queryParam(q, "SearchTerm", "searchTerm")
	if sb := queryParam(q, "SortBy", "sortBy"); sb != "" {
		// Clients send a variety of date-ish sort keys; all of them map
		// onto the one date the store tracks (date_added). Anything
		// else falls through to the default name sort — never an error.
		first := strings.TrimSpace(strings.Split(sb, ",")[0])
		for _, k := range []string{"DateCreated", "DateLastContentAdded", "PremiereDate"} {
			if strings.EqualFold(first, k) {
				filter.SortField = "date"
			}
		}
		if strings.Contains(sb, "IndexNumber") {
			filter.SortField = "index"
		}
	}
	// Episode-only and season-only listings default to story order:
	// Emby's own name sort keys for episodes embed the season/episode
	// numbers, while yingku's sort_title is the bare episode name, so
	// a name sort presents episodes scrambled.
	if filter.SortField == "" && len(filter.Types) == 1 &&
		(filter.Types[0] == "episode" || filter.Types[0] == "season") {
		filter.SortField = "index"
	}
	filter.Desc = strings.EqualFold(queryParam(q, "SortOrder", "sortOrder"), "Descending")

	items, total, err := s.st.QueryItems(filter)
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

func (s *Server) handleEmbyItem(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad item id")
		return
	}
	if id >= embyLibraryIDOffset {
		lib, err := s.st.GetLibrary(id - embyLibraryIDOffset)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if lib == nil {
			writeErr(w, http.StatusNotFound, "item not found")
			return
		}
		writeJSON(w, http.StatusOK, s.embyLibraryDTO(*lib))
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
	dtos, err := s.embyDTOs(s.currentUserID(r), []*store.Item{it})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dtos[0])
}

// ---- images ----

// parseEmbyWidth maps Emby's maxWidth/width parameters onto the same
// 40–1280 window the internal ?w= accepts; anything else means "serve
// the original".
func parseEmbyWidth(r *http.Request) int {
	for _, name := range []string{"maxWidth", "width", "w"} {
		v := r.URL.Query().Get(name)
		if v == "" {
			continue
		}
		if n, err := strconv.Atoi(v); err == nil && n >= 40 && n <= 1280 {
			return n
		}
		return 0
	}
	return 0
}

func (s *Server) handleEmbyImage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad item id")
		return
	}
	var kind string
	switch strings.ToLower(r.PathValue("imageType")) {
	case "primary":
		kind = "poster"
	case "backdrop", "thumb":
		kind = "backdrop"
	default:
		writeErr(w, http.StatusNotFound, "unknown image type")
		return
	}
	// A CollectionFolder id serves the library's composed cover
	// (see libraryCover) for every image type — client home screens
	// render library tiles in landscape.
	if id >= embyLibraryIDOffset {
		s.serveLibraryImage(w, r, id-embyLibraryIDOffset, parseEmbyWidth(r))
		return
	}
	s.serveItemImageW(w, r, id, kind, parseEmbyWidth(r))
}

// ---- PlaybackInfo ----

type embyMediaStreamDTO struct {
	Type              string  `json:"Type"`
	Index             int     `json:"Index"`
	Codec             string  `json:"Codec,omitempty"`
	CodecTag          string  `json:"CodecTag,omitempty"`
	Width             int     `json:"Width,omitempty"`
	Height            int     `json:"Height,omitempty"`
	Language          string  `json:"Language,omitempty"`
	DisplayTitle      string  `json:"DisplayTitle,omitempty"`
	BitRate           int     `json:"BitRate,omitempty"`
	BitDepth          int     `json:"BitDepth,omitempty"`
	Profile           string  `json:"Profile,omitempty"`
	Level             float64 `json:"Level,omitempty"`
	AverageFrameRate  float64 `json:"AverageFrameRate,omitempty"`
	RealFrameRate     float64 `json:"RealFrameRate,omitempty"`
	AspectRatio       string  `json:"AspectRatio,omitempty"`
	IsInterlaced      bool    `json:"IsInterlaced"`
	IsDefault         bool    `json:"IsDefault"`
	IsForced          bool    `json:"IsForced"`
	IsExternal        bool    `json:"IsExternal"`
	Channels          int     `json:"Channels,omitempty"`
	ChannelLayout     string  `json:"ChannelLayout,omitempty"`
	SampleRate        int     `json:"SampleRate,omitempty"`
	ColorPrimaries    string  `json:"ColorPrimaries,omitempty"`
	ColorSpace        string  `json:"ColorSpace,omitempty"`
	ColorTransfer     string  `json:"ColorTransfer,omitempty"`
	PixelFormat       string  `json:"PixelFormat,omitempty"`
	RefFrames         int     `json:"RefFrames,omitempty"`
	VideoRange        string  `json:"VideoRange,omitempty"`
	ExtendedVideoType string  `json:"ExtendedVideoType,omitempty"`
}

type embyMediaSourceDTO struct {
	Id                   string               `json:"Id"`
	Name                 string               `json:"Name"`
	Path                 string               `json:"Path"`
	Protocol             string               `json:"Protocol"`
	Container            string               `json:"Container,omitempty"`
	Type                 string               `json:"Type"`
	SupportsDirectPlay   bool                 `json:"SupportsDirectPlay"`
	SupportsDirectStream bool                 `json:"SupportsDirectStream"`
	SupportsTranscoding  bool                 `json:"SupportsTranscoding"`
	RunTimeTicks         int64                `json:"RunTimeTicks,omitempty"`
	Size                 int64                `json:"Size,omitempty"`
	Bitrate              int                  `json:"Bitrate,omitempty"`
	MediaStreams         []embyMediaStreamDTO `json:"MediaStreams,omitempty"`
}

type embyPlaybackInfoDTO struct {
	MediaSources  []embyMediaSourceDTO `json:"MediaSources"`
	PlaySessionId string               `json:"PlaySessionId"`
}

// mediaStreamsOf builds the Emby stream list of a source from the
// stored mediainfo: one video stream from the probed dimensions/codec,
// plus one audio stream per entry of the audio summary
// ("truehd 7.1 (eng), aac 2.0" — the trailing "· N subs" note is not a
// stream and is dropped).
func mediaStreamsOf(src store.MediaSource) []embyMediaStreamDTO {
	if src.StreamsJSON != "" {
		var stored []meta.StreamInfo
		if err := json.Unmarshal([]byte(src.StreamsJSON), &stored); err == nil && len(stored) > 0 {
			out := make([]embyMediaStreamDTO, 0, len(stored))
			for _, st := range stored {
				out = append(out, embyMediaStreamDTO{
					Type: st.Type, Index: st.Index, Codec: st.Codec, CodecTag: st.CodecTag,
					Width: st.Width, Height: st.Height, Language: st.Language,
					DisplayTitle: st.DisplayTitle, BitRate: st.BitRate, BitDepth: st.BitDepth,
					Profile: st.Profile, Level: st.Level, AverageFrameRate: st.AverageFrameRate,
					RealFrameRate: st.RealFrameRate, AspectRatio: st.AspectRatio,
					IsInterlaced: st.IsInterlaced, IsDefault: st.IsDefault, IsForced: st.IsForced,
					IsExternal: st.IsExternal, Channels: st.Channels, ChannelLayout: st.ChannelLayout,
					SampleRate: st.SampleRate, ColorPrimaries: st.ColorPrimaries,
					ColorSpace: st.ColorSpace, ColorTransfer: st.ColorTransfer,
					PixelFormat: st.PixelFormat, RefFrames: st.RefFrames,
					VideoRange: st.VideoRange, ExtendedVideoType: st.ExtendedVideoType,
				})
			}
			return out
		}
	}
	var out []embyMediaStreamDTO
	idx := 0
	if src.Width > 0 || src.Height > 0 || src.VideoCodec != "" {
		st := embyMediaStreamDTO{Type: "Video", Index: idx, Codec: src.VideoCodec, Width: src.Width, Height: src.Height}
		idx++
		var parts []string
		// Tier by width or height, whichever is higher: cropped 2160p
		// content (3840x1904) must not be labelled 1080p.
		switch {
		case src.Width >= 3800 || src.Height >= 2000:
			parts = append(parts, "2160p")
		case src.Width >= 1900 || src.Height >= 1000:
			parts = append(parts, "1080p")
		case src.Width >= 1200 || src.Height >= 700:
			parts = append(parts, "720p")
		case src.Height > 0:
			parts = append(parts, strconv.Itoa(src.Height)+"p")
		}
		if src.VideoCodec != "" {
			parts = append(parts, strings.ToUpper(src.VideoCodec))
		}
		if src.HDR != "" {
			parts = append(parts, src.HDR)
		}
		st.DisplayTitle = strings.Join(parts, " ")
		out = append(out, st)
	}
	summary := src.AudioSummary
	if i := strings.Index(summary, " · "); i >= 0 {
		summary = summary[:i]
	}
	for _, entry := range strings.Split(summary, ", ") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		st := embyMediaStreamDTO{Type: "Audio", Index: idx, DisplayTitle: entry}
		idx++
		st.Codec = strings.Fields(entry)[0]
		if open := strings.LastIndex(entry, "("); open >= 0 && strings.HasSuffix(entry, ")") {
			st.Language = entry[open+1 : len(entry)-1]
		}
		out = append(out, st)
	}
	return out
}

// mediaFileName picks the display file name for a media source: the
// real file name at the end of the STRM target URL, else the .strm
// file's own stem.
func mediaFileName(targetURL, strmPath string) string {
	if targetURL != "" {
		if u, err := url.Parse(targetURL); err == nil && u.Path != "" {
			if base := path.Base(u.Path); base != "." && base != "/" {
				return base
			}
		}
		return filepath.Base(targetURL)
	}
	return filepath.Base(strmPath)
}

func (s *Server) handleEmbyPlaybackInfo(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
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
	target := it
	if it.Type == "series" || it.Type == "season" {
		target, err = s.resolvePlayable(it)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	resp := embyPlaybackInfoDTO{MediaSources: []embyMediaSourceDTO{}}
	if target == nil {
		resp.PlaySessionId = "yk-" + strconv.FormatInt(id, 10)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.PlaySessionId = "yk-" + strconv.FormatInt(target.ID, 10)
	srcs, err := s.st.SourcesOf(target.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(srcs) == 0 {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	src := srcs[0]
	targetURL := ""
	if src.StrmPath != "" {
		targetURL = readStrmTarget(src.StrmPath)
	}
	name := mediaFileName(targetURL, src.StrmPath)
	playPath := targetURL
	if playPath == "" {
		playPath = "http://" + r.Host + "/api/items/" + strconv.FormatInt(target.ID, 10) + "/stream"
	}
	container := ""
	if ext := filepath.Ext(name); ext != "" {
		container = strings.ToLower(strings.TrimPrefix(ext, "."))
	}
	ms := embyMediaSourceDTO{
		Id:                   strconv.FormatInt(target.ID, 10),
		Name:                 name,
		Path:                 playPath,
		Protocol:             "Http",
		Container:            container,
		Type:                 "Default",
		SupportsDirectPlay:   true,
		SupportsDirectStream: true,
		SupportsTranscoding:  false,
		Size:                 src.SizeBytes,
		Bitrate:              src.Bitrate,
		MediaStreams:         mediaStreamsOf(src),
	}
	if target.RuntimeMin > 0 {
		ms.RunTimeTicks = int64(target.RuntimeMin) * 60 * 10_000_000
	}
	resp.MediaSources = append(resp.MediaSources, ms)
	writeJSON(w, http.StatusOK, resp)
}

// ---- Videos stream (direct clients) ----

func (s *Server) handleEmbyVideoStream(w http.ResponseWriter, r *http.Request) {
	if f := r.PathValue("file"); f != "" && !strings.HasPrefix(f, "stream.") {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad item id")
		return
	}
	s.serveItemStream(w, r, id)
}

// ---- session progress reporting ----

// flexID accepts an Emby id as either a JSON string or a JSON number.
type flexID string

func (f *flexID) UnmarshalJSON(b []byte) error {
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) == 0 || string(b) == "null" {
		*f = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexID(s)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexID(strconv.FormatInt(n, 10))
	return nil
}

type sessionReport struct {
	ItemId        flexID `json:"ItemId"`
	PositionTicks int64  `json:"PositionTicks"`
}

// handleSessionReport records a Sessions/Playing[/Progress|/Stopped]
// report into the same user_data the web UI uses (played at >=90% of
// the item's runtime, matching the web progress semantics) and always
// answers 204: a progress report must never fail playback.
func (s *Server) handleSessionReport(w http.ResponseWriter, r *http.Request) {
	var p sessionReport
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&p)
	}
	if id, err := strconv.ParseInt(string(p.ItemId), 10, 64); err == nil && id > 0 {
		if it, gerr := s.st.GetItem(id); gerr == nil && it != nil {
			ticks := p.PositionTicks
			if ticks < 0 {
				ticks = 0
			}
			played := false
			if rt := int64(it.RuntimeMin) * 60 * 10_000_000; rt > 0 && ticks*10 >= rt*9 {
				played = true
			}
			_ = s.st.UpsertProgress(id, s.currentUserID(r), ticks, played, time.Now().Unix())
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- authentication ----

type authenticateByNameRequest struct {
	Username string `json:"Username"`
	Pw       string `json:"Pw"`
}

// embyUserDTOFull renders the full Emby UserDto shape. It mirrors a
// real Emby server closely — notably ServerId, which clients like
// SenPlayer cross-check against System/Info's Id while validating a
// saved server; a thin DTO without it reads as an unreadable server
// response even with HTTP 200.
func (s *Server) embyUserDTOFull(id, name string, hasPassword, isAdmin bool, createdAt int64) map[string]any {
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}
	created := time.Unix(createdAt, 0).UTC().Format("2006-01-02T15:04:05.0000000Z")
	return map[string]any{
		"Name":                      name,
		"ServerId":                  s.ensureServerID(),
		"Id":                        id,
		"DateCreated":               created,
		"HasPassword":               hasPassword,
		"HasConfiguredPassword":     hasPassword,
		"HasConfiguredEasyPassword": false,
		"LastLoginDate":             created,
		"LastActivityDate":          created,
		"Configuration": map[string]any{
			"AudioLanguagePreference":    "",
			"DisplayMissingEpisodes":     false,
			"EnableLocalPassword":        false,
			"EnableNextEpisodeAutoPlay":  true,
			"HidePlayedInLatest":         false,
			"LatestItemsExcludes":        []any{},
			"MyMediaExcludes":            []any{},
			"OrderedViews":               []any{},
			"PlayDefaultAudioTrack":      true,
			"RememberAudioSelections":    true,
			"RememberSubtitleSelections": true,
			"SubtitleLanguagePreference": "",
			"SubtitleMode":               "Default",
		},
		"Policy": map[string]any{
			"IsAdministrator":                isAdmin,
			"IsHidden":                       false,
			"IsDisabled":                     false,
			"EnableAllFolders":               true,
			"EnableAllDevices":               true,
			"EnableAllChannels":              true,
			"EnableContentDeletion":          isAdmin,
			"EnableContentDownloading":       true,
			"EnableLiveTvAccess":             false,
			"EnableLiveTvManagement":         false,
			"EnableMediaPlayback":            true,
			"EnableAudioPlaybackTranscoding": false,
			"EnableVideoPlaybackTranscoding": false,
			"EnablePlaybackRemuxing":         false,
			"EnableSyncTranscoding":          false,
			"AllowCameraUpload":              false,
			"AllowSharingPersonalItems":      false,
			"BlockUnratedItems":              []any{},
			"BlockedTags":                    []any{},
			"AccessSchedules":                []any{},
			"InvalidLoginAttemptCount":       0,
			"LoginAttemptsBeforeLockout":     0,
			"MaxActiveSessions":              0,
			"RemoteClientBitrateLimit":       0,
			"AuthenticationProviderId":       "Emby.Server.Implementations.Library.DefaultAuthenticationProvider",
			"PasswordResetProviderId":        "Emby.Server.Implementations.Library.DefaultPasswordResetProvider",
			"SyncPlayAccess":                 "CreateAndJoinGroups",
		},
	}
}

// embyUserDTO is the synthetic fallback user, served when the
// requested id names no real account (and for the user list while no
// accounts exist at all). Nan's helper probes arbitrary user ids when
// registering a server, so the probe keeps working exactly as it did
// before real accounts landed (batch 10 behavior).
func (s *Server) embyUserDTO() map[string]any {
	return s.embyUserDTOFull(s.ensureServerID(), "yingku", false, true, 0)
}

// embyUserDTOFor renders a real account as an Emby user DTO.
func (s *Server) embyUserDTOFor(u *store.User) map[string]any {
	return s.embyUserDTOFull(u.ID, u.Name, true, u.IsAdmin, u.CreatedAt)
}

func (s *Server) handleEmbyUser(w http.ResponseWriter, r *http.Request) {
	if u, err := s.st.GetUserByID(r.PathValue("userId")); err == nil && u != nil {
		writeJSON(w, http.StatusOK, s.embyUserDTOFor(u))
		return
	}
	writeJSON(w, http.StatusOK, s.embyUserDTO())
}

func (s *Server) handleEmbyUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.st.ListUsers()
	if err == nil && len(users) > 0 {
		out := make([]any, 0, len(users))
		for _, u := range users {
			out = append(out, s.embyUserDTOFor(u))
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	writeJSON(w, http.StatusOK, []any{s.embyUserDTO()})
}

// handleAuthenticateByName answers Emby's by-name login with real
// credential checks (batch 12): the username must exist and the
// password must verify, otherwise 401. On success the caller gets a
// freshly issued per-user access token — the token Nan and Emby
// clients present on later calls, which is also what pins their
// progress reports to this account.
func (s *Server) handleAuthenticateByName(w http.ResponseWriter, r *http.Request) {
	var req authenticateByNameRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	u, err := s.verifyLogin(req.Username, req.Pw)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if u == nil {
		writeErr(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	token, err := s.st.IssueToken(u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"User":        s.embyUserDTOFor(u),
		"AccessToken": token,
		"SessionInfo": map[string]any{
			"Id":     "yk-session",
			"UserId": u.ID,
		},
	})
}
