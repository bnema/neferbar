// Package app follows the window that has the keyboard focus, using the
// wlr-foreign-toplevel-management protocol that NeferWL, Sway, Hyprland, River
// and Niri speak.
package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode"

	wlr "github.com/bnema/go-wayland-bindings/client/wlrforeigntoplevelmanagement"
	"github.com/bnema/neferclient"
	wl "github.com/bnema/wlturbo"
)

// Window is what the bar shows about the focused window. The zero Window means
// that nothing has the focus.
type Window struct {
	ID    string // the application id, such as "org.mozilla.firefox"
	Title string
}

// stateActivated is the protocol's "this window has the focus" state.
const stateActivated = 2

// Tracker keeps what the compositor said about each window and answers which
// one has the focus. A window's properties apply together when its Done comes,
// as the protocol says, so Focused never shows half an update.
//
// The connection's reader goroutine calls the recording methods while another
// goroutine calls Focused, so a mutex guards the state.
type Tracker struct {
	mu   sync.Mutex
	wins map[uint32]*window
	seq  uint64
	// Changed has a signal waiting whenever Done or Closed may have changed the
	// focus. Capacity 1: a missed signal is harmless, as Focused is read after.
	Changed chan struct{}
}

type window struct {
	pending, shown Window
	pendingActive  bool
	active         bool
	since          uint64 // when it last became active: the newest wins
}

// NewTracker returns an empty tracker.
func NewTracker() *Tracker {
	return &Tracker{wins: map[uint32]*window{}, Changed: make(chan struct{}, 1)}
}

func (t *Tracker) signal() {
	select {
	case t.Changed <- struct{}{}:
	default:
	}
}

func (t *Tracker) get(h uint32) *window {
	w := t.wins[h]
	if w == nil {
		w = &window{}
		t.wins[h] = w
	}
	return w
}

// Title, ID and State record an update that applies at the next Done.
func (t *Tracker) Title(h uint32, s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.get(h).pending.Title = s
}

func (t *Tracker) ID(h uint32, s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.get(h).pending.ID = s
}

func (t *Tracker) State(h uint32, state []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.get(h).pendingActive = hasState(state, stateActivated)
}

// Done applies the updates recorded for window h.
func (t *Tracker) Done(h uint32) {
	defer t.signal()
	t.mu.Lock()
	defer t.mu.Unlock()
	w := t.get(h)
	w.shown = w.pending
	if w.pendingActive && !w.active {
		t.seq++
		w.since = t.seq
	}
	w.active = w.pendingActive
}

// Closed forgets window h.
func (t *Tracker) Closed(h uint32) {
	defer t.signal()
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.wins, h)
}

// Focused returns the window that has the focus; when several do (one per
// seat or output), the one that got it last.
func (t *Tracker) Focused() Window {
	t.mu.Lock()
	defer t.mu.Unlock()
	var best *window
	for _, w := range t.wins {
		if w.active && (best == nil || w.since > best.since) {
			best = w
		}
	}
	if best == nil {
		return Window{}
	}
	return best.shown
}

// hasState reports whether a state array (native-endian uint32 values) has s.
func hasState(state []byte, s uint32) bool {
	for i := 0; i+4 <= len(state); i += 4 {
		v := uint32(state[i]) | uint32(state[i+1])<<8 | uint32(state[i+2])<<16 | uint32(state[i+3])<<24
		if v == s {
			return true
		}
	}
	return false
}

// Name is the short form of an application id: the part after the last dot,
// so "com.github.bnema.dumber" is "dumber". An id without a dot is its own name.
func Name(id string) string {
	if i := strings.LastIndexByte(id, '.'); i >= 0 && i < len(id)-1 {
		return id[i+1:]
	}
	return id
}

// Clean removes control characters. An application chooses its own title, and
// a title must never be able to move the cursor or end a line in a terminal.
func Clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// Watch calls emit with the focused window once, and again each time it
// changes, until ctx ends or the connection fails. display is the Wayland
// socket; empty uses $WAYLAND_DISPLAY.
func Watch(ctx context.Context, display string, emit func(Window) error) error {
	conn, err := neferclient.Connect(ctx, display)
	if err != nil {
		return fmt.Errorf("cannot connect to the compositor: %w", err)
	}
	defer conn.Close()

	t := NewTracker()
	_, _, err = conn.Bind("zwlr_foreign_toplevel_manager_v1", 3, func(c *wl.Context) *wlr.ForeignToplevelManager {
		m := wlr.NewForeignToplevelManager(c)
		m.OnToplevel(func(h *wlr.ForeignToplevelHandle) {
			id := h.ID()
			h.OnTitle(func(s string) { t.Title(id, s) })
			h.OnAppId(func(s string) { t.ID(id, s) })
			h.OnState(func(b []byte) { t.State(id, b) })
			h.OnDone(func() { t.Done(id) })
			h.OnClosed(func() { t.Closed(id); _ = h.Destroy() })
		})
		return m
	})
	if errors.Is(err, neferclient.ErrGlobalNotFound) {
		return errors.New("this compositor does not offer zwlr_foreign_toplevel_manager_v1, so the focused window is unknown")
	}
	if err != nil {
		return fmt.Errorf("cannot ask the compositor for its windows: %w", err)
	}
	// The windows that exist now arrive after one round trip.
	if err = conn.Roundtrip(); err != nil {
		return err
	}
	var last Window
	first := true
	for {
		if err = conn.Dispatch(nil); err != nil {
			return fmt.Errorf("lost the connection to the compositor: %w", err)
		}
		if now := t.Focused(); first || now != last {
			first, last = false, now
			if err = emit(now); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-conn.Wake(): // the connection's own events: drain them
		case <-t.Changed: // the compositor changed a window
		}
	}
}
