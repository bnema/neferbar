package config

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func startWatch(t *testing.T, path string, last Config) <-chan Config {
	t.Helper()
	out := make(chan Config, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := Watch(ctx, path, last, out, slog.New(slog.DiscardHandler)); err != nil {
			t.Error(err)
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	time.Sleep(150 * time.Millisecond) // let the watch attach
	return out
}

func recv(t *testing.T, ch <-chan Config) Config {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("no config reload")
		return Config{}
	}
}

func none(t *testing.T, ch <-chan Config) {
	t.Helper()
	select {
	case c := <-ch:
		t.Fatalf("unexpected reload: %+v", c.Bar)
	case <-time.After(600 * time.Millisecond):
	}
}

func TestWatchInPlaceAndAtomicSaves(t *testing.T) {
	path := write(t, "[bar]\nsize = 14\n")
	first, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	ch := startWatch(t, path, first)

	if err := os.WriteFile(path, []byte("[bar]\nsize = 18\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, ch).Bar.Size; got != 18 {
		t.Fatalf("in-place save: size = %v", got)
	}

	tmp := path + ".tmp" // what most editors do: write a sibling, rename over
	if err := os.WriteFile(tmp, []byte("[bar]\nsize = 20\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, ch).Bar.Size; got != 20 {
		t.Fatalf("atomic save: size = %v", got)
	}
}

func TestWatchKeepsOldConfigOnErrors(t *testing.T) {
	path := write(t, "[bar]\nsize = 14\n")
	first, _ := Load(path, true)
	ch := startWatch(t, path, first)

	os.WriteFile(path, []byte("[bar\n"), 0o600) // syntax error
	none(t, ch)
	os.WriteFile(path, []byte("[bar]\nsize = 1\n"), 0o600) // out of range
	none(t, ch)
	os.Remove(path)
	none(t, ch)

	os.WriteFile(path, []byte("[bar]\nsize = 16\n"), 0o600) // recovers
	if got := recv(t, ch).Bar.Size; got != 16 {
		t.Fatalf("after recovery: size = %v", got)
	}
}

func TestWatchIgnoresUnchangedSave(t *testing.T) {
	path := write(t, "[bar]\nsize = 14\n")
	first, _ := Load(path, true)
	ch := startWatch(t, path, first)
	os.WriteFile(path, []byte("# a comment\n[bar]\nsize = 14\n"), 0o600)
	none(t, ch)
}

func TestWatchFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "config.toml")
	os.MkdirAll(filepath.Dir(real), 0o700)
	os.WriteFile(real, []byte("[bar]\nsize = 14\n"), 0o600)
	link := filepath.Join(dir, "config.toml")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	first, _ := Load(link, true)
	ch := startWatch(t, link, first)
	os.WriteFile(real, []byte("[bar]\nsize = 22\n"), 0o600)
	if got := recv(t, ch).Bar.Size; got != 22 {
		t.Fatalf("symlink target edit: size = %v", got)
	}
}

func TestDuplicateModuleNames(t *testing.T) {
	_, err := Load(write(t, "[[module]]\nname=\"a\"\nzone=\"left\"\nexec=\"x\"\n[[module]]\nname=\"a\"\nzone=\"right\"\nexec=\"y\"\n"), true)
	if err == nil {
		t.Fatal("duplicate module names must fail")
	}
}
