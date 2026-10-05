package module

import (
	"bytes"
	"context"
	"git.bnema.dev/bnema/neferbar/internal/racecheck"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func newTest() (*Module, chan struct{}) {
	wake := make(chan struct{}, 1)
	return New("t", Left, "true", wake, slog.New(slog.DiscardHandler)), wake
}

func TestReadFramesSplitsOnNewlineAndFormFeed(t *testing.T) {
	m, _ := newTest()
	m.readFrames(strings.NewReader("one\ntwo\fthree"))
	// "three" has no terminator yet: the latest complete frame is "two".
	got, changed, _ := m.Take(nil)
	if !changed || string(got) != "two" {
		t.Fatalf("got %q changed=%v, want latest complete frame %q", got, changed, "two")
	}
	if _, changed, _ = m.Take(got); changed {
		t.Fatal("Take must clear the dirty flag")
	}
}

func TestFrameLongerThanMaxIsTruncated(t *testing.T) {
	m, _ := newTest()
	m.readFrames(strings.NewReader(strings.Repeat("x", MaxFrame*3) + "\n"))
	got, _, _ := m.Take(nil)
	if len(got) != MaxFrame {
		t.Fatalf("len = %d, want %d", len(got), MaxFrame)
	}
}

func TestPublishWakesWithoutBlocking(t *testing.T) {
	m, wake := newTest()
	for i := 0; i < 1000; i++ { // nobody drains wake: must never block
		m.publish([]byte("x"))
	}
	if len(wake) != 1 {
		t.Fatalf("wake queue = %d, want 1", len(wake))
	}
}

func TestRunStreamsAndRestarts(t *testing.T) {
	wake := make(chan struct{}, 1)
	m := New("t", Left, "printf 'hello\\n'", wake, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go m.Run(ctx)
	var seen int
	deadline := time.After(4 * time.Second)
	for seen < 2 { // the script exits at once; it must be restarted
		select {
		case <-wake:
			if b, ch, failed := m.Take(nil); ch && !failed && bytes.Equal(b, []byte("hello")) {
				seen++
			}
		case <-deadline:
			t.Fatalf("saw %d frames, want 2 (restart)", seen)
		}
	}
}

func TestPublishAllocs(t *testing.T) {
	if racecheck.Enabled {
		t.Skip("the race detector allocates")
	}
	m, _ := newTest()
	frame := []byte("\x1b[31mred\x1b[0m text")
	dst := make([]byte, 0, MaxFrame)
	if got := testing.AllocsPerRun(500, func() {
		m.publish(frame)
		dst, _, _ = m.Take(dst)
	}); got > 0 {
		t.Errorf("publish+Take allocates %.1f objects; want 0", got)
	}
}
