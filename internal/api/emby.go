package api

import (
	"net/http"
	"runtime"
)

// ---- Emby-protocol handshake surface ----
//
// The smallest slice of the Emby API that companion software touches
// first when registering a server: System/Info (identity/version check)
// and System/Ping (liveness). Both are served with and without the
// /emby prefix, as Emby clients and Nan's Emby helper use either base.
//
// The core read surface (MediaFolders, Items, images, PlaybackInfo,
// session reporting, by-name auth) lives in emby_items.go under these
// same /emby/* routes.

// systemInfoDTO mirrors Emby's SystemInfo shape (PascalCase fields).
type systemInfoDTO struct {
	ServerName             string `json:"ServerName"`
	ProductName            string `json:"ProductName"`
	Version                string `json:"Version"`
	Id                     string `json:"Id"`
	OperatingSystem        string `json:"OperatingSystem"`
	StartupWizardCompleted bool   `json:"StartupWizardCompleted"`
}

func embyOSName() string {
	switch runtime.GOOS {
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	case "darwin":
		return "OSX"
	default:
		return runtime.GOOS
	}
}

func (s *Server) handleSystemInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, systemInfoDTO{
		ServerName:             "影库 Yingku",
		ProductName:            "Yingku",
		Version:                serverVersion,
		Id:                     s.ensureServerID(),
		OperatingSystem:        embyOSName(),
		StartupWizardCompleted: true,
	})
}

func (s *Server) handleSystemPing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("Yingku Server Ready"))
}
