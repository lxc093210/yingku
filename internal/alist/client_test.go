package alist

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeAlist serves the three endpoints the client uses. Tokens are
// "tok-N" where N counts logins; failFirstList makes the first list
// call answer 401 so the re-login path is exercised.
type fakeAlist struct {
	logins       atomic.Int32
	lists        atomic.Int32
	failFirst401 bool
}

func (f *fakeAlist) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Username != "dav" || body.Password != "pw" {
			writeEnv(w, 401, "bad credentials", nil)
			return
		}
		n := f.logins.Add(1)
		writeEnv(w, 200, "success", map[string]any{"token": fmt.Sprintf("tok-%d", n)})
	})
	authed := func(w http.ResponseWriter, r *http.Request) bool {
		want := fmt.Sprintf("tok-%d", f.logins.Load())
		if r.Header.Get("Authorization") != want {
			writeEnv(w, 401, "token expired", nil)
			return false
		}
		return true
	}
	mux.HandleFunc("/api/fs/list", func(w http.ResponseWriter, r *http.Request) {
		if f.failFirst401 && f.lists.Add(1) == 1 {
			writeEnv(w, 401, "token expired", nil)
			return
		}
		if !authed(w, r) {
			return
		}
		var body struct {
			Path    string `json:"path"`
			Page    int    `json:"page"`
			PerPage int    `json:"per_page"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		// 5 entries total, served in pages of the requested size.
		all := []map[string]any{}
		for i := 1; i <= 5; i++ {
			all = append(all, map[string]any{
				"name": fmt.Sprintf("f%d", i), "is_dir": i%2 == 1, "size": int64(i * 100),
			})
		}
		start := (body.Page - 1) * body.PerPage
		if start > len(all) {
			start = len(all)
		}
		end := start + body.PerPage
		if end > len(all) {
			end = len(all)
		}
		writeEnv(w, 200, "success", map[string]any{"total": len(all), "content": all[start:end]})
	})
	mux.HandleFunc("/api/fs/get", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		var body struct {
			Path string `json:"path"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeEnv(w, 200, "success", map[string]any{
			"raw_url": "https://cdn.example" + body.Path + "?sign=x",
		})
	})
	return mux
}

func writeEnv(w http.ResponseWriter, code int, msg string, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": msg, "data": data})
}

func TestClientLoginListPaginationGet(t *testing.T) {
	f := &fakeAlist{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := New(srv.URL, "dav", "pw")

	entries, err := c.List("/电影/4K REMUX")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("entries = %d, want 5 (paginated)", len(entries))
	}
	if entries[0].Name != "f1" || !entries[0].IsDir || entries[0].Size != 100 {
		t.Errorf("entry0 = %+v", entries[0])
	}
	if entries[1].IsDir {
		t.Errorf("entry1 should be a file: %+v", entries[1])
	}
	if got := f.logins.Load(); got != 1 {
		t.Errorf("logins = %d, want 1 (token cached)", got)
	}

	raw, err := c.GetRawURL("/电影/4K REMUX/x/x.mkv")
	if err != nil {
		t.Fatalf("GetRawURL: %v", err)
	}
	if !strings.HasPrefix(raw, "https://cdn.example/") || !strings.Contains(raw, "x.mkv") {
		t.Errorf("raw url = %q", raw)
	}
	if got := f.logins.Load(); got != 1 {
		t.Errorf("logins after get = %d, want still 1", got)
	}
}

func TestClientReloginOn401(t *testing.T) {
	f := &fakeAlist{failFirst401: true}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := New(srv.URL, "dav", "pw")
	if _, err := c.List("/x"); err != nil {
		t.Fatalf("List with one 401: %v", err)
	}
	if got := f.logins.Load(); got != 2 {
		t.Errorf("logins = %d, want 2 (initial + one relogin)", got)
	}
}

func TestClientBadPassword(t *testing.T) {
	f := &fakeAlist{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := New(srv.URL, "dav", "wrong")
	if _, err := c.List("/x"); err == nil {
		t.Fatal("List with bad password should fail")
	}
}
