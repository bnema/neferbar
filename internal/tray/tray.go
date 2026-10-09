// Package tray shows the system tray (StatusNotifierItem) as one line of
// Nerd Font glyphs, for "neferbar tray".
//
// It acts as the StatusNotifierWatcher when no other program does, and as a
// plain host of the existing watcher otherwise, so it can run next to another
// panel. Everything runs on one goroutine around zerobus.Conn.ReadMessage;
// calls to applications never block it, as their replies are matched by
// serial when they arrive.
package tray

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	vt "github.com/bnema/vev-vt"
	"github.com/bnema/zerobus"
)

const (
	watcherName  = "org.kde.StatusNotifierWatcher"
	watcherPath  = "/StatusNotifierWatcher"
	watcherIface = "org.kde.StatusNotifierWatcher"
	itemIface    = "org.kde.StatusNotifierItem"
	itemPath     = "/StatusNotifierItem"
	propsIface   = "org.freedesktop.DBus.Properties"
	busName      = "org.freedesktop.DBus"
	busPath      = "/org/freedesktop/DBus"
)

// RGB is a color.
type RGB = [3]uint8

// Options configure a Tray.
type Options struct {
	Resolver *Resolver
	// Foreground colors items without an icon to take a color from;
	// Background is what tinted icons must stand out against; Accent marks
	// items that need attention.
	Foreground, Background, Accent RGB
	Out                            io.Writer // one line per change
	Log                            *slog.Logger
	// In carries the bar's pointer lines (see the README); nil ignores input.
	In io.Reader
	// Dial opens the connection that runs the actions In asks for, apart from
	// the one that serves the tray. Required with In.
	Dial func() (*zerobus.Conn, error)
}

type status uint8

const (
	active status = iota
	passive
	attention
)

type item struct {
	service string // as listed by the watcher: "bus/path" or "bus"
	dest    string // where to send calls: the owner, or the registered bus name
	owner   string // the unique name; signals come from it
	path    string

	id, iconName, title string
	status              status
	icon                string // resolved glyph; "" hides the item
	resolved            bool
	color               RGB
	hasColor            bool
	menu                string // object path of its dbusmenu, "" when none
	isMenu              bool   // ItemIsMenu: Activate should open the menu
	loaded              bool   // GetAll answered at least once
	pending             uint32 // serial of a GetAll in flight
	stale               bool   // a change arrived while GetAll was in flight
}

type pendKind uint8

const (
	pendGetAll pendKind = iota + 1
	pendItems           // the watcher's RegisteredStatusNotifierItems
	pendName            // RequestName of the watcher name
	pendIgnore
)

type pend struct {
	serial uint32
	kind   pendKind
	it     *item
}

// Tray is one running tray.
type Tray struct {
	c       *zerobus.Conn
	opt     Options
	watcher bool // this process owns the watcher name
	host    string
	items   []*item
	pending []pend
	frame   []byte
	last    []byte
	hist    histogram
	err     error // first fatal error from dispatch

	snap    snapshot // what the last line printed holds, for the control loop
	targets []Target // scratch for the next snapshot
}

// Run serves the tray on c until ctx ends or the bus connection fails. It
// closes c.
func Run(ctx context.Context, c *zerobus.Conn, opt Options) error {
	t := &Tray{c: c, opt: opt, host: fmt.Sprintf("org.kde.StatusNotifierHost-%d", os.Getpid()),
		frame: make([]byte, 0, 256), last: make([]byte, 0, 256)}
	if t.opt.Log == nil {
		t.opt.Log = slog.New(slog.DiscardHandler)
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	defer func() { _ = c.Close() }()

	if opt.In != nil {
		if opt.Dial == nil {
			return fmt.Errorf("tray: Options.In needs Options.Dial")
		}
		t.opt.Out = &lockedWriter{w: opt.Out}
		cctx, cancel := context.WithCancel(ctx)
		lines := make(chan string, 16)
		ctl := &control{t: t, dial: opt.Dial, log: t.opt.Log}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); ctl.run(cctx, lines) }()
		// The reader may sit in a blocking Read of stdin that nothing can
		// interrupt; it is not waited for, and ends with the process.
		go readCommands(cctx, opt.In, lines, t.opt.Log)
		defer func() { cancel(); wg.Wait() }()
	}

	// Calls made from here on only go to the bus, which always answers.
	// Handle sees what arrives meanwhile.
	c.Handle = t.dispatch
	for _, rule := range [...]string{
		"type='signal',interface='" + itemIface + "'",
		// The bus matches a well-known sender by its current owner.
		"type='signal',sender='" + watcherName + "',interface='" + watcherIface + "'",
		"type='signal',sender='" + busName + "',interface='" + busName + "',member='NameOwnerChanged'",
	} {
		if err := c.AddMatch(rule); err != nil {
			return t.exit(ctx, fmt.Errorf("tray: add match: %w", err))
		}
	}
	if _, err := c.RequestName(t.host, zerobus.NameFlagDoNotQueue); err != nil {
		return t.exit(ctx, fmt.Errorf("tray: request host name: %w", err))
	}
	t.claim()
	if err := t.render(true); err != nil {
		return t.exit(ctx, err)
	}
	for t.err == nil {
		m, err := c.ReadMessage()
		if err != nil {
			return t.exit(ctx, err)
		}
		t.dispatch(m)
	}
	// A send fails too when ctx ends during dispatch and closes c.
	return t.exit(ctx, t.err)
}

// exit returns nil when the error comes from ctx ending.
func (t *Tray) exit(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (t *Tray) send() uint32 {
	serial, err := t.c.Send()
	if err != nil {
		if errors.Is(err, zerobus.ErrInvalid) {
			// A name or path an application gave us that D-Bus refuses.
			t.opt.Log.Warn("tray: message not sent", "err", err)
			return 0
		}
		t.fatal(err)
		return 0
	}
	return serial
}

func (t *Tray) fatal(err error) {
	if t.err == nil {
		t.err = err
	}
}

func (t *Tray) expect(serial uint32, kind pendKind, it *item) {
	if serial != 0 {
		t.pending = append(t.pending, pend{serial: serial, kind: kind, it: it})
	}
}

// claim asks for the watcher name; the reply decides the mode.
func (t *Tray) claim() {
	e := t.c.NewCall(busName, busPath, busName, "RequestName", "su")
	e.Str(watcherName)
	e.Uint32(zerobus.NameFlagDoNotQueue)
	t.expect(t.send(), pendName, nil)
}

func (t *Tray) dispatch(m *zerobus.Message) {
	switch m.Type {
	case zerobus.TypeMethodReturn, zerobus.TypeError:
		i := slices.IndexFunc(t.pending, func(p pend) bool { return p.serial == m.ReplySerial })
		if i < 0 {
			return
		}
		p := t.pending[i]
		t.pending = slices.Delete(t.pending, i, i+1)
		t.reply(p, m)
	case zerobus.TypeSignal:
		t.signal(m)
	case zerobus.TypeMethodCall:
		t.call(m)
	}
}

func (t *Tray) reply(p pend, m *zerobus.Message) {
	failed := m.Type == zerobus.TypeError
	switch p.kind {
	case pendName:
		code := m.Body().Uint32()
		if !failed && m.Signature == "u" && (code == zerobus.NamePrimaryOwner || code == zerobus.NameAlreadyOwner) {
			t.becomeWatcher()
			return
		}
		t.becomeHost()
	case pendItems:
		if failed {
			t.opt.Log.Warn("tray: the watcher did not list its items", "err", m.ErrorName)
			return
		}
		r := m.Body()
		if m.Signature != "v" || r.Variant() != "as" {
			t.opt.Log.Warn("tray: the watcher listed its items in an unknown form")
			return
		}
		end := r.Array('s')
		for r.More(end) {
			t.add(r.Str(), "")
		}
	case pendGetAll:
		it := p.it
		if it.pending != m.ReplySerial || !slices.Contains(t.items, it) {
			return // removed, or an older request
		}
		it.pending = 0
		if failed || m.Signature != "a{sv}" {
			t.opt.Log.Info("tray: item did not answer", "item", it.service, "err", m.ErrorName, "signature", m.Signature)
			if !it.loaded {
				t.remove(it)
			}
			return
		}
		if it.owner != m.Sender {
			it.owner = strings.Clone(m.Sender)
		}
		t.load(it, m.Body())
		if it.stale {
			t.refresh(it)
		}
	}
}

func (t *Tray) becomeWatcher() {
	t.watcher = true
	t.opt.Log.Info("tray: acting as the StatusNotifierWatcher")
	// Items that waited for a host register now.
	t.c.NewSignal(watcherPath, watcherIface, "StatusNotifierHostRegistered", "")
	t.send()
}

func (t *Tray) becomeHost() {
	t.watcher = false
	t.opt.Log.Info("tray: using the existing StatusNotifierWatcher")
	t.c.NewCall(watcherName, watcherPath, watcherIface, "RegisterStatusNotifierHost", "s").Str(t.host)
	t.expect(t.send(), pendIgnore, nil)
	e := t.c.NewCall(watcherName, watcherPath, propsIface, "Get", "ss")
	e.Str(watcherIface)
	e.Str("RegisteredStatusNotifierItems")
	t.expect(t.send(), pendItems, nil)
}

func (t *Tray) signal(m *zerobus.Message) {
	switch m.Interface {
	case busName:
		if m.Member != "NameOwnerChanged" || m.Sender != busName {
			return
		}
		r := m.Body()
		name, old, owner := r.Str(), r.Str(), r.Str()
		switch {
		case r.Err() != nil:
		case owner == "":
			t.vanished(name)
		case name == watcherName && old != "" && !t.watcher && owner != t.c.UniqueName():
			// Another watcher replaced the one we used: its items are
			// the ones to show now, not a late list from the old one.
			t.pending = slices.DeleteFunc(t.pending, func(p pend) bool { return p.kind == pendItems })
			t.dropAll()
			t.becomeHost()
		}
	case watcherIface:
		if t.watcher || m.Signature != "s" {
			return // our own signals come back to us too
		}
		s := m.Body().Str()
		switch m.Member {
		case "StatusNotifierItemRegistered":
			t.add(s, "")
		case "StatusNotifierItemUnregistered":
			svc, _, _ := normService(s)
			if i := slices.IndexFunc(t.items, func(it *item) bool { return it.service == svc }); i >= 0 {
				t.remove(t.items[i])
			}
		}
	case itemIface:
		switch m.Member {
		case "NewToolTip", "NewMenu", "NewOverlayIcon":
			return // nothing the tray shows
		}
		for _, it := range t.items {
			if it.owner == m.Sender && it.path == m.Path {
				t.refresh(it)
				return
			}
		}
	}
}

// vanished drops the items of a bus name that left, and takes over the
// watcher name when its owner left.
func (t *Tray) vanished(name string) {
	changed := false
	for i := len(t.items) - 1; i >= 0; i-- {
		if it := t.items[i]; it.owner == name || it.dest == name {
			t.drop(i)
			changed = true
		}
	}
	if changed {
		t.renderOrFail()
	}
	if name == watcherName && !t.watcher {
		// Items register again with the new watcher.
		t.dropAll()
		t.claim()
	}
}

func (t *Tray) dropAll() {
	if len(t.items) == 0 {
		return
	}
	for i := len(t.items) - 1; i >= 0; i-- {
		t.drop(i)
	}
	t.renderOrFail()
}

// normService turns the watcher's form of an item, "bus/path" or "bus", into
// "bus/path", its bus and its path.
func normService(s string) (service, bus, path string) {
	bus, p, ok := strings.Cut(s, "/")
	if !ok {
		return bus + itemPath, bus, itemPath
	}
	return s, bus, "/" + p
}

// validBusName reports whether s is a unique (":1.42") or well-known
// ("org.example.App") bus name, as D-Bus defines them.
func validBusName(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	unique := s[0] == ':'
	if unique {
		s = s[1:]
	}
	n := 0
	for more := true; more; n++ {
		var e string
		e, s, more = strings.Cut(s, ".")
		if e == "" || !unique && e[0] >= '0' && e[0] <= '9' {
			return false
		}
		for _, c := range []byte(e) {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
				return false
			}
		}
	}
	return n >= 2
}

// addResult says what add did.
type addResult uint8

const (
	addNew addResult = iota
	addKnown
	addInvalid // D-Bus refuses its bus name or path
)

// add starts tracking an item. service is the watcher's form, "bus/path" or
// "bus"; owner is its unique name when known.
func (t *Tray) add(service, owner string) addResult {
	service, dest, path := normService(service)
	if owner == "" && strings.HasPrefix(dest, ":") {
		owner = dest
	}
	for _, it := range t.items {
		if it.service == service || owner != "" && it.owner == owner && it.path == path {
			return addKnown
		}
	}
	it := &item{service: strings.Clone(service), dest: strings.Clone(dest), owner: strings.Clone(owner), path: strings.Clone(path)}
	t.items = append(t.items, it)
	t.refresh(it)
	if it.pending == 0 {
		// Never shown nor announced: forget it quietly.
		t.items = t.items[:len(t.items)-1]
		return addInvalid
	}
	return addNew
}

func (t *Tray) remove(it *item) {
	if i := slices.Index(t.items, it); i >= 0 {
		t.drop(i)
		t.renderOrFail()
	}
}

func (t *Tray) drop(i int) {
	it := t.items[i]
	t.items = slices.Delete(t.items, i, i+1)
	if t.watcher {
		t.c.NewSignal(watcherPath, watcherIface, "StatusNotifierItemUnregistered", "s").Str(it.service)
		t.send()
	}
}

// refresh asks an item for its properties, unless a request is in flight.
func (t *Tray) refresh(it *item) {
	if it.pending != 0 {
		it.stale = true
		return
	}
	it.stale = false
	dest := it.dest
	if it.owner != "" {
		dest = it.owner
	}
	t.c.NewCall(dest, it.path, propsIface, "GetAll", "s").Str(itemIface)
	it.pending = t.send()
	t.expect(it.pending, pendGetAll, it)
}

// load reads a GetAll reply into it and redraws when something shown
// changed. Strings are copied only when they change.
func (t *Tray) load(it *item, r *zerobus.Reader) {
	was := *it
	var pixmaps, attentionPixmaps []byte // the chosen pixmap of each kind
	end := r.Array('{')
	for r.More(end) {
		r.Struct()
		key := r.Str()
		sig := r.Variant()
		switch {
		case sig == "s" && key == "Id":
			setStr(&it.id, r.Str())
		case sig == "s" && key == "Title":
			setStr(&it.title, r.Str())
		case sig == "s" && key == "IconName":
			setStr(&it.iconName, r.Str())
		case sig == "o" && key == "Menu":
			setStr(&it.menu, r.ObjectPath())
		case sig == "b" && key == "ItemIsMenu":
			it.isMenu = r.Bool()
		case sig == "s" && key == "Status":
			it.status = parseStatus(r.Str())
		case sig == "a(iiay)" && key == "IconPixmap":
			pixmaps = pickPixmap(r)
		case sig == "a(iiay)" && key == "AttentionIconPixmap":
			attentionPixmaps = pickPixmap(r)
		default:
			r.Skip(sig)
		}
	}
	if r.Err() != nil {
		t.opt.Log.Warn("tray: bad properties", "item", it.service, "err", r.Err())
		*it = was
		return
	}
	it.loaded = true
	if it.status == attention && attentionPixmaps != nil {
		pixmaps = attentionPixmaps
	}
	it.color, it.hasColor = t.hist.dominant(pixmaps)
	if it.hasColor {
		it.color = readable(it.color, t.opt.Background, t.opt.Foreground)
	}
	if !it.resolved || it.id != was.id || it.iconName != was.iconName || it.title != was.title {
		it.icon, it.resolved = t.opt.Resolver.Resolve(it.id, it.iconName, it.title), true
	}
	t.renderOrFail()
}

// pickPixmap reads an a(iiay) value and returns the ARGB data of the pixmap
// best suited to find a color: the largest up to 64x64, else the smallest.
// The result points into the message buffer.
func pickPixmap(r *zerobus.Reader) []byte {
	var best []byte
	bestN := 0
	end := r.Array('(')
	for r.More(end) {
		r.Struct()
		w, h := int(r.Int32()), int(r.Int32())
		data := r.ByteArray()
		n := w * h
		if w <= 0 || h <= 0 || w > 1024 || h > 1024 || len(data) != 4*n {
			continue
		}
		if best == nil || better(n, bestN) {
			best, bestN = data, n
		}
	}
	return best
}

// better reports whether an icon of n pixels suits the color search better
// than one of cur pixels: the largest up to 64x64, else the smallest.
func better(n, cur int) bool {
	const enough = 64 * 64
	if cur > enough {
		return n < cur
	}
	return n > cur && n <= enough
}

func setStr(dst *string, s string) {
	if *dst != s {
		*dst = strings.Clone(s)
	}
}

func parseStatus(s string) status {
	switch s {
	case "Passive":
		return passive
	case "NeedsAttention":
		return attention
	}
	return active
}

// call answers method calls when this process is the watcher.
func (t *Tray) call(m *zerobus.Message) {
	noReply := m.Flags&zerobus.FlagNoReplyExpected != 0
	if !t.watcher || m.Path != watcherPath {
		if !noReply {
			t.c.NewError(m, "org.freedesktop.DBus.Error.UnknownObject", "s").Str("no such object")
			t.send()
		}
		return
	}
	switch {
	case m.Interface == watcherIface && m.Member == "RegisterStatusNotifierItem" && m.Signature == "s":
		s := m.Body().Str()
		if s == "" {
			t.errorReply(m, noReply, "org.freedesktop.DBus.Error.InvalidArgs", "empty service")
			return
		}
		if strings.HasPrefix(s, "/") {
			s = m.Sender + s // registered by object path
		} else if validBusName(s) {
			s += itemPath
		} else {
			t.errorReply(m, noReply, "org.freedesktop.DBus.Error.InvalidArgs", "invalid service")
			return
		}
		// add copies s before the next read, and sends only a GetAll.
		switch t.add(s, m.Sender) {
		case addInvalid:
			t.errorReply(m, noReply, "org.freedesktop.DBus.Error.InvalidArgs", "invalid service")
			return
		case addNew:
			t.c.NewSignal(watcherPath, watcherIface, "StatusNotifierItemRegistered", "s").Str(t.items[len(t.items)-1].service)
			t.send()
		}
		if !noReply {
			t.c.NewReply(m, "")
			t.send()
		}
	case m.Interface == watcherIface && m.Member == "RegisterStatusNotifierHost":
		if !noReply {
			t.c.NewReply(m, "")
			t.send()
		}
	case m.Interface == propsIface && m.Member == "Get" && m.Signature == "ss":
		r := m.Body()
		iface, prop := r.Str(), r.Str()
		if iface != watcherIface || !t.prop(m, prop, noReply) {
			t.errorReply(m, noReply, "org.freedesktop.DBus.Error.UnknownProperty", "no such property")
		}
	case m.Interface == propsIface && m.Member == "GetAll" && m.Signature == "s":
		if noReply {
			return
		}
		e := t.c.NewReply(m, "a{sv}")
		a := e.BeginArray('{')
		if m.Body().Str() == watcherIface {
			for _, p := range [...]string{"RegisteredStatusNotifierItems", "IsStatusNotifierHostRegistered", "ProtocolVersion"} {
				e.Struct()
				e.Str(p)
				t.propValue(e, p)
			}
		}
		e.EndArray(a)
		t.send()
	case m.Interface == "org.freedesktop.DBus.Introspectable" && m.Member == "Introspect":
		if !noReply {
			t.c.NewReply(m, "s").Str(introspection)
			t.send()
		}
	case m.Interface == "org.freedesktop.DBus.Peer" && m.Member == "Ping":
		if !noReply {
			t.c.NewReply(m, "")
			t.send()
		}
	default:
		t.errorReply(m, noReply, "org.freedesktop.DBus.Error.UnknownMethod", "no such method")
	}
}

func (t *Tray) errorReply(m *zerobus.Message, noReply bool, name, text string) {
	if !noReply {
		t.c.NewError(m, name, "s").Str(text)
		t.send()
	}
}

// prop answers Properties.Get; it reports false for an unknown property.
func (t *Tray) prop(m *zerobus.Message, name string, noReply bool) bool {
	switch name {
	case "RegisteredStatusNotifierItems", "IsStatusNotifierHostRegistered", "ProtocolVersion":
	default:
		return false
	}
	if !noReply {
		t.propValue(t.c.NewReply(m, "v"), name)
		t.send()
	}
	return true
}

func (t *Tray) propValue(e *zerobus.Encoder, name string) {
	switch name {
	case "RegisteredStatusNotifierItems":
		e.Variant("as")
		a := e.BeginArray('s')
		for _, it := range t.items {
			e.Str(it.service)
		}
		e.EndArray(a)
	case "IsStatusNotifierHostRegistered":
		e.Variant("b")
		e.Bool(true) // this process is the host
	case "ProtocolVersion":
		e.Variant("i")
		e.Int32(0)
	}
}

func (t *Tray) renderOrFail() {
	if err := t.render(false); err != nil {
		t.fatal(err)
	}
}

// render writes the line of visible items when it differs from the last one,
// and publishes where each icon sits for the control loop. The targets are
// compared too: an item may change its menu without changing its icon.
func (t *Tray) render(force bool) error {
	f := t.frame[:0]
	ts := t.targets[:0]
	col := 0
	for _, it := range t.items {
		if !it.loaded || it.status == passive || it.icon == "" {
			continue
		}
		if len(f) > 0 {
			f = append(f, ' ')
			col++
		}
		c, bold := t.opt.Foreground, false
		switch {
		case it.status == attention:
			c, bold = t.opt.Accent, true
		case it.hasColor:
			c = it.color
		}
		f = append(f, "\x1b["...)
		if bold {
			f = append(f, "1;"...)
		}
		f = append(f, "38;2;"...)
		f = strconv.AppendUint(f, uint64(c[0]), 10)
		f = append(f, ';')
		f = strconv.AppendUint(f, uint64(c[1]), 10)
		f = append(f, ';')
		f = strconv.AppendUint(f, uint64(c[2]), 10)
		f = append(f, 'm')
		f = append(f, it.icon...)
		f = append(f, "\x1b[0m"...)
		dest := it.dest
		if it.owner != "" {
			dest = it.owner
		}
		w := 0
		for _, r := range it.icon {
			w += vt.RuneWidth(r)
		}
		ts = append(ts, Target{Dest: dest, Path: it.path, Menu: it.menu, IsMenu: it.isMenu, Start: col, Width: w})
		col += w
	}
	f = append(f, '\n')
	t.frame, t.targets = f, ts
	sameFrame := bytes.Equal(f, t.last)
	if !force && sameFrame && t.sameTargets(ts) {
		return nil
	}
	t.snap.publish(ts) // before the line: whoever sees the line finds its targets
	if !force && sameFrame {
		return nil
	}
	t.frame, t.last = t.last, t.frame
	if _, err := t.opt.Out.Write(t.last); err != nil {
		return fmt.Errorf("tray: %w", err)
	}
	return nil
}

// sameTargets reports whether the published snapshot equals ts.
func (t *Tray) sameTargets(ts []Target) bool {
	t.snap.mu.Lock()
	defer t.snap.mu.Unlock()
	return slices.Equal(t.snap.targets, ts)
}

const introspection = `<!DOCTYPE node PUBLIC "-//freedesktop//DTD D-BUS Object Introspection 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/introspect.dtd">
<node>
 <interface name="org.kde.StatusNotifierWatcher">
  <method name="RegisterStatusNotifierItem"><arg name="service" type="s" direction="in"/></method>
  <method name="RegisterStatusNotifierHost"><arg name="service" type="s" direction="in"/></method>
  <property name="RegisteredStatusNotifierItems" type="as" access="read"/>
  <property name="IsStatusNotifierHostRegistered" type="b" access="read"/>
  <property name="ProtocolVersion" type="i" access="read"/>
  <signal name="StatusNotifierItemRegistered"><arg type="s"/></signal>
  <signal name="StatusNotifierItemUnregistered"><arg type="s"/></signal>
  <signal name="StatusNotifierHostRegistered"/>
 </interface>
 <interface name="org.freedesktop.DBus.Properties">
  <method name="Get"><arg type="s" direction="in"/><arg type="s" direction="in"/><arg type="v" direction="out"/></method>
  <method name="GetAll"><arg type="s" direction="in"/><arg type="a{sv}" direction="out"/></method>
 </interface>
 <interface name="org.freedesktop.DBus.Introspectable">
  <method name="Introspect"><arg type="s" direction="out"/></method>
 </interface>
</node>
`
