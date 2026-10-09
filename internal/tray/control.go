package tray

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
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
	num := func(s string, limit int) (int, bool) {
		n, err := strconv.Atoi(s)
		return n, err == nil && n >= 0 && n <= limit && s[0] != '+'
	}
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
		col, ok1 := num(f[2], 1<<20)
		tok, ok2 := num(f[3], 1<<32-1)
		if !ok1 || !ok2 {
			return command{}, false
		}
		c.col, c.token = col, uint32(tok)
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
		steps, ok1 := num(f[2], maxSteps)
		col, ok2 := num(f[3], 1<<20)
		if !ok1 || !ok2 || steps == 0 {
			return command{}, false
		}
		c.delta, c.col = sign*int32(steps), col
	case "hover":
		col, ok := num(f[len(f)-1], 1<<20)
		if len(f) != 2 || !ok {
			return command{}, false
		}
		c.col = col
	case "leave":
		if len(f) != 1 {
			return command{}, false
		}
	case "menu-activate":
		if len(f) != 3 {
			return command{}, false
		}
		tok, ok1 := num(f[1], 1<<32-1)
		id, ok2 := num(f[2], 1<<31-1)
		if !ok1 || !ok2 {
			return command{}, false
		}
		c.token, c.col = uint32(tok), id // col carries the menu item id
	case "menu-closed":
		tok, ok := num(f[len(f)-1], 1<<32-1)
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
}

// run serves lines until ctx ends.
func (c *control) run(ctx context.Context, lines <-chan string) {
	stop := context.AfterFunc(ctx, c.close)
	defer stop()
	defer c.close()
	for {
		select {
		case <-ctx.Done():
			return
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

// conn2 returns the control connection, dialing it when needed.
func (c *control) conn2() (*zerobus.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return nil, zerobus.ErrClosed
	}
	if c.conn == nil {
		conn, err := c.dial()
		if err != nil {
			return nil, err
		}
		c.conn = conn
	}
	return c.conn, nil
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
	case "click":
		tg, ok := c.t.target(cmd.col)
		if !ok {
			return
		}
		switch cmd.button {
		case btnLeft:
			if tg.IsMenu {
				c.contextMenu(tg)
				return
			}
			err := c.call(tg, "Activate", "ii", func(e *zerobus.Encoder) { e.Int32(0); e.Int32(0) })
			if isUnknownMethod(err) {
				c.contextMenu(tg)
			}
		case btnMiddle:
			c.call(tg, "SecondaryActivate", "ii", func(e *zerobus.Encoder) { e.Int32(0); e.Int32(0) })
		case btnRight:
			c.contextMenu(tg)
		}
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
	conn, err := c.conn2()
	if err != nil {
		if !errors.Is(err, zerobus.ErrClosed) {
			c.log.Warn("tray: cannot reach the session bus", "err", err)
		}
		return err
	}
	args(conn.NewCall(tg.Dest, tg.Path, itemIface, member, sig))
	timer := time.AfterFunc(callTimeout, func() { _ = conn.Close() })
	_, err = conn.Call()
	expired := !timer.Stop()
	var reply *zerobus.Error
	if errors.As(err, &reply) && !expired {
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
			c.log.Warn("tray: application did not answer", "method", member, "dest", tg.Dest, "err", err)
		}
	}
	return err
}
