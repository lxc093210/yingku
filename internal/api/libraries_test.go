package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"yingku/internal/scan"
	"yingku/internal/store"
)

func fixturesAbs(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "fixtures", "library"))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// bareHandler builds a handler over an empty store (no pre-scan), with the
// fs browser confined to the fixtures tree.
func bareHandler(t *testing.T) http.Handler {
	t.Helper()
	t.Setenv("YINGKU_FS_ROOT", fixturesAbs(t))
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, scan.New(st), "")
}

func doReq(t *testing.T, h http.Handler, method, path, body string) (int, []byte) {
	t.Helper()
	var rd *bytes.Reader
	if body != "" {
		rd = bytes.NewReader([]byte(body))
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func TestLibraryEndpoints(t *testing.T) {
	h := bareHandler(t)
	root := fixturesAbs(t)
	movieRoot := filepath.Join(root, "电影", "动画电影")
	seriesRoot := filepath.Join(root, "电视剧", "国产剧")

	// Create with two root paths.
	code, body := doReq(t, h, http.MethodPost, "/api/libraries",
		`{"Name":"我的库","Kind":"mixed","Paths":["`+movieRoot+`","`+seriesRoot+`"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	var lib libraryDTO
	if err := json.Unmarshal(body, &lib); err != nil {
		t.Fatal(err)
	}
	if lib.Name != "我的库" || lib.Kind != "mixed" || len(lib.Paths) != 2 {
		t.Fatalf("created = %+v", lib)
	}
	id := itoa(lib.ID)

	// Duplicate path conflicts.
	if code, body := doReq(t, h, http.MethodPost, "/api/libraries",
		`{"Name":"冲突","Kind":"movie","Paths":["`+movieRoot+`"]}`); code != http.StatusConflict {
		t.Fatalf("duplicate path create = %d %s, want 409", code, body)
	}
	// Relative path rejected.
	if code, _ := doReq(t, h, http.MethodPost, "/api/libraries",
		`{"Name":"坏","Kind":"movie","Paths":["relative/dir"]}`); code != http.StatusBadRequest {
		t.Fatalf("relative path create = %d, want 400", code)
	}
	// Missing name rejected.
	if code, _ := doReq(t, h, http.MethodPost, "/api/libraries",
		`{"Kind":"movie","Paths":["`+movieRoot+`"]}`); code != http.StatusBadRequest {
		t.Fatalf("nameless create = %d, want 400", code)
	}

	// List + get.
	var libs []libraryDTO
	if code := get(t, h, "/api/libraries", &libs); code != 200 || len(libs) != 1 {
		t.Fatalf("list = %d %+v", code, libs)
	}
	var got libraryDTO
	if code := get(t, h, "/api/libraries/"+id, &got); code != 200 || got.Name != "我的库" {
		t.Fatalf("get = %d %+v", code, got)
	}

	// Update: rename, narrow to the movie root only.
	code, body = doReq(t, h, http.MethodPut, "/api/libraries/"+id,
		`{"Name":"改名","Kind":"movie","Paths":["`+movieRoot+`"]}`)
	if code != 200 {
		t.Fatalf("update = %d %s", code, body)
	}
	if err := json.Unmarshal(body, &lib); err != nil || lib.Name != "改名" || len(lib.Paths) != 1 {
		t.Fatalf("updated = %+v err %v", lib, err)
	}
	if code, _ := doReq(t, h, http.MethodPut, "/api/libraries/9999",
		`{"Name":"x","Kind":"movie","Paths":["/nonexistent"]}`); code != http.StatusNotFound {
		t.Fatalf("update missing = %d, want 404", code)
	}

	// Per-library scan runs to completion and fills the library.
	code, _ = doReq(t, h, http.MethodPost, "/api/libraries/"+id+"/scan", "")
	if code != http.StatusAccepted {
		t.Fatalf("library scan = %d, want 202", code)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var status scan.Status
		if code := get(t, h, "/api/scan/status", &status); code != 200 {
			t.Fatalf("status = %d", code)
		}
		if status.State == "done" {
			if status.LibraryID != lib.ID {
				t.Errorf("status libraryId = %d, want %d", status.LibraryID, lib.ID)
			}
			break
		}
		if status.State == "error" {
			t.Fatalf("scan error: %s", status.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("scan did not finish, state = %q", status.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var page struct {
		Items            []itemDTO `json:"Items"`
		TotalRecordCount int       `json:"TotalRecordCount"`
	}
	if code := get(t, h, "/api/libraries/"+id+"/items", &page); code != 200 || len(page.Items) != 1 {
		t.Fatalf("items after scan = %d %+v", code, page)
	}

	// Delete removes the library and its items.
	if code, _ := doReq(t, h, http.MethodDelete, "/api/libraries/"+id, ""); code != 200 {
		t.Fatalf("delete = %d", code)
	}
	if code := get(t, h, "/api/libraries/"+id, nil); code != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", code)
	}
	if code := get(t, h, "/api/libraries/"+id+"/items", nil); code != http.StatusNotFound {
		t.Fatalf("items after delete = %d, want 404", code)
	}
}

func TestFSList(t *testing.T) {
	h := bareHandler(t)
	root := fixturesAbs(t)

	var out fsListDTO
	if code := get(t, h, "/api/fs/list", &out); code != 200 {
		t.Fatalf("fs list = %d", code)
	}
	if out.Path != root || out.Parent != "" {
		t.Fatalf("fs root = %+v", out)
	}
	names := map[string]bool{}
	for _, d := range out.Dirs {
		names[d.Name] = true
		if !filepath.IsAbs(d.Path) {
			t.Errorf("dir path not absolute: %q", d.Path)
		}
	}
	if len(out.Dirs) != 2 || !names["电影"] || !names["电视剧"] {
		t.Fatalf("fs dirs = %+v", out.Dirs)
	}

	var sub fsListDTO
	q := "/api/fs/list?path=" + queryEscape(filepath.Join(root, "电影"))
	if code := get(t, h, q, &sub); code != 200 {
		t.Fatalf("fs sublist = %d", code)
	}
	if len(sub.Dirs) != 1 || sub.Dirs[0].Name != "动画电影" || sub.Parent != root {
		t.Fatalf("fs sub = %+v", sub)
	}

	// Files are never listed: the movie folder contains files and no dirs.
	var leaf fsListDTO
	q = "/api/fs/list?path=" + queryEscape(filepath.Join(root, "电影", "动画电影", "蜘蛛侠：平行宇宙 (2018) {tmdbid-324857}"))
	if code := get(t, h, q, &leaf); code != 200 || len(leaf.Dirs) != 0 {
		t.Fatalf("fs leaf = %d %+v", code, leaf)
	}

	// Outside the fs root -> 400; relative -> 400; missing -> 404.
	if code := get(t, h, "/api/fs/list?path="+queryEscape(string(filepath.Separator)), nil); code != http.StatusBadRequest {
		t.Errorf("fs escape = %d, want 400", code)
	}
	if code := get(t, h, "/api/fs/list?path="+queryEscape(filepath.Join(root, "..", "library2")), nil); code != http.StatusBadRequest {
		t.Errorf("fs dotdot escape = %d, want 400", code)
	}
	if code := get(t, h, "/api/fs/list?path=relative", nil); code != http.StatusBadRequest {
		t.Errorf("fs relative = %d, want 400", code)
	}
	if code := get(t, h, "/api/fs/list?path="+queryEscape(filepath.Join(root, "不存在")), nil); code != http.StatusNotFound {
		t.Errorf("fs missing = %d, want 404", code)
	}
}

func TestFSListHiddenFiltered(t *testing.T) {
	base := t.TempDir()
	for _, d := range []string{".hidden", "visible", ".git"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YINGKU_FS_ROOT", base)
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := New(st, scan.New(st), "")
	var out fsListDTO
	if code := get(t, h, "/api/fs/list", &out); code != 200 {
		t.Fatalf("fs list = %d", code)
	}
	if len(out.Dirs) != 1 || out.Dirs[0].Name != "visible" {
		t.Fatalf("dirs = %+v, want only [visible]", out.Dirs)
	}
}
