package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"yingku/internal/scan"
	"yingku/internal/store"
)

// Batch 26: MediaCoverGenerator talks to yingku through two Emby
// endpoints — VirtualFolders/Query to enumerate libraries and
// POST /Items/{libraryId}/Images/Primary to publish a rendered
// cover. The uploaded cover must then be what the library image
// endpoint serves, and the DTO tag must advertise it.
func TestLibraryCoverUploadRoundtrip(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	libID, err := st.CreateLibrary("电影库", "movie", []string{fixturesAbs(t)})
	if err != nil {
		t.Fatal(err)
	}
	h := New(st, scan.New(st), "")
	embyID := embyLibraryID(libID)

	// VirtualFolders/Query lists the library in MCG's expected shape.
	code, body := doReq(t, h, "GET", "/emby/Library/VirtualFolders/Query", "")
	if code != http.StatusOK {
		t.Fatalf("VirtualFolders/Query: %d %s", code, body)
	}
	var vf struct {
		Items []struct {
			Id             string
			Name           string
			CollectionType string
		}
	}
	if err := json.Unmarshal(body, &vf); err != nil || len(vf.Items) != 1 {
		t.Fatalf("VirtualFolders/Query shape: %v %s", err, body)
	}
	if vf.Items[0].Id != embyID || vf.Items[0].Name != "电影库" || vf.Items[0].CollectionType != "movies" {
		t.Fatalf("VirtualFolders/Query item: %+v", vf.Items[0])
	}

	// Upload a cover: base64 body of a 1x1 PNG, like MCG posts.
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3, 4}
	enc := base64.StdEncoding.EncodeToString(png)
	code, body = doReq(t, h, "POST", "/emby/Items/"+embyID+"/Images/Primary", enc)
	if code != http.StatusNoContent {
		t.Fatalf("upload: %d %s", code, body)
	}

	// The library image endpoint now serves the uploaded bytes.
	code, body = doReq(t, h, "GET", "/emby/Items/"+embyID+"/Images/Primary", "")
	if code != http.StatusOK || string(body) != string(png) {
		t.Fatalf("serve uploaded cover: %d (%d bytes)", code, len(body))
	}

	// MediaFolders DTO advertises the upload signature in its tag.
	code, body = doReq(t, h, "GET", "/emby/Library/MediaFolders", "")
	if code != http.StatusOK || !strings.Contains(string(body), "-up") {
		t.Fatalf("MediaFolders tag: %d %s", code, body)
	}

	// A non-library id is rejected.
	code, _ = doReq(t, h, "POST", "/emby/Items/123/Images/Primary", enc)
	if code != http.StatusNotFound {
		t.Fatalf("upload to item id: %d", code)
	}
	// Garbage body is rejected.
	code, _ = doReq(t, h, "POST", "/emby/Items/"+embyID+"/Images/Primary", "!!!not-base64!!!")
	if code != http.StatusBadRequest {
		t.Fatalf("upload garbage: %d", code)
	}
}
