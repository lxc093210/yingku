// Package api exposes yingku's HTTP API and embedded web UI.
//
// The internal JSON shapes deliberately mirror Emby's item model (Id, Name,
// Type, ParentId, SeriesId, IndexNumber, ParentIndexNumber, ProviderIds) so
// the later Emby-protocol layer maps 1:1. NanShare still fronts the
// Emby-protocol surface and performs STRM playback resolution for external
// clients; the built-in web UI additionally gets a minimal playback path:
// /stream answers with a 302 to the item's STRM target (Nan's resolver),
// which in turn redirects to the CDN — bytes never pass through yingku.
package api

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"yingku/internal/scan"
	"yingku/internal/store"
	"yingku/internal/web"
)

// defaultUserID is the legacy implicit user: the user_data owner from
// before accounts existed (batch 12), kept as the attribution fallback
// while no accounts exist. It equals store.LegacyUserID.
const defaultUserID = store.LegacyUserID

// settingTMDBKey is the app_settings key holding the TMDB API key.
const settingTMDBKey = "tmdb_api_key"

// Server wires the store, scanner and config into an http.Handler.
type Server struct {
	st          *store.Store
	scanner     *scan.Scanner
	defaultRoot string
	fsRoot      string
	imgLocks    *keyedLocks
	bfMu        sync.Mutex
	bf          backfillState
}

// New builds the HTTP handler.
func New(st *store.Store, scanner *scan.Scanner, defaultRoot string) http.Handler {
	fsRoot := os.Getenv("YINGKU_FS_ROOT")
	if fsRoot == "" {
		fsRoot = string(filepath.Separator)
	}
	if abs, err := filepath.Abs(fsRoot); err == nil {
		fsRoot = filepath.Clean(abs)
	}
	s := &Server{st: st, scanner: scanner, defaultRoot: defaultRoot, fsRoot: fsRoot, imgLocks: newKeyedLocks()}
	// A key (and the stable server id) exist from first start so the
	// settings page always has something usable to show/copy.
	_, _ = s.ensureAPIKey()
	_ = s.ensureServerID()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/home", s.handleHome)
	mux.HandleFunc("GET /api/favorites", s.handleFavorites)
	mux.HandleFunc("GET /api/libraries", s.handleLibraries)
	mux.HandleFunc("POST /api/libraries", s.handleLibraryCreate)
	mux.HandleFunc("GET /api/libraries/{id}", s.handleLibraryGet)
	mux.HandleFunc("PUT /api/libraries/{id}", s.handleLibraryUpdate)
	mux.HandleFunc("DELETE /api/libraries/{id}", s.handleLibraryDelete)
	mux.HandleFunc("POST /api/libraries/{id}/scan", s.handleLibraryScan)
	mux.HandleFunc("GET /api/libraries/{id}/items", s.handleLibraryItems)
	mux.HandleFunc("GET /api/items/{id}", s.handleItem)
	mux.HandleFunc("DELETE /api/items/{id}", s.handleItemDelete)
	mux.HandleFunc("GET /api/items/{id}/delete-preview", s.handleDeletePreview)
	mux.HandleFunc("GET /api/items/{id}/subtitles", s.handleItemSubtitles)
	mux.HandleFunc("GET /api/subtitles/{subId}/track", s.handleSubtitleTrack)
	mux.HandleFunc("GET /api/items/{id}/image/backdrop-hi", s.handleBackdropHi)
	mux.HandleFunc("GET /api/items/{id}/image/{kind}", s.handleImage)
	mux.HandleFunc("GET /api/items/{id}/play-info", s.handlePlayInfo)
	mux.HandleFunc("GET /api/items/{id}/stream", s.handleStream)
	mux.HandleFunc("POST /api/items/{id}/progress", s.handleProgress)
	mux.HandleFunc("POST /api/items/{id}/progress/reset", s.handleProgressReset)
	mux.HandleFunc("POST /api/items/{id}/favorite", s.handleFavorite)
	mux.HandleFunc("GET /api/settings", s.handleSettingsGet)
	mux.HandleFunc("PUT /api/settings", s.handleSettingsPut)
	mux.HandleFunc("GET /api/settings/api-key", s.handleAPIKeyGet)
	mux.HandleFunc("POST /api/settings/api-key/regenerate", s.handleAPIKeyRegenerate)
	// Accounts (batch 12): web login/logout/session probe, the one-time
	// bootstrap that creates the first administrator, and the admin
	// user-management surface.
	mux.HandleFunc("GET /api/auth/me", s.handleAuthMe)
	mux.HandleFunc("POST /api/auth/login", s.handleAuthLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleAuthLogout)
	mux.HandleFunc("POST /api/users/bootstrap", s.handleUserBootstrap)
	mux.HandleFunc("GET /api/users", s.handleUsersList)
	mux.HandleFunc("POST /api/users", s.handleUserCreate)
	mux.HandleFunc("DELETE /api/users/{id}", s.handleUserDelete)
	mux.HandleFunc("POST /api/users/{id}/password", s.handleUserPassword)
	// Emby-protocol handshake surface (Nan / Emby clients register here).
	mux.HandleFunc("GET /emby/System/Info", s.handleSystemInfo)
	mux.HandleFunc("GET /System/Info", s.handleSystemInfo)
	mux.HandleFunc("GET /emby/System/Info/Public", s.handleSystemInfo)
	mux.HandleFunc("GET /System/Info/Public", s.handleSystemInfo)
	mux.HandleFunc("GET /emby/System/Ping", s.handleSystemPing)
	mux.HandleFunc("GET /System/Ping", s.handleSystemPing)
	mux.HandleFunc("GET /emby/System/Ext/ServerDomains", s.handleServerDomains)
	mux.HandleFunc("GET /System/Ext/ServerDomains", s.handleServerDomains)
	// Emby core read surface (batch 9): the endpoints Nan's Emby helper
	// needs to browse and play the library. Every route exists with and
	// without the /emby prefix.
	mux.HandleFunc("GET /emby/Users/Public", s.handleUsersPublic)
	mux.HandleFunc("GET /Users/Public", s.handleUsersPublic)
	mux.HandleFunc("POST /emby/Users/AuthenticateByName", s.handleAuthenticateByName)
	mux.HandleFunc("POST /Users/AuthenticateByName", s.handleAuthenticateByName)
	mux.HandleFunc("GET /emby/Users/{userId}", s.handleEmbyUser)
	mux.HandleFunc("GET /Users/{userId}", s.handleEmbyUser)
	mux.HandleFunc("GET /emby/Users", s.handleEmbyUsers)
	mux.HandleFunc("GET /Users", s.handleEmbyUsers)
	mux.HandleFunc("GET /emby/Library/VirtualFolders/Query", s.handleVirtualFoldersQuery)
	mux.HandleFunc("GET /Library/VirtualFolders/Query", s.handleVirtualFoldersQuery)
	mux.HandleFunc("POST /emby/Items/{id}/Images/Primary", s.handleEmbyLibraryImageUpload)
	mux.HandleFunc("POST /Items/{id}/Images/Primary", s.handleEmbyLibraryImageUpload)
	mux.HandleFunc("GET /emby/Library/MediaFolders", s.handleMediaFolders)
	mux.HandleFunc("GET /Library/MediaFolders", s.handleMediaFolders)
	mux.HandleFunc("GET /emby/Items", s.handleEmbyItems)
	mux.HandleFunc("GET /Items", s.handleEmbyItems)
	mux.HandleFunc("GET /emby/Users/{userId}/Items", s.handleEmbyItems)
	mux.HandleFunc("GET /Users/{userId}/Items", s.handleEmbyItems)
	mux.HandleFunc("GET /emby/Items/{id}", s.handleEmbyItem)
	mux.HandleFunc("GET /Items/{id}", s.handleEmbyItem)
	mux.HandleFunc("GET /emby/Users/{userId}/Items/{id}", s.handleEmbyItem)
	mux.HandleFunc("GET /Users/{userId}/Items/{id}", s.handleEmbyItem)
	mux.HandleFunc("GET /emby/Items/{id}/Images/{imageType}", s.handleEmbyImage)
	mux.HandleFunc("GET /Items/{id}/Images/{imageType}", s.handleEmbyImage)
	mux.HandleFunc("GET /emby/Items/{id}/Images/{imageType}/{index}", s.handleEmbyImage)
	mux.HandleFunc("GET /Items/{id}/Images/{imageType}/{index}", s.handleEmbyImage)
	mux.HandleFunc("/emby/Items/{id}/PlaybackInfo", s.handleEmbyPlaybackInfo)
	mux.HandleFunc("/Items/{id}/PlaybackInfo", s.handleEmbyPlaybackInfo)
	mux.HandleFunc("GET /emby/Videos/{id}/stream", s.handleEmbyVideoStream)
	mux.HandleFunc("GET /Videos/{id}/stream", s.handleEmbyVideoStream)
	mux.HandleFunc("GET /emby/Videos/{id}/{file}", s.handleEmbyVideoStream)
	mux.HandleFunc("GET /Videos/{id}/{file}", s.handleEmbyVideoStream)
	mux.HandleFunc("POST /emby/Sessions/Playing", s.handleSessionReport)
	mux.HandleFunc("POST /Sessions/Playing", s.handleSessionReport)
	mux.HandleFunc("POST /emby/Sessions/Playing/Progress", s.handleSessionReport)
	mux.HandleFunc("POST /Sessions/Playing/Progress", s.handleSessionReport)
	mux.HandleFunc("POST /emby/Sessions/Playing/Stopped", s.handleSessionReport)
	mux.HandleFunc("POST /Sessions/Playing/Stopped", s.handleSessionReport)
	// Emby client mount surface (batch 11): the endpoints SenPlayer /
	// Infuse call right after login — Views is their first stop, and
	// its absence read as a bare 404 on mount. Same dual registration.
	mux.HandleFunc("GET /emby/Users/{userId}/Views", s.handleViews)
	mux.HandleFunc("GET /Users/{userId}/Views", s.handleViews)
	mux.HandleFunc("GET /emby/Users/{userId}/Items/Latest", s.handleEmbyLatest)
	mux.HandleFunc("GET /Users/{userId}/Items/Latest", s.handleEmbyLatest)
	mux.HandleFunc("GET /emby/Items/Latest", s.handleEmbyLatest)
	mux.HandleFunc("GET /Items/Latest", s.handleEmbyLatest)
	mux.HandleFunc("GET /emby/Users/{userId}/Items/Resume", s.handleEmbyResume)
	mux.HandleFunc("GET /Users/{userId}/Items/Resume", s.handleEmbyResume)
	mux.HandleFunc("GET /emby/Items/Resume", s.handleEmbyResume)
	mux.HandleFunc("GET /Items/Resume", s.handleEmbyResume)
	mux.HandleFunc("GET /emby/Shows/NextUp", s.handleNextUp)
	mux.HandleFunc("GET /Shows/NextUp", s.handleNextUp)
	mux.HandleFunc("GET /emby/Shows/{seriesId}/Seasons", s.handleShowSeasons)
	mux.HandleFunc("GET /Shows/{seriesId}/Seasons", s.handleShowSeasons)
	mux.HandleFunc("GET /emby/Shows/{seriesId}/Episodes", s.handleShowEpisodes)
	mux.HandleFunc("GET /Shows/{seriesId}/Episodes", s.handleShowEpisodes)
	mux.HandleFunc("GET /emby/Items/{id}/Similar", s.handleSimilar)
	mux.HandleFunc("GET /Items/{id}/Similar", s.handleSimilar)
	mux.HandleFunc("GET /emby/Items/Counts", s.handleItemCounts)
	mux.HandleFunc("GET /Items/Counts", s.handleItemCounts)
	mux.HandleFunc("GET /emby/Search/Hints", s.handleSearchHints)
	mux.HandleFunc("GET /Search/Hints", s.handleSearchHints)
	mux.HandleFunc("GET /emby/System/Endpoint", s.handleSystemEndpoint)
	mux.HandleFunc("GET /System/Endpoint", s.handleSystemEndpoint)
	mux.HandleFunc("POST /emby/Users/{userId}/FavoriteItems/{itemId}", s.handleFavoriteItems)
	mux.HandleFunc("POST /Users/{userId}/FavoriteItems/{itemId}", s.handleFavoriteItems)
	mux.HandleFunc("DELETE /emby/Users/{userId}/FavoriteItems/{itemId}", s.handleFavoriteItems)
	mux.HandleFunc("DELETE /Users/{userId}/FavoriteItems/{itemId}", s.handleFavoriteItems)
	mux.HandleFunc("POST /emby/Users/{userId}/PlayedItems/{itemId}", s.handlePlayedItems)
	mux.HandleFunc("POST /Users/{userId}/PlayedItems/{itemId}", s.handlePlayedItems)
	mux.HandleFunc("DELETE /emby/Users/{userId}/PlayedItems/{itemId}", s.handlePlayedItems)
	mux.HandleFunc("DELETE /Users/{userId}/PlayedItems/{itemId}", s.handlePlayedItems)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/fs/list", s.handleFSList)
	mux.HandleFunc("POST /api/maintenance/tmdb-backfill", s.handleBackfillStart)
	mux.HandleFunc("GET /api/maintenance/tmdb-backfill/status", s.handleBackfillStatus)
	mux.HandleFunc("POST /api/scan", s.handleScanStart)
	mux.HandleFunc("GET /api/scan/status", s.handleScanStatus)
	mux.HandleFunc("/", s.handleUI)
	return s.logMiddleware(s.authMiddleware(mux))
}

// ---- DTOs (Emby-shaped) ----

type libraryDTO struct {
	ID        int64    `json:"Id"`
	Name      string   `json:"Name"`
	Kind      string   `json:"Kind"`
	Paths     []string `json:"Paths"`
	ItemCount int      `json:"ItemCount"`
	// Thumbs holds up to three backdrop-owning top-level item ids of the
	// library (highest rated first) for the home page's library cards;
	// populated by the list/home handlers, empty elsewhere.
	Thumbs []int64 `json:"Thumbs"`
}

func toLibraryDTO(l store.Library) libraryDTO {
	paths := l.Paths
	if paths == nil {
		paths = []string{}
	}
	return libraryDTO{ID: l.ID, Name: l.Name, Kind: l.Kind, Paths: paths, ItemCount: l.ItemCount, Thumbs: []int64{}}
}

// libraryDTOs converts libraries and fills each one's Thumbs in a single
// extra query (best-effort: a thumbs failure leaves them empty).
func (s *Server) libraryDTOs(libs []store.Library) []libraryDTO {
	out := make([]libraryDTO, 0, len(libs))
	thumbs, _ := s.st.LibraryThumbs()
	for _, l := range libs {
		d := toLibraryDTO(l)
		if t := thumbs[l.ID]; len(t) > 0 {
			d.Thumbs = t
		}
		out = append(out, d)
	}
	return out
}

type sourceDTO struct {
	ID              int64  `json:"Id"`
	ItemID          int64  `json:"ItemId"`
	Path            string `json:"Path"`
	TargetURL       string `json:"TargetUrl"`
	Width           int    `json:"Width"`
	Height          int    `json:"Height"`
	VideoCodec      string `json:"VideoCodec"`
	HDR             string `json:"Hdr"`
	AudioSummary    string `json:"AudioSummary"`
	MediaInfoSource string `json:"MediaInfoSource"`
}

type subtitleDTO struct {
	Path     string `json:"Path"`
	Language string `json:"Language"`
}

type personDTO struct {
	Name     string `json:"Name"`
	Role     string `json:"Role,omitempty"`
	Type     string `json:"Type,omitempty"`
	ImageURL string `json:"ImageUrl,omitempty"`
}

func toPersonDTO(p store.Person) personDTO {
	d := personDTO{Name: p.Name, Role: p.Role, Type: p.Kind}
	// Only absolute http(s) thumbs are directly renderable; local relative
	// paths from NFOs are not served, so they stay empty.
	if strings.HasPrefix(p.Thumb, "http://") || strings.HasPrefix(p.Thumb, "https://") {
		d.ImageURL = p.Thumb
	}
	return d
}

type userDataDTO struct {
	Played                bool  `json:"Played"`
	Favorite              bool  `json:"Favorite"`
	PlaybackPositionTicks int64 `json:"PlaybackPositionTicks"`
}

type itemDTO struct {
	ID                int64             `json:"Id"`
	Name              string            `json:"Name"`
	OriginalTitle     string            `json:"OriginalTitle,omitempty"`
	Type              string            `json:"Type"`
	ParentID          *int64            `json:"ParentId,omitempty"`
	SeriesID          *int64            `json:"SeriesId,omitempty"`
	SeriesName        string            `json:"SeriesName,omitempty"`
	IndexNumber       *int              `json:"IndexNumber,omitempty"`
	ParentIndexNumber *int              `json:"ParentIndexNumber,omitempty"`
	ProductionYear    int               `json:"ProductionYear,omitempty"`
	Overview          string            `json:"Overview,omitempty"`
	CommunityRating   float64           `json:"CommunityRating,omitempty"`
	RunTimeTicks      int64             `json:"RunTimeTicks,omitempty"`
	Genres            []string          `json:"Genres"`
	ProviderIds       map[string]string `json:"ProviderIds"`
	HasPoster         bool              `json:"HasPoster"`
	HasBackdrop       bool              `json:"HasBackdrop"`
	DateCreated       int64             `json:"DateCreated"`
	MediaSources      []sourceDTO       `json:"MediaSources,omitempty"`
	Subtitles         []subtitleDTO     `json:"Subtitles,omitempty"`
	Seasons           []seasonDTO       `json:"Seasons,omitempty"`
	People            []personDTO       `json:"People,omitempty"`
	UserData          *userDataDTO      `json:"UserData,omitempty"`
	// PlaybackPositionTicks is set on home ContinueWatching entries.
	PlaybackPositionTicks int64 `json:"PlaybackPositionTicks,omitempty"`
	// ContinueWatching episode entries also carry these for the
	// "S1:E7 - 集名" caption line.
	SeasonNumber  *int   `json:"SeasonNumber,omitempty"`
	EpisodeNumber *int   `json:"EpisodeNumber,omitempty"`
	EpisodeName   string `json:"EpisodeName,omitempty"`
}

type seasonDTO struct {
	itemDTO
	Episodes []itemDTO `json:"Episodes"`
}

var embyType = map[string]string{
	"movie": "Movie", "series": "Series", "season": "Season", "episode": "Episode",
}

func toDTO(it *store.Item) itemDTO {
	d := itemDTO{
		ID: it.ID, Name: it.Title, OriginalTitle: it.OriginalTitle,
		Type: embyType[it.Type], ParentID: it.ParentID, SeriesID: it.SeriesID,
		ProductionYear: it.Year, Overview: it.Overview,
		CommunityRating: it.Rating, Genres: it.Genres,
		ProviderIds: map[string]string{},
		HasPoster:   it.HasPoster, HasBackdrop: it.HasBackdrop,
		DateCreated: it.CreatedAt,
	}
	if it.RuntimeMin > 0 {
		d.RunTimeTicks = int64(it.RuntimeMin) * 60 * 10_000_000
	}
	if it.TmdbID > 0 {
		d.ProviderIds["Tmdb"] = strconv.Itoa(it.TmdbID)
	}
	if it.Type == "episode" {
		d.IndexNumber = it.EpisodeNum
		d.ParentIndexNumber = it.SeasonNum
	}
	if it.Type == "season" {
		d.IndexNumber = it.SeasonNum
	}
	return d
}

func toSourceDTO(ms store.MediaSource) sourceDTO {
	return sourceDTO{
		ID: ms.ID, ItemID: ms.ItemID, Path: ms.StrmPath, TargetURL: ms.TargetURL,
		Width: ms.Width, Height: ms.Height, VideoCodec: ms.VideoCodec, HDR: ms.HDR,
		AudioSummary: ms.AudioSummary, MediaInfoSource: ms.MediaInfoSource,
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

// ---- handlers ----

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.st.Stats()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	libs := make([]libraryDTO, 0, len(st.Libraries))
	for _, l := range st.Libraries {
		libs = append(libs, toLibraryDTO(l))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Libraries":        libs,
		"TotalItems":       st.TotalItems,
		"TotalMovies":      st.TotalMovies,
		"TotalSeries":      st.TotalSeries,
		"TotalEpisodes":    st.TotalEpisodes,
		"MediaInfoSources": st.MediaInfoSource,
	})
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	// The Emby-style home page renders only the library cards and the
	// continue-watching rail; the per-library "latest" rows come from
	// the library items endpoint. Hero/Recent assembly used to dominate
	// this handler's cost (~800ms on the live library) for data the UI
	// never reads, so it is no longer computed here.
	libs, err := s.st.ListLibraries()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	libDTOs := s.libraryDTOs(libs)
	cont, err := s.st.ContinueWatching(s.currentUserID(r), 12)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	contDTOs := make([]itemDTO, 0, len(cont))
	for _, e := range cont {
		d := toDTO(e.Item)
		d.PlaybackPositionTicks = e.ProgressTicks
		if e.SeriesName != "" {
			d.SeriesName = e.SeriesName
		}
		d.SeasonNumber = e.SeasonNumber
		d.EpisodeNumber = e.EpisodeNumber
		d.EpisodeName = e.EpisodeName
		contDTOs = append(contDTOs, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Libraries": libDTOs, "ContinueWatching": contDTOs,
	})
}

func (s *Server) handleLibraries(w http.ResponseWriter, r *http.Request) {
	libs, err := s.st.ListLibraries()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.libraryDTOs(libs))
}

// handleFavorites lists the current user's favorited top-level items.
func (s *Server) handleFavorites(w http.ResponseWriter, r *http.Request) {
	items, err := s.st.Favorites(s.currentUserID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]itemDTO, 0, len(items))
	for _, it := range items {
		out = append(out, toDTO(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{"Items": out})
}

// ---- library management ----

type libraryPayload struct {
	Name  string   `json:"Name"`
	Kind  string   `json:"Kind"`
	Paths []string `json:"Paths"`
}

// parse validates and normalizes a library payload.
func (p *libraryPayload) parse() (string, string, []string, error) {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return "", "", nil, errBad("library name is required")
	}
	kind := strings.ToLower(strings.TrimSpace(p.Kind))
	if kind == "" {
		kind = "mixed"
	}
	switch kind {
	case "movie", "series", "mixed":
	default:
		return "", "", nil, errBad("kind must be movie, series or mixed")
	}
	var paths []string
	seen := map[string]bool{}
	for _, raw := range p.Paths {
		clean := filepath.Clean(strings.TrimSpace(raw))
		if clean == "." || !filepath.IsAbs(clean) {
			return "", "", nil, errBad("paths must be absolute: " + raw)
		}
		if !seen[clean] {
			seen[clean] = true
			paths = append(paths, clean)
		}
	}
	if len(paths) == 0 {
		return "", "", nil, errBad("at least one root path is required")
	}
	return name, kind, paths, nil
}

type badRequest string

func errBad(msg string) error      { return badRequest(msg) }
func (e badRequest) Error() string { return string(e) }

func decodeLibraryPayload(w http.ResponseWriter, r *http.Request) (string, string, []string, bool) {
	var p libraryPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return "", "", nil, false
	}
	name, kind, paths, err := p.parse()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return "", "", nil, false
	}
	return name, kind, paths, true
}

func (s *Server) handleLibraryCreate(w http.ResponseWriter, r *http.Request) {
	name, kind, paths, ok := decodeLibraryPayload(w, r)
	if !ok {
		return
	}
	id, err := s.st.CreateLibrary(name, kind, paths)
	if err != nil {
		if errors.Is(err, store.ErrPathOwned) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	lib, err := s.st.GetLibrary(id)
	if err != nil || lib == nil {
		writeErr(w, http.StatusInternalServerError, "library created but not readable")
		return
	}
	writeJSON(w, http.StatusCreated, toLibraryDTO(*lib))
}

func (s *Server) handleLibraryGet(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad library id")
		return
	}
	lib, err := s.st.GetLibrary(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if lib == nil {
		writeErr(w, http.StatusNotFound, "library not found")
		return
	}
	writeJSON(w, http.StatusOK, toLibraryDTO(*lib))
}

func (s *Server) handleLibraryUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad library id")
		return
	}
	name, kind, paths, ok := decodeLibraryPayload(w, r)
	if !ok {
		return
	}
	err = s.st.UpdateLibrary(id, name, kind, paths)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "library not found")
		return
	}
	if errors.Is(err, store.ErrPathOwned) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	lib, err := s.st.GetLibrary(id)
	if err != nil || lib == nil {
		writeErr(w, http.StatusInternalServerError, "library updated but not readable")
		return
	}
	writeJSON(w, http.StatusOK, toLibraryDTO(*lib))
}

func (s *Server) handleLibraryDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad library id")
		return
	}
	err = s.st.DeleteLibrary(id)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "library not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleLibraryScan(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad library id")
		return
	}
	lib, err := s.st.GetLibrary(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if lib == nil {
		writeErr(w, http.StatusNotFound, "library not found")
		return
	}
	if err := s.scanner.StartLibrary(id); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, s.scanner.Status())
}

func (s *Server) handleLibraryItems(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad library id")
		return
	}
	lib, err := s.st.GetLibrary(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if lib == nil {
		writeErr(w, http.StatusNotFound, "library not found")
		return
	}
	limit := 60
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	items, next, err := s.st.ListTopLevel(id, r.URL.Query().Get("sort"), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out := make([]itemDTO, 0, len(items))
	for _, it := range items {
		out = append(out, toDTO(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Items": out, "NextCursor": next, "TotalRecordCount": lib.ItemCount,
	})
}

func (s *Server) handleItem(w http.ResponseWriter, r *http.Request) {
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
	// Metadata lazy fill: for movies/series whose local metadata is
	// incomplete (no overview or no rating), pull the TMDB details once
	// and fill ONLY the empty fields — NFO values always win. The item
	// is re-read afterwards so this very response carries the new data.
	// Best-effort like the cast fill below: failures keep local data.
	if (it.Type == "movie" || it.Type == "series") && it.TmdbID > 0 && (it.Overview == "" || it.Rating == 0 || !hasHan(it.Title)) {
		if cfg, cerr := s.tmdbCfg(); cerr == nil && cfg.available() {
			kind := "movie"
			if it.Type == "series" {
				kind = "tv"
			}
			if title, ov, rating, genres, runtime, ferr := fetchTMDBDetails(cfg, kind, it.TmdbID); ferr == nil {
				filled := false
				if uerr := s.st.FillItemMeta(it.ID, ov, rating, genres, runtime); uerr == nil {
					filled = true
				}
				// Language preference: Chinese metadata by default.
				// A title with no Chinese characters is replaced by
				// TMDB's zh-CN title when that one is Chinese; when
				// TMDB has no Chinese title either, the current
				// (English/other) title stays.
				if title != "" && title != it.Title && !hasHan(it.Title) && hasHan(title) {
					if uerr := s.st.FillItemTitle(it.ID, title); uerr == nil {
						filled = true
					}
				}
				if filled {
					if fresh, gerr := s.st.GetItem(it.ID); gerr == nil && fresh != nil {
						it = fresh
					}
				}
			}
		}
	}
	d := toDTO(it)
	switch it.Type {
	case "series":
		seasons, err := s.st.Children(it.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		d.Seasons = []seasonDTO{}
		for _, sn := range seasons {
			if sn.Type != "season" {
				continue
			}
			sd := seasonDTO{itemDTO: toDTO(sn), Episodes: []itemDTO{}}
			eps, err := s.st.Children(sn.ID)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			for _, ep := range eps {
				ed := toDTO(ep)
				ed.SeriesName = it.Title
				srcs, err := s.st.SourcesOf(ep.ID)
				if err != nil {
					writeErr(w, http.StatusInternalServerError, err.Error())
					return
				}
				for _, ms := range srcs {
					ed.MediaSources = append(ed.MediaSources, toSourceDTO(ms))
				}
				// Per-episode watched/progress state for the detail page's
				// episode list and resume line.
				eud, err := s.st.UserDataOf(ep.ID, s.currentUserID(r))
				if err != nil {
					writeErr(w, http.StatusInternalServerError, err.Error())
					return
				}
				ed.UserData = &userDataDTO{
					Played: eud.Played, Favorite: eud.Favorite,
					PlaybackPositionTicks: eud.ProgressTicks,
				}
				sd.Episodes = append(sd.Episodes, ed)
			}
			d.Seasons = append(d.Seasons, sd)
		}
	case "movie", "episode":
		srcs, err := s.st.SourcesOf(it.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, ms := range srcs {
			d.MediaSources = append(d.MediaSources, toSourceDTO(ms))
		}
		subs, err := s.st.SubtitlesOf(it.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, sub := range subs {
			d.Subtitles = append(d.Subtitles, subtitleDTO{Path: sub.Path, Language: sub.Lang})
		}
		if it.Type == "episode" && it.SeriesID != nil {
			if series, err := s.st.GetItem(*it.SeriesID); err == nil && series != nil {
				d.SeriesName = series.Title
			}
		}
	}
	// Cast: local NFO people first; for movies/series with a TMDB id but
	// no cast photos, lazily fill from TMDB when an API key is configured
	// (best-effort: failures keep whatever local data exists).
	people, err := s.st.PeopleOf(it.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if (it.Type == "movie" || it.Type == "series") && it.TmdbID > 0 && peopleThumbsMissing(people) {
		if cfg, cerr := s.tmdbCfg(); cerr == nil && cfg.available() {
			kind := "movie"
			if it.Type == "series" {
				kind = "tv"
			}
			if cast, ferr := fetchTMDBCast(cfg, kind, it.TmdbID); ferr == nil && len(cast) > 0 {
				_ = s.st.ReplacePeople(it.ID, cast)
				people = cast
			}
		}
	}
	for _, p := range people {
		d.People = append(d.People, toPersonDTO(p))
	}
	ud, err := s.st.UserDataOf(it.ID, s.currentUserID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	d.UserData = &userDataDTO{
		Played: ud.Played, Favorite: ud.Favorite,
		PlaybackPositionTicks: ud.ProgressTicks,
	}
	writeJSON(w, http.StatusOK, d)
}

// ---- playback, progress, favorites ----

// resolvePlayable maps any item to the movie/episode that would actually
// play: movies/episodes resolve to themselves, a series/season to its
// first episode that has a media source. nil when nothing is playable.
func (s *Server) resolvePlayable(it *store.Item) (*store.Item, error) {
	switch it.Type {
	case "movie", "episode":
		srcs, err := s.st.SourcesOf(it.ID)
		if err != nil {
			return nil, err
		}
		if len(srcs) == 0 {
			return nil, nil
		}
		return it, nil
	case "series":
		return s.st.FirstPlayableInSeries(it.ID)
	case "season":
		return s.st.FirstPlayableInSeason(it.ID)
	}
	return nil, nil
}

func (s *Server) handlePlayInfo(w http.ResponseWriter, r *http.Request) {
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
	target, err := s.resolvePlayable(it)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if target == nil {
		writeErr(w, http.StatusNotFound, "no playable source")
		return
	}
	out := map[string]any{"ItemId": target.ID, "Name": target.Title}
	if target.Type == "episode" && target.SeriesID != nil {
		if series, err := s.st.GetItem(*target.SeriesID); err == nil && series != nil {
			out["SeriesName"] = series.Title
		}
	}
	var pos int64
	if ud, err := s.st.UserDataOf(target.ID, s.currentUserID(r)); err == nil && ud != nil && !ud.Played {
		pos = ud.ProgressTicks
	}
	out["PositionTicks"] = pos
	writeJSON(w, http.StatusOK, out)
}

// handleStream 302-redirects to the item's STRM target URL, read fresh
// from the .strm file. Only movies/episodes are directly playable.
// Note: browser playback is a single-file direct stream, so embedded
// audio tracks of the underlying file cannot be switched from the web
// player (that would need transcoding/remuxing, deliberately out of
// scope); sidecar subtitles ARE switchable via /api/subtitles tracks.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad item id")
		return
	}
	s.serveItemStream(w, r, id)
}

// serveItemStream is the stream redirect for a known item id, shared
// by the web UI endpoint and the Emby Videos/{id}/stream endpoints.
func (s *Server) serveItemStream(w http.ResponseWriter, r *http.Request, id int64) {
	it, err := s.st.GetItem(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if it == nil {
		writeErr(w, http.StatusNotFound, "item not found")
		return
	}
	if it.Type != "movie" && it.Type != "episode" {
		writeErr(w, http.StatusBadRequest, "item is not directly playable")
		return
	}
	srcs, err := s.st.SourcesOf(it.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(srcs) == 0 || srcs[0].StrmPath == "" {
		writeErr(w, http.StatusBadGateway, "no media source")
		return
	}
	target := readStrmTarget(srcs[0].StrmPath)
	if target == "" {
		writeErr(w, http.StatusBadGateway, "cannot read strm target")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	// Manual redirect: http.Redirect would percent-escape the non-ASCII
	// file name in the target; the STRM URL is emitted verbatim instead.
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusFound)
}

// readStrmTarget returns the first line of a .strm file ("" on failure).
func readStrmTarget(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	line, _ := bufio.NewReader(f).ReadString('\n')
	return strings.TrimSpace(line)
}

type progressPayload struct {
	PositionSeconds float64 `json:"positionSeconds"`
	DurationSeconds float64 `json:"durationSeconds"`
}

func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request) {
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
	var p progressPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if p.PositionSeconds < 0 {
		p.PositionSeconds = 0
	}
	ticks := int64(p.PositionSeconds * 10_000_000)
	played := p.DurationSeconds > 0 && p.PositionSeconds/p.DurationSeconds >= 0.9
	if err := s.st.UpsertProgress(id, s.currentUserID(r), ticks, played, time.Now().Unix()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleProgressReset(w http.ResponseWriter, r *http.Request) {
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
	if err := s.st.ResetProgress(id, s.currentUserID(r)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleFavorite(w http.ResponseWriter, r *http.Request) {
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
	var p struct {
		Favorite bool `json:"favorite"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.st.SetFavorite(id, s.currentUserID(r), p.Favorite); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "favorite": p.Favorite})
}

// ---- app settings ----

type settingsDTO struct {
	TMDBKeySet      bool   `json:"tmdbKeySet"`
	TMDBBaseURL     string `json:"tmdbBaseUrl"`
	APIAuthRequired bool   `json:"apiAuthRequired"`
}

func (s *Server) settingsDTO() (settingsDTO, error) {
	cfg, err := s.tmdbCfg()
	if err != nil {
		return settingsDTO{}, err
	}
	// Only whether a key exists is exposed — never the key itself. The
	// base URL reported is the effective one (official default included).
	return settingsDTO{
		TMDBKeySet:      cfg.key != "",
		TMDBBaseURL:     cfg.base,
		APIAuthRequired: s.authRequired(),
	}, nil
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	dto, err := s.settingsDTO()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	// Pointer fields distinguish "leave unchanged" from "set/clear".
	var p struct {
		TMDBAPIKey      *string `json:"tmdbApiKey"`
		TMDBBaseURL     *string `json:"tmdbBaseUrl"`
		APIAuthRequired *bool   `json:"apiAuthRequired"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if p.TMDBAPIKey != nil {
		if err := s.st.SetSetting(settingTMDBKey, strings.TrimSpace(*p.TMDBAPIKey)); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if p.TMDBBaseURL != nil {
		base := strings.TrimRight(strings.TrimSpace(*p.TMDBBaseURL), "/")
		if base == defaultTMDBBase {
			base = "" // the official default needs no stored override
		}
		if err := s.st.SetSetting(settingTMDBBaseURL, base); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if p.APIAuthRequired != nil {
		v := ""
		if *p.APIAuthRequired {
			v = "true"
		}
		// An empty value deletes the row, which reads back as "off".
		if err := s.st.SetSetting(settingAPIAuthRequired, v); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	dto, err := s.settingsDTO()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	limit := 60
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	items, err := s.st.Search(q, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]itemDTO, 0, len(items))
	for _, it := range items {
		out = append(out, toDTO(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{"Items": out})
}

// pathWithinRoots reports whether clean (already filepath.Clean'ed) lies
// inside one of the registered library roots.
func (s *Server) pathWithinRoots(clean string) (bool, error) {
	roots, err := s.st.LibraryPaths()
	if err != nil {
		return false, err
	}
	for _, root := range roots {
		if withinRoot(filepath.Clean(root), clean) {
			return true, nil
		}
	}
	return false, nil
}

// serveFile streams one regular file with range support.
func serveFile(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "file missing")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		writeErr(w, http.StatusNotFound, "file missing")
		return
	}
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

// serveLocalImage serves an item's registered poster/backdrop from disk,
// guarded to files under a library root.
func (s *Server) serveLocalImage(w http.ResponseWriter, r *http.Request, id int64, kind string) {
	p, err := s.st.ImagePath(id, kind)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p == "" {
		writeErr(w, http.StatusNotFound, "no such image")
		return
	}
	clean := filepath.Clean(p)
	allowed, err := s.pathWithinRoots(clean)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !allowed {
		writeErr(w, http.StatusForbidden, "path outside library")
		return
	}
	serveFile(w, r, clean)
}

func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad item id")
		return
	}
	kind := r.PathValue("kind")
	if kind != "poster" && kind != "backdrop" {
		writeErr(w, http.StatusBadRequest, "bad image kind")
		return
	}
	s.serveItemImage(w, r, id, kind)
}

// backdropCacheDir is <data dir>/cache/backdrops, derived from the DB
// file location (the cached TMDB originals live next to the database).
func (s *Server) backdropCacheDir() string {
	return filepath.Join(filepath.Dir(s.st.DBPath()), "cache", "backdrops")
}

// handleBackdropHi serves the highest-resolution backdrop available for
// the hero: a previously cached TMDB original when present, otherwise a
// fresh TMDB fetch (cached for next time), otherwise the local backdrop.
func (s *Server) handleBackdropHi(w http.ResponseWriter, r *http.Request) {
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
	cacheFile := filepath.Join(s.backdropCacheDir(), strconv.FormatInt(id, 10)+".jpg")
	if fi, err := os.Stat(cacheFile); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
		serveFile(w, r, cacheFile)
		return
	}
	if (it.Type == "movie" || it.Type == "series") && it.TmdbID > 0 {
		if cfg, cerr := s.tmdbCfg(); cerr == nil && cfg.available() {
			kind := "movie"
			if it.Type == "series" {
				kind = "tv"
			}
			if fp, ferr := fetchTMDBBackdrop(cfg, kind, it.TmdbID); ferr == nil && fp != "" {
				if data, derr := cfg.downloadBackdrop(fp); derr == nil && len(data) > 0 {
					_ = os.MkdirAll(s.backdropCacheDir(), 0o755)
					tmp := cacheFile + ".tmp"
					if werr := os.WriteFile(tmp, data, 0o644); werr == nil {
						_ = os.Rename(tmp, cacheFile)
						serveFile(w, r, cacheFile)
						return
					}
				}
			}
		}
	}
	// Fallback: whatever local backdrop the scanner registered.
	s.serveLocalImage(w, r, id, "backdrop")
}

func (s *Server) handleScanStart(w http.ResponseWriter, r *http.Request) {
	// Scans all libraries. The optional {"root"} only seeds libraries
	// when the library table is still empty (first-run compatibility).
	root := s.defaultRoot
	var body struct {
		Root string `json:"root"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Root != "" {
			root = body.Root
		}
	}
	if err := s.scanner.StartAll(root); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, s.scanner.Status())
}

func (s *Server) handleScanStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.scanner.Status())
}

// json404Prefixes are the API surfaces whose unknown sub-paths must
// answer a JSON 404 instead of falling through to the SPA page — an
// HTML answer is what made Nan report `invalid character '<'` when it
// asked for a not-yet-implemented /emby/* path.
var json404Prefixes = []string{
	"/api/", "/emby/", "/System/", "/Users/", "/Library/",
	"/Items/", "/Videos/", "/Sessions/", "/Shows/", "/Search/",
}

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	for _, p := range json404Prefixes {
		if strings.HasPrefix(r.URL.Path, p) {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(web.IndexHTML)
}
