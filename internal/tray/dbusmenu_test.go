package tray

import (
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bnema/zerobus"

	"github.com/bnema/neferbar/internal/module"
)

// tprop is one property of a test menu node.
type tprop struct {
	k string
	v any // string, bool or int32
}

// tnode is a node of the layout a fake application serves.
type tnode struct {
	id    int32
	props []tprop
	kids  []tnode
	// junk is written as an extra child with a signature that is not a layout node.
	junk bool
}

func (n *tnode) encode(e *zerobus.Encoder) {
	e.Struct()
	e.Int32(n.id)
	a := e.BeginArray('{')
	for _, p := range n.props {
		e.Struct()
		e.Str(p.k)
		switch v := p.v.(type) {
		case string:
			e.Variant("s")
			e.Str(v)
		case bool:
			e.Variant("b")
			e.Bool(v)
		case int32:
			e.Variant("i")
			e.Int32(v)
		}
	}
	e.EndArray(a)
	kids := e.BeginArray('v')
	if n.junk {
		e.Variant("s")
		e.Str("not a node")
	}
	for i := range n.kids {
		e.Variant(layoutSig)
		n.kids[i].encode(e)
	}
	e.EndArray(kids)
}

// menuCall answers a dbusmenu call and records it on mcalls.
func (f *fakeItem) menuCall(m *zerobus.Message) bool {
	r := m.Body()
	switch m.Member {
	case "AboutToShow":
		f.mcalls <- fmt.Sprintf("AboutToShow(%d)", r.Int32())
		f.c.NewReply(m, "b").Bool(false)
	case "Event":
		id, name := r.Int32(), r.Str()
		r.Skip(r.Variant())
		f.mcalls <- fmt.Sprintf("Event(%d,%s)", id, name)
		f.c.NewReply(m, "")
	case "GetLayout":
		f.mcalls <- "GetLayout"
		if f.menu == nil {
			f.c.NewError(m, "org.freedesktop.DBus.Error.UnknownObject", "s").Str("no menu")
			break
		}
		if f.badSig {
			f.c.NewReply(m, "s").Str("not a layout")
			break
		}
		e := f.c.NewReply(m, "u"+layoutSig)
		e.Uint32(1)
		f.menu.encode(e)
	default:
		return true
	}
	time.Sleep(f.mslow[m.Member]) // a slow application: the reply is built, not sent yet
	_, err := f.c.Send()
	return err == nil
}

func expectMenuCall(t *testing.T, f *fakeItem, want string) {
	t.Helper()
	select {
	case got := <-f.mcalls:
		if got != want {
			t.Fatalf("menu got %s, want %s", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("menu never got %s", want)
	}
}

func expectNoMenuCall(t *testing.T, f *fakeItem) {
	t.Helper()
	select {
	case got := <-f.mcalls:
		t.Fatalf("unexpected menu call %s", got)
	case <-time.After(150 * time.Millisecond):
	}
}

func sampleLayout() *tnode {
	return &tnode{id: 0, kids: []tnode{
		{id: 1, props: []tprop{{"label", "_Open"}}},
		{id: 2, props: []tprop{{"type", "separator"}}},
		{id: 3, props: []tprop{{"label", "Mu__te"}, {"toggle-type", "checkmark"}, {"toggle-state", int32(1)}}},
		{id: 4, props: []tprop{{"label", "Settings"}, {"children-display", "submenu"}}, kids: []tnode{
			{id: 5, props: []tprop{{"label", "Advanced"}}},
		}},
		{id: 6, props: []tprop{{"label", "Quit"}, {"enabled", false}}},
		{id: 7, props: []tprop{{"label", "Hidden"}, {"visible", false}}, kids: []tnode{{id: 70, props: []tprop{{"label", "Under hidden"}}}}},
		{id: 8, props: []tprop{{"label", "Radio"}, {"toggle-type", "radio"}, {"toggle-state", int32(0)}}},
	}, junk: true}
}

// menuReady gives steam a dbusmenu and waits until the tray knows it: right
// clicks fall back to ContextMenu until then, and the first menu line proves it.
func menuReady(t *testing.T, steam *fakeItem, in barInput, out *lines, layout *tnode) module.Control {
	t.Helper()
	steam.run(t, func() {
		steam.menu = layout
		steam.c.NewSignal(itemPath, itemIface, "NewStatus", "s").Str("Active")
		_, _ = steam.c.Send()
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		in.send(t, "click right 0 9")
		wait := time.After(600 * time.Millisecond)
	poll:
		for {
			select {
			case s := <-out.ch:
				if j, ok := strings.CutPrefix(s, controlPrefix); ok && strings.Contains(j, `"type":"menu"`) {
					c, err := module.ParseControl([]byte(strings.TrimSuffix(j, "\x07")))
					if err != nil {
						t.Fatalf("the bar would refuse the menu line %q: %v", j, err)
					}
					// Drain what the successful open recorded.
					for _, want := range []string{"AboutToShow(0)", "Event(0,opened)", "GetLayout"} {
						expectMenuCall(t, steam, want)
					}
					return c
				}
			case <-steam.calls: // ContextMenu: the tray has not seen the menu yet
				break poll
			case <-wait:
				break poll
			}
		}
		for len(steam.mcalls) > 0 {
			<-steam.mcalls
		}
	}
	t.Fatal("the tray never produced a menu")
	return module.Control{}
}

func TestRightClickOpensTheDbusmenu(t *testing.T) {
	steam, _, in, out := setup(t)
	c := menuReady(t, steam, in, out, sampleLayout())

	if c.Type != module.ControlMenu || c.Click != 9 || c.Col != 0 || c.Width != 1 {
		t.Fatalf("control = %+v", c)
	}
	type flat struct {
		id           int32
		label, kind  string
		enabled, chk bool
		kids         int
	}
	var got []flat
	for _, it := range c.Items {
		got = append(got, flat{it.ID, it.Label, it.Kind, it.Enabled, it.Checked, len(it.Items)})
	}
	want := []flat{
		{1, "Open", "normal", true, false, 0},
		{2, "", "separator", true, false, 0},
		{3, "Mute", "check", true, true, 0}, // "__" is a literal underscore: "Mu_te"
		{4, "Settings", "normal", true, false, 1},
		{6, "Quit", "normal", false, false, 0},
		{8, "Radio", "radio", true, false, 0},
	}
	want[2].label = "Mu_te"
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("items = %+v\nwant   %+v", got, want)
	}
	if sub := c.Items[3].Items[0]; sub.ID != 5 || sub.Label != "Advanced" {
		t.Fatalf("submenu = %+v", c.Items[3].Items)
	}
	expectNoCall(t, steam) // no ContextMenu: the bar draws the menu

	// A choice reaches the application, once.
	in.send(t, "menu-activate 9 5")
	expectMenuCall(t, steam, "Event(5,clicked)")
	in.send(t, "menu-activate 9 5", "menu-closed 9")
	expectNoMenuCall(t, steam)
}

func TestMenuClosedAndStaleTokens(t *testing.T) {
	steam, _, in, out := setup(t)
	menuReady(t, steam, in, out, sampleLayout())

	in.send(t, "menu-activate 8 1") // another token
	in.send(t, "menu-closed 8")
	in.send(t, "menu-activate 9 99") // an id the menu does not hold
	in.send(t, "menu-activate 9 7")  // an invisible item
	in.send(t, "menu-activate 9 70") // a child of an invisible item
	expectNoMenuCall(t, steam)

	in.send(t, "menu-closed 9")
	expectMenuCall(t, steam, "Event(0,closed)")
	in.send(t, "menu-closed 9", "menu-activate 9 1") // forgotten after closing
	expectNoMenuCall(t, steam)
}

func TestNewerMenuReplacesTheOlder(t *testing.T) {
	steam, _, in, out := setup(t)
	menuReady(t, steam, in, out, sampleLayout())
	in.send(t, "click right 0 10")
	out.waitFor(t, "the second menu", func(s string) bool { return strings.Contains(s, `"click":10`) })
	for _, want := range []string{"AboutToShow(0)", "Event(0,opened)", "GetLayout"} {
		expectMenuCall(t, steam, want)
	}
	in.send(t, "menu-activate 9 1") // the first menu is gone
	expectNoMenuCall(t, steam)
	in.send(t, "menu-activate 10 1")
	expectMenuCall(t, steam, "Event(1,clicked)")
}

func TestLeftClickOnAMenuOnlyItemOpensTheMenu(t *testing.T) {
	steam, _, in, out := setup(t)
	menuReady(t, steam, in, out, sampleLayout())
	// ItemIsMenu is not served by the fake, so the left click calls Activate,
	// which it refuses: the fallback opens the menu.
	steam.run(t, func() { steam.unknown = "Activate" })
	in.send(t, "click left 0 12")
	expectCall(t, steam, "Activate(0,0)")
	out.waitFor(t, "the menu after the fallback", func(s string) bool { return strings.Contains(s, `"click":12`) })
}

func TestMenuLayoutLimitsAndDecoding(t *testing.T) {
	// Build a deep and wide layout and decode it through the real reply path.
	deep := tnode{id: 100, props: []tprop{{"label", "d"}}}
	for i := int32(1); i < 12; i++ {
		deep = tnode{id: 100 + i, props: []tprop{{"label", "d"}}, kids: []tnode{deep}}
	}
	root := tnode{id: 0, kids: []tnode{deep}}
	for i := int32(0); i < 600; i++ {
		root.kids = append(root.kids, tnode{id: 1000 + i, props: []tprop{{"label", strings.Repeat("x", 300)}}})
	}
	items, ids := decodeTestLayout(t, &root)
	if n := countItems(items); n > maxMenuItems || len(ids) != n {
		t.Fatalf("items %d, ids %d, want at most %d and equal", n, len(ids), maxMenuItems)
	}
	depth := 0
	for l := items; len(l) > 0; l = l[0].Items {
		depth++
	}
	if depth != maxMenuDepth {
		t.Fatalf("depth %d, want %d", depth, maxMenuDepth)
	}
	if got := len(items[len(items)-1].Label); got != 256 {
		t.Fatalf("label of %d bytes, want 256", got)
	}
	line, _, err := encodeMenu(menuLine{Type: "menu", Width: 1, Click: 1, Items: items})
	if err != nil || len(line) > maxMenuLine {
		t.Fatalf("encodeMenu: %d bytes, %v", len(line), err)
	}
	if _, err = module.ParseControl(line); err != nil {
		t.Fatalf("the bar refuses the trimmed menu: %v", err)
	}
}

func TestMenuLabelMnemonics(t *testing.T) {
	for in, want := range map[string]string{
		"_Open": "Open", "Mu__te": "Mu_te", "plain": "plain", "": "", "_": "", "a_": "a", "__": "_", "_a_b": "ab", "é_è": "éè",
	} {
		if got := cleanLabel(in); got != want {
			t.Errorf("cleanLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// decodeTestLayout encodes root as a GetLayout reply and decodes it again.
func decodeTestLayout(t *testing.T, root *tnode) ([]menuItem, []int32) {
	t.Helper()
	addr := privateBus(t)
	srv, cli := dial(t, addr), dial(t, addr)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var m *zerobus.Message
		for { // the bus first sends signals such as NameAcquired
			var err error
			if m, err = srv.ReadMessage(); err != nil {
				return
			}
			if m.Type == zerobus.TypeMethodCall {
				break
			}
		}
		e := srv.NewReply(m, "u"+layoutSig)
		e.Uint32(1)
		root.encode(e)
		_, _ = srv.Send()
	}()
	cli.NewCall(srv.UniqueName(), "/MenuBar", menuIface, "GetLayout", "")
	m, err := cli.Call()
	if err != nil {
		t.Fatal(err)
	}
	items, ids, err := decodeLayout(m.Body())
	if err != nil {
		t.Fatal(err)
	}
	_ = srv.Close()
	<-done
	return items, ids
}

// A slow application cannot hold the tray for more than the shared budget, and
// it is told the menu is closed when it was told it opened.
func TestSlowMenuUsesOneDeadlineAndIsClosed(t *testing.T) {
	old := menuBudget
	menuBudget = 1200 * time.Millisecond
	t.Cleanup(func() { menuBudget = old })

	steam, _, in, out := setup(t)
	menuReady(t, steam, in, out, sampleLayout()) // the tray knows the menu now
	in.send(t, "menu-closed 9")
	expectMenuCall(t, steam, "Event(0,closed)")

	// Every call takes 500 ms: each is under the 2 s call limit, and the
	// sequence runs out of its 1.2 s while GetLayout is waiting.
	steam.run(t, func() {
		steam.mslow = map[string]time.Duration{"AboutToShow": 500 * time.Millisecond, "Event": 500 * time.Millisecond, "GetLayout": 500 * time.Millisecond}
	})
	start := time.Now()
	in.send(t, "click right 0 20")
	expectMenuCall(t, steam, "AboutToShow(0)")
	expectMenuCall(t, steam, "Event(0,opened)")
	expectMenuCall(t, steam, "GetLayout")
	// GetLayout ran out of budget: the application hears "closed".
	expectMenuCall(t, steam, "Event(0,closed)")
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("opening the menu took %v", d)
	}
	noControlLineFor(t, out, "menu", 300*time.Millisecond)
	// The menu is forgotten: a late choice does nothing.
	in.send(t, "menu-activate 20 1")
	expectNoMenuCall(t, steam)
}

// An application with an empty menu is also told that it closed.
func TestEmptyMenuIsClosedAgain(t *testing.T) {
	steam, _, in, out := setup(t)
	menuReady(t, steam, in, out, sampleLayout())
	in.send(t, "menu-closed 9")
	expectMenuCall(t, steam, "Event(0,closed)")
	steam.run(t, func() { steam.menu = &tnode{id: 0} })
	// The tray still thinks it has a menu path: the layout is just empty.
	in.send(t, "click right 0 21")
	for _, want := range []string{"AboutToShow(0)", "Event(0,opened)", "GetLayout", "Event(0,closed)"} {
		expectMenuCall(t, steam, want)
	}
	in.send(t, "menu-activate 21 1")
	expectNoMenuCall(t, steam)
}

// noControlLineFor fails when a control line of the given type arrives within d.
func noControlLineFor(t *testing.T, out *lines, typ string, d time.Duration) {
	t.Helper()
	timeout := time.After(d)
	for {
		select {
		case s, ok := <-out.ch:
			if ok && strings.HasPrefix(s, controlPrefix) && strings.Contains(s, `"type":"`+typ+`"`) {
				t.Fatalf("unexpected %s line %q", typ, s)
			}
		case <-timeout:
			return
		}
	}
}

// A reply that is not a layout makes no menu, and the application hears that
// the menu it was told to open is closed.
func TestUnexpectedLayoutSignature(t *testing.T) {
	steam, _, in, out := setup(t)
	menuReady(t, steam, in, out, sampleLayout())
	in.send(t, "menu-closed 9")
	expectMenuCall(t, steam, "Event(0,closed)")
	steam.run(t, func() { steam.badSig = true })
	in.send(t, "click right 0 30")
	for _, want := range []string{"AboutToShow(0)", "Event(0,opened)", "GetLayout", "Event(0,closed)"} {
		expectMenuCall(t, steam, want)
	}
	noControlLineFor(t, out, "menu", 300*time.Millisecond)
	in.send(t, "menu-activate 30 1")
	expectNoMenuCall(t, steam)
}

// decodeRaw sends body as the reply to one call on c and decodes it.
func decodeRaw(t testing.TB, c *zerobus.Conn, next chan<- reply, sig string, body []byte) ([]menuItem, []int32, error) {
	t.Helper()
	next <- reply{sig, body}
	c.NewCall("x.y", "/MenuBar", menuIface, "GetLayout", "")
	m, err := c.Call()
	if err != nil {
		t.Fatal(err)
	}
	return decodeLayout(m.Body())
}

// Bodies that are cut short or inconsistent must make an error, never a panic
// or an unbounded read.
func TestDecodeLayoutTruncatedAndMalformed(t *testing.T) {
	addr, next := rawBus(t)
	c := rawDial(t, addr)
	good := layoutBody(sampleLayout())
	if items, _, err := decodeRaw(t, c, next, "u"+layoutSig, good); err != nil || len(items) != 6 {
		t.Fatalf("the hand-built body does not decode like the encoder's: %d items, %v", len(items), err)
	}
	// Every strict prefix of a valid body.
	for n := 0; n < len(good); n++ {
		if _, _, err := decodeRaw(t, c, next, "u"+layoutSig, good[:n]); err == nil {
			t.Fatalf("a body cut at %d of %d bytes decoded without error", n, len(good))
		}
	}
	// A child array that claims more bytes than the message has.
	bad := slices.Clone(good)
	// The children array of the root sits right after the root's properties
	// (empty): find it by decoding position: revision(4) + pad(4) + id(4) + props len(4) + pad + kids len.
	binary.LittleEndian.PutUint32(bad[16:], 1<<20)
	if _, _, err := decodeRaw(t, c, next, "u"+layoutSig, bad); err == nil {
		t.Fatal("an oversized child array decoded without error")
	}
	// An item with a property of the wrong type: ignored, not an error.
	odd := &tnode{id: 0, kids: []tnode{{id: 1, props: []tprop{{"label", int32(5)}, {"enabled", "yes"}, {"visible", int32(0)}}}}}
	items, _, err := decodeRaw(t, c, next, "u"+layoutSig, layoutBody(odd))
	if err != nil || len(items) != 1 || items[0].Label != "" || !items[0].Enabled {
		t.Fatalf("odd properties: %+v, %v", items, err)
	}
	// Garbage of every kind does not panic.
	for _, junk := range [][]byte{nil, {0}, make([]byte, 7), make([]byte, 64), slices.Repeat([]byte{0xff}, 100)} {
		decodeRaw(t, c, next, "u"+layoutSig, junk)
	}
}

func FuzzDecodeLayout(f *testing.F) {
	addr, next := rawBus(f)
	c := rawDial(f, addr)
	f.Add(layoutBody(sampleLayout()))
	f.Add(layoutBody(&tnode{id: 0}))
	f.Add(layoutBody(&tnode{id: 0, kids: []tnode{{id: 1, props: []tprop{{"label", "x"}, {"type", "separator"}}}}}))
	f.Add(layoutBody(sampleLayout())[:30])
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 1<<16 {
			return
		}
		items, ids, err := decodeRaw(t, c, next, "u"+layoutSig, body)
		if err != nil {
			return
		}
		if n := countItems(items); n > maxMenuItems || len(ids) != n {
			t.Fatalf("%d items, %d ids", n, len(ids))
		}
		var walk func(l []menuItem, depth int)
		walk = func(l []menuItem, depth int) {
			if len(l) > 0 && depth > maxMenuDepth {
				t.Fatalf("depth %d", depth)
			}
			for _, it := range l {
				if len(it.Label) > maxMenuLabel || it.ID < 0 {
					t.Fatalf("item %+v", it)
				}
				walk(it.Items, depth+1)
			}
		}
		walk(items, 1)
		// Whatever decodes must be acceptable to the bar (an empty menu is
		// not sent at all).
		if len(items) == 0 {
			return
		}
		line, _, err := encodeMenu(menuLine{Type: "menu", Width: 1, Click: 1, Items: items})
		if err == nil {
			if _, err = module.ParseControl(line); err != nil {
				t.Fatalf("the bar refuses a decoded menu: %v", err)
			}
		}
	})
}
