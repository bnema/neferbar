package tray

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zerobus"
)

const (
	// callTimeout is how long the control loop waits for an application. A
	// frozen one must not keep the next click from working.
	callTimeout = 2 * time.Second
	// maxInputLine bounds a line from the bar; the bar sends at most 127 bytes.
	maxInputLine = 256
	// maxSteps bounds the steps of a scroll line.
	maxSteps = 1 << 16
	// tooltipDelay is how long the pointer rests on an icon before its
	// tooltip is sent to the bar.
	tooltipDelay = 500 * time.Millisecond
)

// Control lines to the bar: OSC 777 "neferbar", a JSON object, BEL, newline.
const (
	controlPrefix = "\x1b]777;neferbar;"
	controlSuffix = "\x07\n"
)

// lockedWriter serializes writes from the main loop and the control loop to
// the shared output.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// button is a mouse button named in a click line.
type button uint8

const (
	btnLeft button = iota + 1
	btnMiddle
	btnRight
)

// command is one parsed line from the bar.
type command struct {
	verb   string // "click", "scroll", "hover", "leave", "menu-activate", "menu-closed"
	button button // click
	vert   bool   // scroll: vertical axis
	delta  int32  // scroll: signed steps, down and right positive
	col    int
	token  uint32 // click: the press token
}

// parseCommand reads one line of the protocol. Every field must be present,
// numbers must be plain non-negative decimals, and nothing may follow.
func parseCommand(line string) (command, bool) {
	f := strings.Fields(line)
	if len(f) == 0 {
		return command{}, false
	}
	// Plain decimal digits only: ParseUint takes no sign, and the bit size
	// keeps the value in range on 32-bit platforms, where int is 32 bits.
	num := func(s string, bits int, limit uint64) (uint64, bool) {
		n, err := strconv.ParseUint(s, 10, bits)
		return n, err == nil && n <= limit
	}
	// Columns are bounded by 1<<20; tokens use the full uint32.
	col := func(s string) (int, bool) { n, ok := num(s, 31, 1<<20); return int(n), ok }
	c := command{verb: f[0]}
	switch f[0] {
	case "click":
		if len(f) != 4 {
			return command{}, false
		}
		switch f[1] {
		case "left":
			c.button = btnLeft
		case "middle":
			c.button = btnMiddle
		case "right":
			c.button = btnRight
		default:
			return command{}, false
		}
		cl, ok1 := col(f[2])
		tok, ok2 := num(f[3], 32, 1<<32-1)
		if !ok1 || !ok2 {
			return command{}, false
		}
		c.col, c.token = cl, uint32(tok)
	case "scroll":
		if len(f) != 4 {
			return command{}, false
		}
		sign := int32(1)
		switch f[1] {
		case "down":
			c.vert = true
		case "up":
			c.vert, sign = true, -1
		case "right":
		case "left":
			sign = -1
		default:
			return command{}, false
		}
		steps, ok1 := num(f[2], 31, maxSteps)
		cl, ok2 := col(f[3])
		if !ok1 || !ok2 || steps == 0 {
			return command{}, false
		}
		c.delta, c.col = sign*int32(steps), cl
	case "hover":
		cl, ok := col(f[len(f)-1])
		if len(f) != 2 || !ok {
			return command{}, false
		}
		c.col = cl
	case "leave":
		if len(f) != 1 {
			return command{}, false
		}
	case "menu-activate":
		if len(f) != 3 {
			return command{}, false
		}
		tok, ok1 := num(f[1], 32, 1<<32-1)
		id, ok2 := num(f[2], 31, 1<<31-1)
		if !ok1 || !ok2 {
			return command{}, false
		}
		c.token, c.col = uint32(tok), int(id) // col carries the menu item id
	case "menu-closed":
		tok, ok := num(f[len(f)-1], 32, 1<<32-1)
		if len(f) != 2 || !ok {
			return command{}, false
		}
		c.token = uint32(tok)
	default:
		return command{}, false
	}
	return c, true
}

// readCommands sends the lines of r to out until r ends or ctx is done. A
// line longer than maxInputLine is dropped whole.
func readCommands(ctx context.Context, r io.Reader, out chan<- string, log *slog.Logger) {
	br := bufio.NewReaderSize(r, maxInputLine)
	skipping := false
	for {
		b, err := br.ReadSlice('\n')
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			skipping = true // the rest of this line is dropped too
			continue
		case err != nil && len(b) == 0:
			return
		}
		if skipping {
			skipping = false
			log.Debug("tray: input line too long")
		} else if err == nil {
			select {
			case out <- strings.TrimRight(string(b), "\r\n"):
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// control performs the actions the bar asks for. It owns a second bus
// connection, so a slow application never holds up the tray's own loop.
type control struct {
	t    *Tray
	dial func() (*zerobus.Conn, error)
	log  *slog.Logger

	mu   sync.Mutex // guards conn and done: ctx ending closes conn from another goroutine
	conn *zerobus.Conn
	done bool

	// The tooltip and menu state belong to the run goroutine.
	tipTimer *time.Timer
	hov      hover
	menu     *lastMenu
}

// hover is the icon the pointer rests on, and whether its tooltip is shown.
type hover struct {
	active     bool
	dest, path string
	col        int
	shown      bool
}

// run serves lines until ctx ends.
func (c *control) run(ctx context.Context, lines <-chan string) {
	stop := context.AfterFunc(ctx, c.close)
	defer stop()
	defer c.close()
	c.tipTimer = time.NewTimer(time.Hour)
	c.tipTimer.Stop()
	defer c.tipTimer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.tipTimer.C:
			c.showTooltip()
		case line := <-lines:
			cmd, ok := parseCommand(line)
			if !ok {
				c.log.Debug("tray: ignoring input line", "line", line)
				continue
			}
			c.handle(cmd)
		}
	}
}

func (c *control) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.done = true
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

// conn2 returns the control connection, dialing it when needed. The dial runs
// outside the lock, so shutdown never waits for it: if shutdown began meanwhile
// the new connection is closed and ErrClosed is returned.
func (c *control) conn2() (*zerobus.Conn, error) {
	c.mu.Lock()
	if c.done {
		c.mu.Unlock()
		return nil, zerobus.ErrClosed
	}
	if conn := c.conn; conn != nil {
		c.mu.Unlock()
		return conn, nil
	}
	c.mu.Unlock()

	conn, err := c.dial()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		_ = conn.Close()
		return nil, zerobus.ErrClosed
	}
	c.conn = conn
	return conn, nil
}

// drop forgets conn after a transport error; the next call dials again.
func (c *control) drop(conn *zerobus.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = conn.Close()
	if c.conn == conn {
		c.conn = nil
	}
}

func (c *control) handle(cmd command) {
	switch cmd.verb {
	case "hover":
		tg, ok := c.t.target(cmd.col)
		if !ok { // between icons
			c.endHover()
			return
		}
		if c.hov.active && c.hov.dest == tg.Dest && c.hov.path == tg.Path {
			return // still on the same icon
		}
		c.endHover()
		c.hov = hover{active: true, dest: tg.Dest, path: tg.Path, col: cmd.col}
		c.tipTimer.Reset(tooltipDelay)
	case "leave":
		c.endHover()
	case "click":
		c.endHover()
		tg, ok := c.t.target(cmd.col)
		if !ok {
			return
		}
		switch cmd.button {
		case btnLeft:
			if tg.IsMenu {
				c.openMenu(tg, cmd.token)
				return
			}
			err := c.call(tg, "Activate", "ii", func(e *zerobus.Encoder) { e.Int32(0); e.Int32(0) })
			if isUnknownMethod(err) {
				c.openMenu(tg, cmd.token)
			}
		case btnMiddle:
			c.call(tg, "SecondaryActivate", "ii", func(e *zerobus.Encoder) { e.Int32(0); e.Int32(0) })
		case btnRight:
			c.openMenu(tg, cmd.token)
		}
	case "menu-activate":
		c.menuActivate(cmd.token, int32(cmd.col)) // col carries the item id
	case "menu-closed":
		c.menuClosed(cmd.token)
	case "scroll":
		tg, ok := c.t.target(cmd.col)
		if !ok {
			return
		}
		orientation := "horizontal"
		if cmd.vert {
			orientation = "vertical"
		}
		c.call(tg, "Scroll", "is", func(e *zerobus.Encoder) { e.Int32(cmd.delta); e.Str(orientation) })
	}
}

// endHover stops the tooltip timer and, if the tooltip was sent, tells the bar
// to close it.
func (c *control) endHover() {
	c.tipTimer.Stop()
	if c.hov.shown {
		c.emit(map[string]string{"type": "close"})
	}
	c.hov = hover{}
}

// showTooltip sends the tooltip of the hovered icon when the pointer is still
// on it and the application has something to say.
func (c *control) showTooltip() {
	if !c.hov.active || c.hov.shown {
		return
	}
	tg, ok := c.t.target(c.hov.col)
	if !ok || tg.Dest != c.hov.dest || tg.Path != c.hov.path {
		c.endHover()
		return
	}
	title, body := plainText(tg.TipTitle), plainText(tg.TipBody)
	if title == "" && body == "" {
		return
	}
	c.hov.shown = true
	c.emit(struct {
		Type  string `json:"type"`
		Col   int    `json:"col"`
		Width int    `json:"width"`
		Title string `json:"title"`
		Body  string `json:"body"`
	}{"tooltip", tg.Start, tg.Width, title, body})
}

// emit writes a control line for the bar.
func (c *control) emit(v any) {
	var buf bytes.Buffer
	buf.WriteString(controlPrefix)
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // "&" and "<" stay readable; the bar parses JSON either way
	if err := enc.Encode(v); err != nil {
		c.log.Warn("tray: cannot encode a control line", "err", err)
		return
	}
	buf.Truncate(buf.Len() - 1) // Encode ends with a newline; the BEL goes first
	buf.WriteString(controlSuffix)
	c.write(buf.Bytes())
}

// writeLine writes the control line whose JSON is b.
func (c *control) writeLine(b []byte) {
	line := make([]byte, 0, len(controlPrefix)+len(b)+len(controlSuffix))
	line = append(append(append(line, controlPrefix...), b...), controlSuffix...)
	c.write(line)
}

func (c *control) write(line []byte) {
	if _, err := c.t.opt.Out.Write(line); err != nil {
		c.log.Debug("tray: cannot write a control line", "err", err)
	}
}

// contextMenu asks the application to show its own menu.
func (c *control) contextMenu(tg Target) {
	c.call(tg, "ContextMenu", "ii", func(e *zerobus.Encoder) { e.Int32(0); e.Int32(0) })
}

func isUnknownMethod(err error) bool {
	var e *zerobus.Error
	return errors.As(err, &e) && strings.HasSuffix(e.Name, "UnknownMethod")
}

// call runs one StatusNotifierItem method with a watchdog. A D-Bus error
// reply is returned as is; a transport failure or a timeout drops the
// connection and is logged.
func (c *control) call(tg Target, member, sig string, args func(*zerobus.Encoder)) error {
	return c.callOn(tg.Dest, tg.Path, itemIface, member, sig, args, nil)
}

// callOn is call for any interface. reply, if set, reads the body of a
// successful reply; it must not keep what it reads.
func (c *control) callOn(dest, path, iface, member, sig string, args func(*zerobus.Encoder), reply func(*zerobus.Message)) error {
	conn, err := c.conn2()
	if err != nil {
		if !errors.Is(err, zerobus.ErrClosed) {
			c.log.Warn("tray: cannot reach the session bus", "err", err)
		}
		return err
	}
	args(conn.NewCall(dest, path, iface, member, sig))
	timer := time.AfterFunc(callTimeout, func() { _ = conn.Close() })
	m, err := conn.Call()
	if err == nil && reply != nil {
		reply(m)
	}
	expired := !timer.Stop()
	var refused *zerobus.Error
	if (errors.As(err, &refused) || errors.Is(err, zerobus.ErrInvalid)) && !expired {
		// An error reply, or a name or path the library would not send:
		// the connection is fine.
		c.log.Debug("tray: the application refused", "method", member, "err", err)
		return err
	}
	if err != nil || expired {
		c.drop(conn)
		if err == nil {
			err = zerobus.ErrClosed
		}
		c.mu.Lock()
		stopping := c.done
		c.mu.Unlock()
		if !stopping {
			c.log.Warn("tray: application did not answer", "method", member, "dest", dest, "err", err)
		}
	}
	return err
}

// transportFailed reports whether err means the call did not complete, as
// opposed to the application answering with an error.
func transportFailed(err error) bool {
	var refused *zerobus.Error
	return err != nil && !errors.As(err, &refused) && !errors.Is(err, zerobus.ErrInvalid)
}

// openMenu shows the application's menu for the press numbered token: its
// dbusmenu when it has one, else it asks the application to show its own.
func (c *control) openMenu(tg Target, token uint32) {
	if tg.Menu == "" || tg.Menu == noMenuPath {
		c.contextMenu(tg)
		return
	}
	// Ignore refusals: many applications do not implement these.
	err := c.callOn(tg.Dest, tg.Menu, menuIface, "AboutToShow", "i", func(e *zerobus.Encoder) { e.Int32(0) }, nil)
	if transportFailed(err) {
		return
	}
	err = c.menuEvent(tg.Dest, tg.Menu, 0, "opened")
	if transportFailed(err) {
		return
	}
	var items []menuItem
	var ids []int32
	var decodeErr error
	err = c.callOn(tg.Dest, tg.Menu, menuIface, "GetLayout", "iias", func(e *zerobus.Encoder) {
		e.Int32(0)
		e.Int32(-1)
		a := e.BeginArray('s')
		e.EndArray(a)
	}, func(m *zerobus.Message) {
		if m.Signature != "u"+layoutSig {
			decodeErr = errors.New("unexpected layout signature " + m.Signature)
			return
		}
		items, ids, decodeErr = decodeLayout(m.Body())
	})
	if err != nil {
		return
	}
	if decodeErr != nil {
		c.log.Warn("tray: unreadable menu", "dest", tg.Dest, "err", decodeErr)
		return
	}
	if len(items) == 0 {
		c.log.Debug("tray: the menu is empty", "dest", tg.Dest)
		return
	}
	line, ids, encErr := encodeMenu(menuLine{Type: "menu", Col: tg.Start, Width: tg.Width, Click: token, Items: items})
	if encErr != nil {
		c.log.Warn("tray: cannot send the menu", "dest", tg.Dest, "err", encErr)
		return
	}
	c.menu = &lastMenu{token: token, dest: tg.Dest, path: tg.Menu, ids: ids}
	c.writeLine(line)
}

// menuEvent sends a dbusmenu Event with no data.
func (c *control) menuEvent(dest, path string, id int32, event string) error {
	return c.callOn(dest, path, menuIface, "Event", "isvu", func(e *zerobus.Encoder) {
		e.Int32(id)
		e.Str(event)
		e.Variant("i")
		e.Int32(0)
		e.Uint32(0)
	}, nil)
}

// menuActivate carries out a choice of the bar: only for the menu it was last
// given, and only for an item that menu holds.
func (c *control) menuActivate(token uint32, id int32) {
	m := c.menu
	if m == nil || m.token != token {
		c.log.Debug("tray: ignoring a choice for an unknown menu", "token", token)
		return
	}
	if _, found := slices.BinarySearch(m.ids, id); !found {
		c.log.Debug("tray: ignoring a choice that is not in the menu", "token", token, "id", id)
		return
	}
	c.menu = nil
	c.menuEvent(m.dest, m.path, id, "clicked")
}

// menuClosed tells the application its menu is closed.
func (c *control) menuClosed(token uint32) {
	m := c.menu
	if m == nil || m.token != token {
		return
	}
	c.menu = nil
	c.menuEvent(m.dest, m.path, 0, "closed")
}
