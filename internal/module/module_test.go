package module

import (
	"bytes"
	"context"
	"github.com/bnema/neferbar/internal/racecheck"
	"log/slog"
	"os"
	"path/filepath"
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

// Stopping a module must also end the processes the script started, and Wait
// must return once they are gone.
func TestStopEndsTheScriptsChildren(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "alive")
	// A background loop in a subshell: it outlives "sh" unless the whole group is killed.
	script := "(while :; do echo x > " + marker + "; sleep 0.05; done) & wait"
	m := New("t", Left, script, make(chan struct{}, 1), slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the script never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	m.Wait(5 * time.Second)
	os.Remove(marker)
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a child of the script is still running after Wait returned")
	}
}

func TestScriptDir(t *testing.T) {
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	for command, want := range map[string]string{
		"/opt/bar/left.sh":          "/opt/bar",
		"/opt/bar/left.sh 60 30":    "/opt/bar",
		"~/.config/neferbar/a.sh":   filepath.Join(home, ".config/neferbar"),
		"examples/modules/clock.sh": filepath.Join(cwd, "examples/modules"),
		"./local.sh":                cwd,
		"date":                      "",
		"sh -c 'echo hi'":           "",
		"":                          "",
		"   ":                       "",
	} {
		if got := ScriptDir(command); got != want {
			t.Errorf("ScriptDir(%q) = %q, want %q", command, got, want)
		}
	}
}

func TestSendReachesTheScript(t *testing.T) {
	wake := make(chan struct{}, 1)
	m := New("t", Left, `while read l; do printf '%s\n' "$l"; done`, wake, slog.New(slog.DiscardHandler))
	m.Interactive = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m.Start(ctx)
	defer m.Wait(5 * time.Second)
	defer cancel()
	deadline := time.After(5 * time.Second)
	for {
		// The script may start after the first Send: resend until it answers.
		m.Send([]byte("hover 3"))
		select {
		case <-wake:
			if b, ch, failed := m.Take(nil); ch && !failed && string(b) == "hover 3" {
				return
			}
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			t.Fatal("the script never echoed the line")
		}
	}
}

func TestSendRefusals(t *testing.T) {
	m, _ := newTest()
	if m.Send([]byte("hover 1")) {
		t.Fatal("a non-interactive module must refuse Send")
	}
	m.Interactive = true
	if m.Send(make([]byte, 128)) {
		t.Fatal("a line over 127 bytes must be refused")
	}
	if !m.Send(make([]byte, 127)) {
		t.Fatal("a 127-byte line must be accepted")
	}
}

func TestSendNeverBlocks(t *testing.T) {
	m, _ := newTest()
	m.Interactive = true
	done := make(chan int)
	go func() {
		ok := 0
		for i := 0; i < 33; i++ {
			if m.Send([]byte("hover 1")) {
				ok++
			}
		}
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok != 32 {
			t.Fatalf("%d sends accepted, want 32 (the queue size)", ok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Send blocked on a full queue")
	}
}

// A script that never reads its stdin must not hold the module back.
func TestUnreadStdinDoesNotBlockStop(t *testing.T) {
	m := New("t", Left, "echo up; sleep 30", make(chan struct{}, 1), slog.New(slog.DiscardHandler))
	m.Interactive = true
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	for i := 0; i < 40; i++ {
		m.Send([]byte(strings.Repeat("x", 100)))
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	start := time.Now()
	m.Wait(5 * time.Second)
	if time.Since(start) > 4*time.Second {
		t.Fatal("stopping took too long")
	}
}

func TestSendAllocs(t *testing.T) {
	if racecheck.Enabled {
		t.Skip("the race detector allocates")
	}
	m, _ := newTest()
	m.Interactive = true
	line := []byte("hover 12")
	if got := testing.AllocsPerRun(100, func() {
		m.Send(line)
		<-m.input
	}); got > 0 {
		t.Errorf("Send allocates %.1f objects; want 0", got)
	}
}

func TestGenCountsStarts(t *testing.T) {
	wake := make(chan struct{}, 1)
	m := New("t", Left, "printf 'hi\\n'", wake, slog.New(slog.DiscardHandler))
	if m.Gen() != 0 {
		t.Fatalf("Gen before start = %d", m.Gen())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go m.Run(ctx)
	deadline := time.After(4 * time.Second)
	for m.Gen() < 2 { // the script exits at once and is restarted
		select {
		case <-wake:
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatalf("Gen = %d, want at least 2 after a restart", m.Gen())
		}
	}
	if allocs := testing.AllocsPerRun(100, func() { m.Gen() }); allocs != 0 {
		t.Errorf("Gen allocates %v", allocs)
	}
}
