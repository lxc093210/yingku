// yingku is a self-hosted, Emby-protocol-oriented personal media server.
// Phase 1: library scanning (NFO + NanShare MediaInfo sidecars), SQLite
// storage and a web browse UI. Playback resolution lives in NanShare, which
// fronts this server; there is deliberately no playback endpoint here.
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"yingku/internal/api"
	"yingku/internal/scan"
	"yingku/internal/store"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	addr := envOr("YINGKU_ADDR", ":8097")
	dataPath := envOr("YINGKU_DATA", "./data/yingku.db")
	library := os.Getenv("YINGKU_LIBRARY")

	st, err := store.Open(dataPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	scanner := scan.New(st)
	handler := api.New(st, scanner, library)

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("yingku listening on %s (db: %s, library: %q)", addr, dataPath, library)
	log.Fatal(srv.ListenAndServe())
}
