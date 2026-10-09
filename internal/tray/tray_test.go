package tray

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"slices"
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

	// calls records the StatusNotifierItem actions it received, as
	// "Method(args)". The fields below are set through run.
	calls   chan string
	unknown string        // the method it answers with UnknownMethod
	slow    string        // the method it answers only after slowFor
	slowFor time.Duration // how long slow takes
}

func newFakeItem(t *testing.T, addr, id, iconName string, pixel [4]byte) *fakeItem {
	c := dial(t, addr)
	f := &fakeItem{c: c, name: c.UniqueName(), driver: dial(t, addr), cmds: make(chan func(), 1),
		cur: [3]string{id, iconName, "Active"}, pixel: pixel, closed: make(chan struct{}),
		calls: make(chan string, 32)}
	go f.serve()
	return f
}

// run hands fn to the item's goroutine, which runs it right after answering:
// run may return before fn ran. Tests wait for its effect on the output.
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
		if m.Type == zerobus.TypeMethodCall && m.Interface == itemIface {
			if !f.action(m) {
				return
			}
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

// action records and answers one StatusNotifierItem method call. It reports
// false when the connection is gone.
func (f *fakeItem) action(m *zerobus.Message) bool {
	r := m.Body()
	var rec string
	switch m.Member {
	case "Activate", "SecondaryActivate", "ContextMenu":
		x, y := r.Int32(), r.Int32()
		rec = fmt.Sprintf("%s(%d,%d)", m.Member, x, y)
	case "Scroll":
		delta, orientation := r.Int32(), r.Str()
		rec = fmt.Sprintf("Scroll(%d,%s)", delta, orientation)
	default:
		return true
	}
	f.calls <- rec
	if m.Member == f.slow {
		time.Sleep(f.slowFor)
	}
	if m.Member == f.unknown {
		f.c.NewError(m, "org.freedesktop.DBus.Error.UnknownMethod", "s").Str("no such method")
	} else {
		f.c.NewReply(m, "")
	}
	_, err := f.c.Send()
	return err == nil
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
	return startTrayWith(t, addr, nil, nil)
}

// startTrayWith is startTray with the bar's input and a dialer for the
// control connection.
func startTrayWith(t *testing.T, addr string, in io.Reader, dialer func() (*zerobus.Conn, error)) (*lines, context.CancelFunc, <-chan error) {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var result error
	stopped := make(chan struct{})
	opt := Options{
		Resolver:   NewResolver(lookup, nil, []string{t.TempDir()}),
		Foreground: RGB{200, 200, 200}, Background: RGB{30, 30, 46}, Accent: RGB{137, 180, 250},
		Out: pw, In: in, Dial: dialer,
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

// waitWatcher waits until the tray owns the watcher name.
func waitWatcher(t *testing.T, addr string) {
	t.Helper()
	probe := dial(t, addr)
	deadline := time.Now().Add(5 * time.Second)
	for {
		probe.NewCall(busName, busPath, busName, "NameHasOwner", "s").Str(watcherName)
		m, err := probe.Call()
		if err != nil {
			t.Fatal(err)
		}
		if m.Body().Bool() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the tray did not become the watcher")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fakeWatcher is another panel's StatusNotifierWatcher. Like fakeItem, its
// connection is used only by its own goroutine, which the test drives
// through Ping.
type fakeWatcher struct {
	c      *zerobus.Conn
	probe  *zerobus.Conn
	items  []string
	cmds   chan func()
	closed chan struct{}
}

func newFakeWatcher(t *testing.T, addr string, flags uint32, items ...string) *fakeWatcher {
	t.Helper()
	w := &fakeWatcher{c: dial(t, addr), probe: dial(t, addr), items: items, cmds: make(chan func(), 1), closed: make(chan struct{})}
	if code, err := w.c.RequestName(watcherName, flags|zerobus.NameFlagDoNotQueue); err != nil || code != zerobus.NamePrimaryOwner {
		t.Fatalf("RequestName = %d, %v", code, err)
	}
	go w.serve()
	return w
}

func (w *fakeWatcher) serve() {
	defer close(w.closed)
	for {
		m, err := w.c.ReadMessage()
		if err != nil {
			return
		}
		if m.Type != zerobus.TypeMethodCall {
			continue
		}
		switch m.Member {
		case "Ping":
			w.c.NewReply(m, "")
			if _, err := w.c.Send(); err != nil {
				return
			}
			(<-w.cmds)()
			continue
		case "Get":
			e := w.c.NewReply(m, "v")
			e.Variant("as")
			a := e.BeginArray('s')
			for _, s := range w.items {
				e.Str(s)
			}
			e.EndArray(a)
		default:
			w.c.NewReply(m, "")
		}
		if _, err := w.c.Send(); err != nil {
			return
		}
	}
}

// do hands fn to the watcher's goroutine; see fakeItem.run.
func (w *fakeWatcher) do(t *testing.T, fn func()) {
	t.Helper()
	w.cmds <- fn
	w.probe.NewCall(w.c.UniqueName(), "/", "org.freedesktop.DBus.Peer", "Ping", "")
	if _, err := w.probe.Call(); err != nil {
		t.Fatal(err)
	}
}

func (w *fakeWatcher) emit(t *testing.T, member, service string) {
	w.do(t, func() {
		w.c.NewSignal(watcherPath, watcherIface, member, "s").Str(service)
		_, _ = w.c.Send()
	})
}

func (w *fakeWatcher) quit(t *testing.T) {
	w.do(t, func() { _ = w.c.Close() })
	<-w.closed
}

func TestTrayAsHost(t *testing.T) {
	addr := privateBus(t)
	obsidian := newFakeItem(t, addr, "obsidian_status_icon_1", "", [4]byte{255, 120, 80, 220})
	// Another program is the watcher, and lists one item.
	w := newFakeWatcher(t, addr, 0, obsidian.name+itemPath)

	out, _, _ := startTray(t, addr)
	out.waitFor(t, "the listed item", func(s string) bool { return strings.Contains(s, "O") })

	// A second item is announced by the other watcher, by bus name only.
	steam := newFakeItem(t, addr, "steam", "", [4]byte{255, 30, 60, 200})
	w.emit(t, "StatusNotifierItemRegistered", steam.name)
	out.waitFor(t, "both items", func(s string) bool { return strings.Contains(s, "O") && strings.Contains(s, "S") })

	// And removed in the same form.
	w.emit(t, "StatusNotifierItemUnregistered", steam.name)
	out.waitFor(t, "steam to go away", func(s string) bool { return strings.Contains(s, "O") && !strings.Contains(s, "S") })
}

func TestHostFollowsReplacedWatcher(t *testing.T) {
	addr := privateBus(t)
	obsidian := newFakeItem(t, addr, "obsidian_status_icon_1", "", [4]byte{255, 120, 80, 220})
	newFakeWatcher(t, addr, zerobus.NameFlagAllowReplacement, obsidian.name+itemPath)
	out, _, _ := startTray(t, addr)
	out.waitFor(t, "the first watcher's item", func(s string) bool { return strings.Contains(s, "O") })

	// A new panel replaces the watcher: its items are the ones to show.
	steam := newFakeItem(t, addr, "steam", "", [4]byte{255, 30, 60, 200})
	newFakeWatcher(t, addr, zerobus.NameFlagReplaceExisting, steam.name+itemPath)
	out.waitFor(t, "the new watcher's item only", func(s string) bool { return strings.Contains(s, "S") && !strings.Contains(s, "O") })
}

func TestHostTakesOverWatcher(t *testing.T) {
	addr := privateBus(t)
	obsidian := newFakeItem(t, addr, "obsidian_status_icon_1", "", [4]byte{255, 120, 80, 220})
	w := newFakeWatcher(t, addr, 0, obsidian.name+itemPath)
	out, cancel, done := startTray(t, addr)
	out.waitFor(t, "the listed item", func(s string) bool { return strings.Contains(s, "O") })

	// The other panel exits: the tray drops its items and becomes the
	// watcher, where items register again.
	w.quit(t)
	out.waitFor(t, "the items to go", func(s string) bool { return s == "" })
	waitWatcher(t, addr)
	obsidian.register(t)
	out.waitFor(t, "the item registered again", func(s string) bool { return strings.Contains(s, "O") })

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// registered returns the watcher's RegisteredStatusNotifierItems.
func registered(t *testing.T, c *zerobus.Conn) []string {
	t.Helper()
	e := c.NewCall(watcherName, watcherPath, propsIface, "Get", "ss")
	e.Str(watcherIface)
	e.Str("RegisteredStatusNotifierItems")
	m, err := c.Call()
	if err != nil {
		t.Fatal(err)
	}
	r := m.Body()
	if v := r.Variant(); v != "as" {
		t.Fatalf("variant %q", v)
	}
	var items []string
	for end := r.Array('s'); r.More(end); {
		items = append(items, strings.Clone(r.Str()))
	}
	return items
}

func TestWatcherRegistration(t *testing.T) {
	addr := privateBus(t)
	out, _, _ := startTray(t, addr)
	waitWatcher(t, addr)

	// libappindicator and Electron register an object path, not a name.
	steam := newFakeItem(t, addr, "steam", "", [4]byte{255, 30, 60, 200})
	steam.run(t, func() {
		steam.c.NewCall(watcherName, watcherPath, watcherIface, "RegisterStatusNotifierItem", "s").Str(itemPath)
		_, _ = steam.c.Send()
	})
	out.waitFor(t, "the item registered by path", func(s string) bool { return strings.Contains(s, "S") })

	// The same item again, by bus name: still one item.
	dup := make(chan error, 1)
	steam.run(t, func() {
		steam.c.NewCall(watcherName, watcherPath, watcherIface, "RegisterStatusNotifierItem", "s").Str(steam.name)
		_, err := steam.c.Call()
		dup <- err
	})
	if err := <-dup; err != nil {
		t.Fatal(err)
	}

	// A path D-Bus refuses is rejected and leaves no trace.
	probe := dial(t, addr)
	for _, bad := range []string{"/bad//path", "not a name", "1.starts.with.digit"} {
		probe.NewCall(watcherName, watcherPath, watcherIface, "RegisterStatusNotifierItem", "s").Str(bad)
		if _, err := probe.Call(); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
	if items := registered(t, probe); len(items) != 1 || items[0] != steam.name+itemPath {
		t.Fatalf("registered items %q", items)
	}
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
	tr.add(app.name, "")
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

func TestRenderPublishesTargets(t *testing.T) {
	var buf strings.Builder
	tr := &Tray{opt: Options{Out: &buf, Foreground: RGB{1, 2, 3}}}
	tr.items = []*item{
		{loaded: true, icon: "S", dest: ":1.1", owner: ":1.1", path: "/a", menu: "/ma"},
		{loaded: true, icon: "Z", status: passive, dest: ":1.9", path: "/z"},
		{loaded: true, icon: "☀", dest: "org.x.Sun", owner: ":1.2", path: "/b", isMenu: true},
	}
	if err := tr.render(false); err != nil {
		t.Fatal(err)
	}
	want := []Target{
		{Dest: ":1.1", Path: "/a", Menu: "/ma", Start: 0, Width: 1},
		{Dest: ":1.2", Path: "/b", IsMenu: true, Start: 2, Width: 1},
	}
	if got := tr.snap.targets; !slices.Equal(got, want) {
		t.Fatalf("targets = %+v, want %+v", got, want)
	}
	for col, wantPath := range map[int]string{0: "/a", 2: "/b"} {
		if tg, ok := tr.target(col); !ok || tg.Path != wantPath {
			t.Errorf("target(%d) = %+v, %v; want %s", col, tg, ok, wantPath)
		}
	}
	for _, col := range []int{-1, 1, 3} {
		if _, ok := tr.target(col); ok {
			t.Errorf("target(%d) found an icon in the gap or past the line", col)
		}
	}
	// A hidden item leaves no target, and a wide icon moves the next one.
	tr.items[0].icon = "界"
	if err := tr.render(false); err != nil {
		t.Fatal(err)
	}
	if got := tr.snap.targets; len(got) != 2 || got[0].Width != 2 || got[1].Start != 3 {
		t.Fatalf("targets after a wide icon = %+v", got)
	}
}

// A change of menu path alone republishes the targets without a new line.
func TestRenderRepublishesTargetsWithoutNewLine(t *testing.T) {
	var buf strings.Builder
	tr := &Tray{opt: Options{Out: &buf}}
	tr.items = []*item{{loaded: true, icon: "S", dest: ":1.1", path: "/a"}}
	for range 2 {
		if err := tr.render(false); err != nil {
			t.Fatal(err)
		}
	}
	tr.items[0].menu = "/menu"
	if err := tr.render(false); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), "\n"); n != 1 {
		t.Fatalf("%d lines written, want 1", n)
	}
	if tg, _ := tr.target(0); tg.Menu != "/menu" {
		t.Fatalf("target = %+v", tg)
	}
}
