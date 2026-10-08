package tray

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zerobus"
)

// privateBus starts a dbus-daemon for the test and returns its address.
func privateBus(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("dbus-daemon not installed")
	}
	cmd := exec.Command(path, "--session", "--nofork", "--print-address")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	addr, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(addr)
}

func dial(t *testing.T, addr string) *zerobus.Conn {
	t.Helper()
	c, err := zerobus.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// fakeItem is an application with one StatusNotifierItem. Its connection is
// used only by its own goroutine, as a Conn requires: the test drives it
// through a peer that calls its Do method.
type fakeItem struct {
	c      *zerobus.Conn
	name   string
	driver *zerobus.Conn
	cmds   chan func()
	cur    [3]string // id, icon name, status
	pixel  [4]byte   // the one ARGB pixel of its icon
	closed chan struct{}
}

func newFakeItem(t *testing.T, addr, id, iconName string, pixel [4]byte) *fakeItem {
	c := dial(t, addr)
	f := &fakeItem{c: c, name: c.UniqueName(), driver: dial(t, addr), cmds: make(chan func(), 1),
		cur: [3]string{id, iconName, "Active"}, pixel: pixel, closed: make(chan struct{})}
	go f.serve()
	return f
}

// run makes the item's goroutine run fn, and waits for it.
func (f *fakeItem) run(t *testing.T, fn func()) {
	t.Helper()
	f.cmds <- fn
	f.driver.NewCall(f.name, "/", "org.example.Fake", "Do", "")
	if _, err := f.driver.Call(); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeItem) serve() {
	defer close(f.closed)
	for {
		m, err := f.c.ReadMessage()
		if err != nil {
			return
		}
		if m.Type == zerobus.TypeMethodCall && m.Member == "Do" {
			f.c.NewReply(m, "")
			if _, err := f.c.Send(); err != nil {
				return
			}
			(<-f.cmds)()
			continue
		}
		if m.Type != zerobus.TypeMethodCall || m.Member != "GetAll" {
			continue
		}
		e := f.c.NewReply(m, "a{sv}")
		a := e.BeginArray('{')
		for i, k := range [...]string{"Id", "IconName", "Status"} {
			e.Struct()
			e.Str(k)
			e.Variant("s")
			e.Str(f.cur[i])
		}
		e.Struct()
		e.Str("IconPixmap")
		e.Variant("a(iiay)")
		px := e.BeginArray('(')
		e.Struct()
		e.Int32(1)
		e.Int32(1)
		e.ByteArray(f.pixel[:])
		e.EndArray(px)
		e.EndArray(a)
		if _, err := f.c.Send(); err != nil {
			return
		}
	}
}

// register registers the item with the watcher, by bus name.
func (f *fakeItem) register(t *testing.T) {
	f.run(t, func() {
		f.c.NewCall(watcherName, watcherPath, watcherIface, "RegisterStatusNotifierItem", "s").Str(f.name)
		_, _ = f.c.Send()
	})
}

// setStatus changes the item's status and emits NewStatus.
func (f *fakeItem) setStatus(t *testing.T, status string) {
	f.run(t, func() {
		f.cur[2] = status
		f.c.NewSignal(itemPath, itemIface, "NewStatus", "s").Str(status)
		_, _ = f.c.Send()
	})
}

// quit closes the item's connection, as when the application exits.
func (f *fakeItem) quit(t *testing.T) {
	f.run(t, func() { _ = f.c.Close() })
	<-f.closed
}

// lines reads the tray's output.
type lines struct {
	ch chan string
}

func readLines(r io.Reader) *lines {
	l := &lines{ch: make(chan string, 64)}
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			l.ch <- sc.Text()
		}
		close(l.ch)
	}()
	return l
}

// waitFor returns the first line for which ok is true.
func (l *lines) waitFor(t *testing.T, what string, ok func(string) bool) string {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case s, open := <-l.ch:
			if !open {
				t.Fatalf("output ended while waiting for %s", what)
			}
			if ok(s) {
				return s
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// startTray runs a tray on the bus. Cancel it and receive from the channel
// to stop it; the test cleanup does both when the test did not.
func startTray(t *testing.T, addr string) (*lines, context.CancelFunc, <-chan error) {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var result error
	stopped := make(chan struct{})
	opt := Options{
		Resolver:   NewResolver(lookup, nil, []string{t.TempDir()}),
		Foreground: RGB{200, 200, 200}, Background: RGB{30, 30, 46}, Accent: RGB{137, 180, 250},
		Out: pw,
	}
	c := dial(t, addr)
	go func() {
		result = Run(ctx, c, opt)
		_ = pw.Close()
		close(stopped)
		done <- result
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
	return readLines(pr), cancel, done
}

func TestTrayAsWatcher(t *testing.T) {
	addr := privateBus(t)
	out, cancel, done := startTray(t, addr)
	if s := out.waitFor(t, "the empty first line", func(string) bool { return true }); s != "" {
		t.Fatalf("first line %q, want empty", s)
	}
	// Wait until the tray owns the watcher name.
	probe := dial(t, addr)
	deadline := time.Now().Add(5 * time.Second)
	for {
		probe.NewCall(busName, busPath, busName, "NameHasOwner", "s").Str(watcherName)
		m, err := probe.Call()
		if err != nil {
			t.Fatal(err)
		}
		if m.Body().Bool() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the tray did not become the watcher")
		}
		time.Sleep(10 * time.Millisecond)
	}

	steam := newFakeItem(t, addr, "steam", "", [4]byte{255, 30, 60, 200})
	steam.register(t)
	line := out.waitFor(t, "the steam glyph", func(s string) bool { return strings.Contains(s, "S") })
	if !strings.Contains(line, "38;2;") {
		t.Fatalf("the icon is not colored: %q", line)
	}

	steam.setStatus(t, "NeedsAttention")
	out.waitFor(t, "the attention style", func(s string) bool { return strings.Contains(s, "\x1b[1;38;2;137;180;250mS") })

	steam.setStatus(t, "Passive")
	out.waitFor(t, "the passive item to hide", func(s string) bool { return s == "" })

	steam.setStatus(t, "Active")
	out.waitFor(t, "the item to come back", func(s string) bool { return strings.Contains(s, "S") })

	// The application quits: its item goes away.
	steam.quit(t)
	out.waitFor(t, "the item to go away", func(s string) bool { return s == "" })

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestTrayAsHost(t *testing.T) {
	addr := privateBus(t)
	// Another program is the watcher. It lists one item and announces it.
	other := dial(t, addr)
	if code, err := other.RequestName(watcherName, zerobus.NameFlagDoNotQueue); err != nil || code != zerobus.NamePrimaryOwner {
		t.Fatalf("RequestName = %d, %v", code, err)
	}
	obsidian := newFakeItem(t, addr, "obsidian_status_icon_1", "", [4]byte{255, 120, 80, 220})
	// A Conn is used from one goroutine: the fake watcher announces new
	// items from its own loop, when the test pings it.
	announce := make(chan string, 1)
	go func() {
		for {
			m, err := other.ReadMessage()
			if err != nil {
				return
			}
			if m.Type != zerobus.TypeMethodCall {
				continue
			}
			switch m.Member {
			case "Ping":
				other.NewReply(m, "")
				if _, err := other.Send(); err != nil {
					return
				}
				other.NewSignal(watcherPath, watcherIface, "StatusNotifierItemRegistered", "s").Str(<-announce)
			case "Get":
				e := other.NewReply(m, "v")
				e.Variant("as")
				a := e.BeginArray('s')
				e.Str(obsidian.name + itemPath)
				e.EndArray(a)
			default:
				other.NewReply(m, "")
			}
			if _, err := other.Send(); err != nil {
				return
			}
		}
	}()

	out, _, _ := startTray(t, addr)
	out.waitFor(t, "the listed item", func(s string) bool { return strings.Contains(s, "O") })

	// A second item is announced by the other watcher.
	steam := newFakeItem(t, addr, "steam", "", [4]byte{255, 30, 60, 200})
	announce <- steam.name
	probe := dial(t, addr)
	probe.NewCall(other.UniqueName(), "/", "org.freedesktop.DBus.Peer", "Ping", "")
	if _, err := probe.Call(); err != nil {
		t.Fatal(err)
	}
	out.waitFor(t, "both items", func(s string) bool { return strings.Contains(s, "O") && strings.Contains(s, "S") })
}

// TestRefreshNoAllocs checks the steady state: an item signals a change, the
// tray asks for its properties and reads the reply. Nothing allocates once
// the buffers have grown.
func TestRefreshNoAllocs(t *testing.T) {
	addr := privateBus(t)
	app := newFakeItem(t, addr, "steam", "", [4]byte{255, 30, 60, 200})
	var out strings.Builder
	tr := &Tray{c: dial(t, addr), opt: Options{Resolver: NewResolver(lookup, nil, []string{t.TempDir()}), Out: &out},
		frame: make([]byte, 0, 256), last: make([]byte, 0, 256)}
	tr.opt.Log = slog.New(slog.DiscardHandler)
	tr.add(app.name, "", "")
	cycle := func() {
		tr.refresh(tr.items[0])
		for len(tr.pending) > 0 {
			m, err := tr.c.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			tr.dispatch(m)
		}
	}
	for range 3 {
		cycle() // resolve the icon and grow the buffers
	}
	if tr.err != nil || !tr.items[0].loaded {
		t.Fatalf("item not loaded: %v", tr.err)
	}
	if allocs := testing.AllocsPerRun(50, cycle); allocs != 0 {
		t.Fatalf("%v allocations per refresh, want 0", allocs)
	}
}

func TestRenderSkipsUnchangedFrames(t *testing.T) {
	var buf strings.Builder
	tr := &Tray{opt: Options{Out: &buf, Foreground: RGB{1, 2, 3}}}
	tr.items = []*item{{loaded: true, icon: "X"}}
	for range 3 {
		if err := tr.render(false); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(buf.String(), "\n"); got != 1 {
		t.Fatalf("%d frames written, want 1", got)
	}
	if allocs := testing.AllocsPerRun(100, func() { _ = tr.render(false) }); allocs != 0 {
		t.Fatalf("render allocates %v times", allocs)
	}
}

func TestBetterPixmapSize(t *testing.T) {
	// The largest icon up to 64x64 wins; above that, the smallest.
	for _, c := range []struct {
		sizes []int // sides, in the order the item lists them
		want  int
	}{
		{[]int{16, 32, 128}, 32},
		{[]int{128, 32, 16}, 32},
		{[]int{256, 128}, 128},
		{[]int{22}, 22},
	} {
		best := 0
		for _, s := range c.sizes {
			if best == 0 || better(s*s, best*best) {
				best = s
			}
		}
		if best != c.want {
			t.Errorf("%v: chose %d, want %d", c.sizes, best, c.want)
		}
	}
}
