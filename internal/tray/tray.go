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
	icon                string // resolved glyph
	color               RGB
	hasColor            bool
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

	// Calls made from here on only go to the bus, which always answers.
	// Handle sees what arrives meanwhile.
	c.Handle = t.dispatch
	for _, rule := range [...]string{
		"type='signal',interface='" + itemIface + "'",
		"type='signal',interface='" + watcherIface + "'",
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
		return err
	}
	for t.err == nil {
		m, err := c.ReadMessage()
		if err != nil {
			return t.exit(ctx, err)
		}
		t.dispatch(m)
	}
	return t.err
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
		if !failed && (code == zerobus.NamePrimaryOwner || code == zerobus.NameAlreadyOwner) {
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
		if r.Variant() != "as" {
			return
		}
		end := r.Array('s')
		for r.More(end) {
			t.add(r.Str(), "", "")
		}
	case pendGetAll:
		it := p.it
		if it.pending != m.ReplySerial || !slices.Contains(t.items, it) {
			return // removed, or an older request
		}
		it.pending = 0
		if failed {
			t.opt.Log.Info("tray: item did not answer", "item", it.service, "err", m.ErrorName)
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
		name, _, owner := r.Str(), r.Str(), r.Str()
		if r.Err() != nil || owner != "" {
			return
		}
		t.vanished(name)
	case watcherIface:
		if t.watcher || m.Signature != "s" {
			return // our own signals come back to us too
		}
		s := m.Body().Str()
		switch m.Member {
		case "StatusNotifierItemRegistered":
			t.add(s, "", "")
		case "StatusNotifierItemUnregistered":
			if i := slices.IndexFunc(t.items, func(it *item) bool { return it.service == s }); i >= 0 {
				t.remove(t.items[i])
			}
		}
	case itemIface:
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
		for i := len(t.items) - 1; i >= 0; i-- {
			t.drop(i)
		}
		t.renderOrFail()
		t.claim()
	}
}

// add starts tracking an item. service is the watcher's form, "bus/path" or
// "bus"; owner is its unique name when known.
func (t *Tray) add(service, owner, path string) {
	if path == "" {
		bus, p, ok := strings.Cut(service, "/")
		if ok {
			path = "/" + p
		} else {
			path = itemPath
		}
		service = bus + path
		if owner == "" && strings.HasPrefix(bus, ":") {
			owner = bus
		}
	}
	dest, _, _ := strings.Cut(service, "/")
	for _, it := range t.items {
		if it.service == service || owner != "" && it.owner == owner && it.path == path {
			return
		}
	}
	it := &item{service: strings.Clone(service), dest: strings.Clone(dest), owner: strings.Clone(owner), path: strings.Clone(path)}
	t.items = append(t.items, it)
	t.refresh(it)
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
	if it.icon == "" || it.id != was.id || it.iconName != was.iconName || it.title != was.title {
		it.icon = t.opt.Resolver.Resolve(it.id, it.iconName, it.title)
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
		if !noReply {
			t.c.NewReply(m, "")
			t.send()
		}
		var owner, path string
		if strings.HasPrefix(s, "/") {
			owner, path, s = m.Sender, s, m.Sender+s // registered by object path
		} else {
			owner, path, s = m.Sender, itemPath, s+itemPath
		}
		n := len(t.items)
		t.add(s, owner, path)
		if len(t.items) > n {
			t.c.NewSignal(watcherPath, watcherIface, "StatusNotifierItemRegistered", "s").Str(t.items[n].service)
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

// render writes the line of visible items when it differs from the last one.
func (t *Tray) render(force bool) error {
	f := t.frame[:0]
	for _, it := range t.items {
		if !it.loaded || it.status == passive {
			continue
		}
		if len(f) > 0 {
			f = append(f, ' ')
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
	}
	f = append(f, '\n')
	t.frame = f
	if !force && bytes.Equal(f, t.last) {
		return nil
	}
	t.frame, t.last = t.last, t.frame
	if _, err := t.opt.Out.Write(t.last); err != nil {
		return fmt.Errorf("tray: %w", err)
	}
	return nil
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
