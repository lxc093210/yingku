package scan

import (
	"testing"

	"github.com/fsnotify/fsnotify"
)

func TestRelevantEvent(t *testing.T) {
	cases := []struct {
		name  string
		op    fsnotify.Op
		path  string
		isDir bool
		want  bool
	}{
		{"create strm", fsnotify.Create, "/lib/片名/片名.strm", false, true},
		{"remove dir", fsnotify.Remove, "/lib/片名", true, true},
		{"rename file", fsnotify.Rename, "/lib/片名/片名.strm", false, true},
		{"write nfo", fsnotify.Write, "/lib/片名/片名.nfo", false, true},
		{"write poster", fsnotify.Write, "/lib/片名/poster.jpg", false, true},
		{"write unrelated", fsnotify.Write, "/lib/片名/notes.txt", false, false},
		{"chmod strm", fsnotify.Chmod, "/lib/片名/片名.strm", false, false},
		{"create any file", fsnotify.Create, "/lib/片名/whatever.bin", false, true},
	}
	for _, c := range cases {
		ev := fsnotify.Event{Name: c.path, Op: c.op}
		if got := relevantEvent(ev, c.isDir); got != c.want {
			t.Errorf("%s: relevantEvent = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestLibraryForLongestPrefix(t *testing.T) {
	w := NewWatcher(nil, nil)
	w.roots = map[string]int64{
		"/media/na":        1,
		"/media/na/已整理":    2,
		"/media/115/电影":    3,
	}
	if id, ok := w.libraryFor("/media/na/已整理/电影/x/x.strm"); !ok || id != 2 {
		t.Errorf("nested root: got %d,%v want 2,true", id, ok)
	}
	if id, ok := w.libraryFor("/media/na/其他/x.strm"); !ok || id != 1 {
		t.Errorf("outer root: got %d,%v want 1,true", id, ok)
	}
	if _, ok := w.libraryFor("/elsewhere/x.strm"); ok {
		t.Errorf("outside roots: got ok, want false")
	}
}
