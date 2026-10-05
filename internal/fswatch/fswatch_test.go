package fswatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func waitSignal(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

// start runs WatchDir and returns once the watch is surely in place.
func start(t *testing.T, dir string) <-chan struct{} {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := make(chan struct{}, 1)
	go func() { _ = WatchDir(ctx, dir, 30*time.Millisecond, ch) }()
	time.Sleep(150 * time.Millisecond)
	return ch
}

func TestWatchDirReportsAnyFileOfTheDirectory(t *testing.T) {
	dir := t.TempDir()
	ch := start(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "lib.sh"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitSignal(ch, 2*time.Second) {
		t.Fatal("a new file in the directory was not reported")
	}
	// An edit of an existing file, and a replacement by rename, are reported too.
	if err := os.WriteFile(filepath.Join(dir, "lib.sh"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitSignal(ch, 2*time.Second) {
		t.Fatal("an edit was not reported")
	}
	tmp := filepath.Join(dir, "lib.sh.new")
	_ = os.WriteFile(tmp, []byte("z"), 0o644)
	waitSignal(ch, 2*time.Second) // the .new file itself
	if err := os.Rename(tmp, filepath.Join(dir, "lib.sh")); err != nil {
		t.Fatal(err)
	}
	if !waitSignal(ch, 2*time.Second) {
		t.Fatal("a rename over a file was not reported")
	}
}

func TestWatchDirIgnoresHiddenAndBackupFiles(t *testing.T) {
	dir := t.TempDir()
	ch := start(t, dir)
	for _, name := range []string{".left.sh.swp", "left.sh~"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if waitSignal(ch, 400*time.Millisecond) {
		t.Fatal("a swap or backup file was reported")
	}
}

func TestWatchDirWaitsForAMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "later")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := make(chan struct{}, 1)
	go func() { _ = WatchDir(ctx, dir, 30*time.Millisecond, ch) }()
	time.Sleep(100 * time.Millisecond)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond) // the missing directory is looked for every 2 s
	if err := os.WriteFile(filepath.Join(dir, "a.sh"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitSignal(ch, 2*time.Second) {
		t.Fatal("a directory that appeared later was not watched")
	}
}
