package tray

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zerobus"
)

func TestParseCommand(t *testing.T) {
	good := map[string]command{
		"click left 0 1":                      {verb: "click", button: btnLeft, col: 0, token: 1},
		"click left 0 4294967295":             {verb: "click", button: btnLeft, token: 1<<32 - 1},
		"click left 1048576 1":                {verb: "click", button: btnLeft, col: 1 << 20, token: 1},
		"menu-activate 4294967295 2147483647": {verb: "menu-activate", token: 1<<32 - 1, col: 1<<31 - 1},
		"click middle 3 42":                   {verb: "click", button: btnMiddle, col: 3, token: 42},
		"click right 12 7":                    {verb: "click", button: btnRight, col: 12, token: 7},
		"scroll up 2 0":                       {verb: "scroll", vert: true, delta: -2},
		"scroll down 1 4":                     {verb: "scroll", vert: true, delta: 1, col: 4},
		"scroll left 3 1":                     {verb: "scroll", delta: -3, col: 1},
		"scroll right 5 2":                    {verb: "scroll", delta: 5, col: 2},
		"hover 4":                             {verb: "hover", col: 4},
		"leave":                               {verb: "leave"},
		"menu-activate 9 17":                  {verb: "menu-activate", token: 9, col: 17},
		"menu-closed 9":                       {verb: "menu-closed", token: 9},
		"  click  left 1  2 ":                 {verb: "click", button: btnLeft, col: 1, token: 2},
	}
	for line, want := range good {
		got, ok := parseCommand(line)
		if !ok || got != want {
			t.Errorf("parseCommand(%q) = %+v, %v; want %+v", line, got, ok, want)
		}
	}
	for _, line := range []string{
		"", "   ", "garbage", "click", "click left", "click left 0", "click left 0 1 2", "click up 0 1",
		"click left -1 1", "click left x 1", "click left 0 -3", "click left +1 1", "click left 0 4294967296",
		"scroll up", "scroll up 0 0", "scroll up -1 0", "scroll up 2", "scroll diagonal 1 0", "scroll up 1000000 0",
		"hover", "hover x", "hover -1", "hover 1 2", "leave now", "menu-activate 1", "menu-activate 1 -2",
		"menu-closed", "menu-closed x", "menu-closed 1 2", "CLICK left 0 1",
		// Signs are refused, and so is anything past 31 bits for columns and steps.
		"click left 0 +1", "click left +0 1", "click left 0 -0", "scroll up +2 0", "scroll up 2 +0", "hover +1",
		"menu-activate +1 2", "menu-activate 1 +2", "menu-closed +1", "menu-closed -1",
		"click left 2147483648 1", "scroll up 1 2147483648", "hover 2147483648", "hover 1048577",
		"menu-activate 1 2147483648", "click left 0 0x1", "click left 0 1_0", "click left 0 1e3",
	} {
		if got, ok := parseCommand(line); ok {
			t.Errorf("parseCommand(%q) = %+v, want it refused", line, got)
		}
	}
}

// barInput is the pipe the test writes the bar's lines to.
type barInput struct{ w *io.PipeWriter }

func (b barInput) send(t *testing.T, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if _, err := io.WriteString(b.w, l+"\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// setup runs a tray with input on a private bus, with two items: steam ("S",
// column 0) and obsidian ("O", column 2).
func setup(t *testing.T) (steam, obs *fakeItem, in barInput, out *lines) {
	t.Helper()
	addr := privateBus(t)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	in = barInput{pw}
	out, _, _ = startTrayWith(t, addr, pr, func() (*zerobus.Conn, error) { return zerobus.Dial(addr) })
	waitWatcher(t, addr)
	steam = newFakeItem(t, addr, "steam", "", [4]byte{255, 30, 60, 200})
	obs = newFakeItem(t, addr, "obsidian_status_icon_1", "", [4]byte{255, 120, 80, 220})
	steam.register(t)
	out.waitFor(t, "steam", func(s string) bool { return strings.Contains(s, "S") })
	obs.register(t)
	out.waitFor(t, "both items", func(s string) bool { return strings.Contains(s, "S") && strings.Contains(s, "O") })
	return steam, obs, in, out
}

func expectCall(t *testing.T, f *fakeItem, want string) {
	t.Helper()
	select {
	case got := <-f.calls:
		if got != want {
			t.Fatalf("item got %s, want %s", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("item never got %s", want)
	}
}

func expectNoCall(t *testing.T, f *fakeItem) {
	t.Helper()
	select {
	case got := <-f.calls:
		t.Fatalf("unexpected call %s", got)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestClicksAndScroll(t *testing.T) {
	steam, obs, in, _ := setup(t)

	in.send(t, "click left 0 1")
	expectCall(t, steam, "Activate(0,0)")
	in.send(t, "click middle 0 2")
	expectCall(t, steam, "SecondaryActivate(0,0)")
	in.send(t, "click right 0 3")
	expectCall(t, steam, "ContextMenu(0,0)")

	// The second icon starts at column 2, after one space.
	in.send(t, "click left 2 4")
	expectCall(t, obs, "Activate(0,0)")
	expectNoCall(t, steam)

	in.send(t, "scroll up 2 0")
	expectCall(t, steam, "Scroll(-2,vertical)")
	in.send(t, "scroll down 3 0")
	expectCall(t, steam, "Scroll(3,vertical)")
	in.send(t, "scroll right 1 2")
	expectCall(t, obs, "Scroll(1,horizontal)")
	in.send(t, "scroll left 4 2")
	expectCall(t, obs, "Scroll(-4,horizontal)")
}

func TestClickOutsideIconsAndGarbageAreIgnored(t *testing.T) {
	steam, obs, in, _ := setup(t)
	in.send(t,
		"click left 1 1", // the space between the icons
		"click left 9 2", // past the last icon
		"garbage",
		"click left x 3",
		"click left 0",
		"click left 0 4 extra",
		"click left -1 5",
		"scroll up 0 0",
		"\x1b[31m not a command",
		strings.Repeat("click left 0 1 ", 40), // longer than the line limit
		"hover 0", "leave",                    // accepted by the parser, nothing to do yet
	)
	expectNoCall(t, steam)
	expectNoCall(t, obs)
	// The loop still works afterwards.
	in.send(t, "click left 0 9")
	expectCall(t, steam, "Activate(0,0)")
}

func TestActivateUnknownMethodFallsBackToContextMenu(t *testing.T) {
	steam, _, in, _ := setup(t)
	steam.run(t, func() { steam.unknown = "Activate" })
	in.send(t, "click left 0 1")
	expectCall(t, steam, "Activate(0,0)")
	expectCall(t, steam, "ContextMenu(0,0)")
}

func TestItemIsMenuLeftClickOpensTheMenu(t *testing.T) {
	var buf strings.Builder
	tr := &Tray{opt: Options{Out: &buf}}
	tr.items = []*item{{loaded: true, icon: "X", dest: ":1.1", path: "/p", isMenu: true, menu: "/m"}}
	if err := tr.render(true); err != nil {
		t.Fatal(err)
	}
	tg, ok := tr.target(0)
	if !ok || !tg.IsMenu || tg.Menu != "/m" {
		t.Fatalf("target = %+v, %v", tg, ok)
	}
}

// A frozen application must not block the tray: the call is abandoned after
// two seconds and the next click, on another item, still goes through.
func TestFrozenApplicationDoesNotBlockNextClick(t *testing.T) {
	steam, obs, in, _ := setup(t)
	steam.run(t, func() { steam.slow, steam.slowFor = "Activate", 4*time.Second })
	start := time.Now()
	in.send(t, "click left 0 1", "click left 2 2")
	expectCall(t, steam, "Activate(0,0)")
	expectCall(t, obs, "Activate(0,0)")
	if d := time.Since(start); d < 1500*time.Millisecond || d > 3500*time.Millisecond {
		t.Fatalf("the second click arrived after %v, want about 2s (the watchdog)", d)
	}
	// And again on the dropped item: the connection was redialed.
	in.send(t, "click middle 2 3")
	expectCall(t, obs, "SecondaryActivate(0,0)")
}

// If shutdown begins while the control connection is being dialed, the new
// connection is closed and not kept.
func TestConn2ClosesADialThatRacesShutdown(t *testing.T) {
	addr := privateBus(t)
	dialing, release := make(chan struct{}), make(chan struct{})
	var dialed *zerobus.Conn
	c := &control{dial: func() (*zerobus.Conn, error) {
		close(dialing)
		<-release
		conn, err := zerobus.Dial(addr)
		dialed = conn
		return conn, err
	}}
	errc := make(chan error, 1)
	go func() { _, err := c.conn2(); errc <- err }()
	<-dialing
	closed := make(chan struct{})
	go func() { c.close(); close(closed) }()
	select {
	case <-closed: // close must not wait for the dial
	case <-time.After(time.Second):
		t.Fatal("close waited for the dial")
	}
	close(release)
	if err := <-errc; !errors.Is(err, zerobus.ErrClosed) {
		t.Fatalf("conn2 = %v, want ErrClosed", err)
	}
	if c.conn != nil {
		t.Fatal("the connection was kept after shutdown")
	}
	dialed.NewCall("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "GetId", "")
	if _, err := dialed.Call(); err == nil {
		t.Fatal("the connection dialed during shutdown is still open")
	}
}
