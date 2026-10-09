// Package bar connects the Wayland layer surface, the Vulkan renderer and the
// module layout.
//
// Everything runs on one goroutine: the loop in Run waits for a Wayland
// wake-up or a module frame, calls Dispatch, and every Handler method,
// renderer call and Present happens inside it. Modules only publish frames
// through a channel.
package bar

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/bnema/neferclient"

	"github.com/bnema/neferbar/internal/config"
	"github.com/bnema/neferbar/internal/fswatch"
	"github.com/bnema/neferbar/internal/glyph"
	"github.com/bnema/neferbar/internal/gpu"
	"github.com/bnema/neferbar/internal/layout"
	"github.com/bnema/neferbar/internal/module"
	"github.com/bnema/neferbar/internal/popup"
	"github.com/bnema/neferbar/internal/syncobj"
	"github.com/bnema/neferbar/internal/theme"
)

const acquireTimelineID = 1 << 32

// Stats counts what the bar did; read it only from the loop goroutine or after
// Run returns.
type Stats struct {
	Presented  uint64 // frames handed to the compositor
	Rebuilds   uint64 // renderer rebuilds (scale or size changes)
	Recreated  uint64 // layer surface recreations
	NoSlot     uint64 // draws skipped because every image was held
	Dirty      uint64 // module wake-ups that changed the row
	ModuleWake uint64
}

// Bar is the running application.
type Bar struct {
	cfg     config.Config
	log     *slog.Logger
	display string
	fg, bg  [3]uint8

	conn *neferclient.Conn
	surf *neferclient.Surface
	sid  neferclient.SurfaceID

	configured bool
	canPresent bool
	dirty      bool

	// font
	fonts  glyph.Paths
	face   *glyph.Face
	facePx float64

	dev  *gpu.Device
	node *syncobj.Node
	fb   *neferclient.Feedback
	rend *gpu.Renderer
	mod  gpu.Modifier

	haveAcquire bool
	frame       gpu.Frame
	watched     [gpu.SlotCount]int // watched eventfds, -1 when none

	runners   []runner
	mods      []*module.Module // the modules of runners, in config order
	runCtx    context.Context
	cancelRun context.CancelFunc
	lay       *layout.Layout
	modCh     chan struct{}
	cfgPath   string
	cfgCh     chan config.Config

	// scriptCh carries the directory of a script that changed on disk.
	scriptCh     chan string
	scriptCancel context.CancelFunc
	stale        map[string]bool // directories whose modules restart at the next syncModules

	theme       theme.Theme
	themeCh     chan struct{}
	themeCancel context.CancelFunc
	pal         [16][3]uint8
	env         []string // theme colors for the scripts

	// settling is true between a scale or size change and the moment both the
	// new scale and the new logical width have arrived. They come as separate
	// events, in either order; building a renderer from half of them presents a
	// buffer of the wrong size, and some compositors keep that size.
	settling bool
	settleC  <-chan time.Time

	// fresh is true from the moment a layer surface is created until a renderer
	// exists for it. A new surface reports scale 1 until the compositor says
	// otherwise, so its first geometry is not trusted until the events settle.
	fresh bool
	// asked is the last (height, scale) a surface was recreated for. Asking for
	// the same pair twice means the compositor will not give that height, and
	// the bar takes what it gets instead of looping.
	askedH     int32
	askedScale float64

	// mappedW is the logical width the current layer surface was first drawn at.
	// It resets to 0 when the surface is recreated.
	mappedW int32

	ptr   pointerState // pointer events turned into module lines
	input inputRegion

	pop     *popup.Host // the tooltip or menu being shown, if any
	popC    <-chan time.Time
	popCSS  string // the stylesheet last written for the popups
	warmed  bool
	spanBuf []layout.Span
	badCtl  map[*module.Module]bool // modules whose invalid control line was logged
	tips    tooltipLimiter
	ctlBuf  []module.Control // the control lines of one wake-up, reused

	Stats  Stats
	fatal  error
	closed bool
}

// New builds a bar from a validated config. cfgPath is the file the config came
// from: relative theme paths are read from its directory, and the bar reloads
// it when it changes (empty: no reload). display names the Wayland socket;
// empty uses $WAYLAND_DISPLAY.
func New(cfg config.Config, cfgPath string, log *slog.Logger, display string) (*Bar, error) {
	b := &Bar{cfg: cfg, cfgPath: cfgPath, log: log, display: display, modCh: make(chan struct{}, 1), cfgCh: make(chan config.Config, 1),
		themeCh: make(chan struct{}, 1), scriptCh: make(chan string, 16)}
	var err error
	if err = b.loadTheme(); err != nil {
		return nil, err
	}
	if b.fonts, err = findFonts(cfg.Bar.Font, log); err != nil {
		return nil, err
	}
	for i := range b.watched {
		b.watched[i] = -1
	}
	return b, nil
}

// barColors picks the bar's own colors: the settings win, otherwise the theme
// foreground, and a background one step toward the foreground so the bar
// stands out from a terminal window of the same theme.
func barColors(cfg config.Config, th theme.Theme) (fg, bg [3]uint8) {
	fg, bg = th.Foreground, theme.Mix(th.Background, th.Foreground, 0.07)
	if c, err := config.ParseColor(cfg.Bar.Foreground); err == nil && cfg.Bar.Foreground != "" {
		fg = c
	}
	if c, err := config.ParseColor(cfg.Bar.Background); err == nil && cfg.Bar.Background != "" {
		bg = c
	}
	return fg, bg
}

// loadTheme resolves bar.theme and applies it. When the theme cannot be read
// the built-in colors are used, and the problem is logged.
func (b *Bar) loadTheme() error {
	base := filepath.Dir(b.cfgPath)
	if b.cfgPath == "" {
		base = "."
	}
	th, err := theme.Resolve(b.cfg.Bar.Theme, base, theme.SystemEnv())
	if err != nil {
		b.log.Warn("using the built-in colors: "+err.Error(), "theme", b.cfg.Bar.Theme)
	} else {
		b.log.Info("theme", "source", th.Source)
	}
	b.setTheme(th)
	return nil
}

// setTheme makes th the colors of the bar and of its scripts.
func (b *Bar) setTheme(th theme.Theme) {
	b.theme = th
	b.fg, b.bg = barColors(b.cfg, th)
	b.pal = th.Palette
	b.env = th.EnvVars(b.bg, b.fg, b.cfg.Bar.Accent)
	if b.lay != nil {
		b.lay.SetColors(b.fg, b.bg, b.pal)
	}
	b.dirty = true
	b.watchTheme()
	b.updatePopupStyle()
}

// watchTheme watches every file the theme was read from, so changing the
// terminal's theme changes the bar.
func (b *Bar) watchTheme() {
	if b.themeCancel != nil {
		b.themeCancel()
	}
	if b.runCtx == nil {
		return // Run has not started; it calls this again
	}
	ctx, cancel := context.WithCancel(b.runCtx)
	b.themeCancel = cancel
	for _, f := range b.theme.Files {
		go func() {
			if err := fswatch.Watch(ctx, f, 150*time.Millisecond, b.themeCh); err != nil {
				b.log.Warn("cannot watch a theme file; edits to it need a restart", "file", f, "err", err)
			}
		}()
	}
}

// watchScripts watches the directory of every module's script, so editing a
// script, or a helper it sources from the same directory, restarts the modules
// that live there. It runs again whenever the set of modules changes.
func (b *Bar) watchScripts() {
	if b.scriptCancel != nil {
		b.scriptCancel()
	}
	if b.runCtx == nil {
		return // Run has not started; it calls syncModules, which calls this
	}
	ctx, cancel := context.WithCancel(b.runCtx)
	b.scriptCancel = cancel
	seen := map[string]bool{}
	for _, r := range b.runners {
		dir := module.ScriptDir(r.cfg.Exec)
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		go func() {
			changed := make(chan struct{}, 1)
			go func() {
				if err := fswatch.WatchDir(ctx, dir, 150*time.Millisecond, changed); err != nil {
					b.log.Warn("cannot watch a script directory; edits to its scripts need a restart", "dir", dir, "err", err)
				}
			}()
			for {
				select {
				case <-ctx.Done():
					return
				case <-changed:
					select {
					case b.scriptCh <- dir:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
}

// restartScripts restarts the modules whose script is in dir.
func (b *Bar) restartScripts(dir string) {
	b.log.Info("script changed; restarting its modules", "dir", dir)
	if b.stale == nil {
		b.stale = map[string]bool{}
	}
	b.stale[dir] = true
	b.syncModules(b.cfg.Module)
}

// reloadTheme re-reads the theme after one of its files changed.
func (b *Bar) reloadTheme() {
	base := filepath.Dir(b.cfgPath)
	if b.cfgPath == "" {
		base = "."
	}
	th, err := theme.Resolve(b.cfg.Bar.Theme, base, theme.SystemEnv())
	if err != nil {
		b.log.Warn("theme reload failed; keeping the current colors", "err", err)
		return
	}
	if th.Equal(b.theme) {
		// Same colors, but the files that make them up may have changed.
		if !slices.Equal(th.Files, b.theme.Files) {
			b.theme.Files = th.Files
			b.watchTheme()
		}
		return
	}
	b.log.Info("theme changed", "source", th.Source)
	b.setTheme(th)
	b.syncModules(b.cfg.Module) // scripts get the new colors in their environment
}

// runner is one started module and how to stop it.
type runner struct {
	cfg    config.Module
	env    []string
	m      *module.Module
	cancel context.CancelFunc
}

var zones = map[string]module.Zone{"left": module.Left, "center": module.Center, "right": module.Right}

// syncModules makes the running modules match want. A module with the same
// name, zone and command keeps running; every other one is stopped, and a new
// or changed one is started.
func (b *Bar) syncModules(want []config.Module) {
	old := make(map[string]runner, len(b.runners))
	for _, r := range b.runners {
		old[r.cfg.Name] = r
	}
	next := make([]runner, 0, len(want))
	for _, w := range want {
		if r, ok := old[w.Name]; ok {
			delete(old, w.Name)
			if r.cfg == w && slices.Equal(r.env, b.env) && !b.stale[module.ScriptDir(w.Exec)] {
				next = append(next, r)
				continue
			}
			r.cancel()
		}
		ctx, cancel := context.WithCancel(b.runCtx)
		m := module.New(w.Name, zones[w.Zone], w.Exec, b.modCh, b.log)
		m.Env = b.env
		m.Interactive = w.Interactive
		m.Start(ctx)
		next = append(next, runner{cfg: w, env: b.env, m: m, cancel: cancel})
	}
	for _, r := range old {
		r.cancel()
	}
	b.runners = next
	if o := b.pop.Owner(); o != nil && !slices.ContainsFunc(next, func(r runner) bool { return r.m == o }) {
		b.closePopup() // its module was stopped or replaced
	}
	clear(b.stale)
	b.watchScripts()
	b.mods = b.mods[:0]
	for _, r := range next {
		b.mods = append(b.mods, r.m)
	}
	b.forgetModules(b.mods)
	if b.ptr.hover != nil && !slices.Contains(b.mods, b.ptr.hover) {
		b.ptr.hover, b.ptr.hoverOff = nil, 0 // the hovered module was stopped
	}
	if b.lay != nil {
		b.lay.SetModules(b.mods)
	}
}

// applyConfig switches to next. Nothing changes if it cannot be applied.
func (b *Bar) applyConfig(next config.Config) {
	old := b.cfg
	var err error
	fontChanged := next.Bar.Font != old.Bar.Font
	fonts := b.fonts
	if fontChanged {
		if fonts, err = findFonts(next.Bar.Font, b.log); err != nil {
			b.log.Warn("config not applied", "err", err)
			return
		}
	}
	if next.Bar.Output != old.Bar.Output {
		b.log.Warn("bar.output changed: restart the bar to move it", "from", old.Bar.Output, "to", next.Bar.Output)
		next.Bar.Output = old.Bar.Output
	}
	b.closePopup() // it was built from the old settings
	b.cfg = next
	b.fonts = fonts
	if err = b.loadTheme(); err != nil { // the theme setting or the colors may have changed
		b.log.Warn("theme not applied", "err", err)
	}
	b.syncModules(next.Module)
	b.dirty = true
	if next.Bar.Position != old.Bar.Position && b.surf != nil {
		// The edge is part of how a layer surface is created: make a new one.
		b.log.Info("moving the bar", "to", next.Bar.Position)
		b.dropRenderer()
		_, h, _ := b.surf.Size()
		b.closePopup()
		if err = b.surf.Close(); err != nil {
			b.fail(err)
			return
		}
		if err = b.createSurface(h); err != nil {
			b.fail(err)
			return
		}
		b.Stats.Recreated++
	}
	if fontChanged || next.Bar.Size != old.Bar.Size || next.Bar.Scale != old.Bar.Scale {
		b.facePx = -1 // reconcile rebuilds the font, and the surface if its height changed
		b.reconcile()
	}
	b.log.Info("config reloaded")
}

// Run connects to the compositor and serves until ctx ends or the surface is
// closed. It returns the first fatal error.
func (b *Bar) Run(ctx context.Context) (err error) {
	b.conn, err = neferclient.Connect(ctx, b.display)
	if err != nil {
		return connectError(b.display, err)
	}
	b.pop = popup.NewHost(b.conn, b.log)
	b.updatePopupStyle()
	defer func() {
		err = errors.Join(err, b.shutdown())
		b.log.Info("stats", "presented", b.Stats.Presented, "rebuilds", b.Stats.Rebuilds,
			"recreated", b.Stats.Recreated, "no_slot", b.Stats.NoSlot, "dirty", b.Stats.Dirty, "module_wakes", b.Stats.ModuleWake)
	}()
	if err = b.createSurface(estimateHeight(b.cfg.Bar.Size * b.cfg.Bar.Scale)); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	b.runCtx, b.cancelRun = runCtx, cancel
	b.watchTheme()
	b.syncModules(b.cfg.Module)
	if b.cfgPath != "" {
		go func() {
			if err := config.Watch(runCtx, b.cfgPath, b.cfg, b.cfgCh, b.log); err != nil {
				b.log.Warn("config reload is off", "err", err)
			}
		}()
	}
	for !b.closed {
		select {
		case <-ctx.Done():
			return nil
		case <-b.conn.Wake():
			if err = b.conn.Dispatch(b); err != nil {
				return fmt.Errorf("dispatch: %w", err)
			}
		case <-b.pop.Wake(): // the popup renderer wants a frame
		case <-b.popC: // a popup frame was waiting for the GPU
		case <-b.modCh:
			b.Stats.ModuleWake++
		case next := <-b.cfgCh:
			b.applyConfig(next)
		case <-b.themeCh:
			b.reloadTheme()
		case dir := <-b.scriptCh:
			b.restartScripts(dir)
		case <-b.settleC:
			b.settleC, b.settling = nil, false
			b.reconcile()
		}
		if b.fatal != nil {
			return b.fatal
		}
		if err = b.step(); err != nil {
			return err
		}
		b.stepPopup()
	}
	return nil
}

// findFonts resolves the four font files of a family and warns when
// fontconfig had to substitute another family.
func findFonts(family string, log *slog.Logger) (glyph.Paths, error) {
	p, err := glyph.FindFonts(family)
	if err != nil {
		return p, err
	}
	if got := glyph.FamilyOf(family); got != "" && !strings.EqualFold(got, family) && !strings.HasPrefix(strings.ToLower(family), strings.ToLower(got)) {
		log.Warn("font not found, fontconfig substituted another family: icons may be missing", "wanted", family, "got", got, "file", p[0])
	}
	return p, nil
}

// socketPath mirrors how neferclient resolves the socket, for error messages.
func socketPath(display string) string {
	if display == "" {
		display = os.Getenv("WAYLAND_DISPLAY")
	}
	if display == "" {
		display = "wayland-0"
	}
	if filepath.IsAbs(display) {
		return display
	}
	return filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), display)
}

// connectError turns a failed dial into one actionable sentence.
func connectError(display string, err error) error {
	path := socketPath(display)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("no Wayland socket at %s: is the compositor running? Set WAYLAND_DISPLAY, NEFERBAR_DISPLAY or -display", path)
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("the Wayland socket %s exists but refuses connections: stale socket or compositor not ready", path)
	case errors.Is(err, os.ErrPermission):
		return fmt.Errorf("not allowed to open the Wayland socket %s", path)
	}
	return err
}

// estimateHeight is the first request, before the real cell height is known.
func estimateHeight(px float64) int32 { return int32(math.Ceil(px * 1.5)) }

// edge is the layer-shell anchor for a bar.position value.
func edge(position string) neferclient.Anchor {
	if position == "bottom" {
		return neferclient.AnchorBottom
	}
	return neferclient.AnchorTop
}

func (b *Bar) createSurface(h int32) error {
	surf, err := b.conn.NewLayerSurface(neferclient.LayerConfig{
		Output:        b.cfg.Bar.Output,
		Namespace:     "neferbar",
		Level:         neferclient.LayerTop,
		Anchors:       edge(b.cfg.Bar.Position) | neferclient.AnchorLeft | neferclient.AnchorRight,
		ExclusiveZone: h,
		Height:        h,
		InputRects:    []neferclient.Rect{}, // read only: click-through
	})
	if err != nil {
		return fmt.Errorf("layer surface: %w", err)
	}
	b.surf, b.sid = surf, surf.ID()
	b.mappedW = 0
	b.resetInput()
	b.fresh = true
	b.configured, b.canPresent = false, false
	return nil
}

// resetInput forgets the input state of the surface being replaced: the new
// surface starts click-through, and the module the pointer was over is told it
// left, since no leave will come from a surface that is gone.
func (b *Bar) resetInput() {
	b.input.reset()
	b.ptr.leave()
}

// step reparses changed modules and draws when something is ready.
func (b *Bar) step() error {
	if b.lay != nil && b.lay.Update() {
		b.dirty = true
		b.Stats.Dirty++
	}
	return b.draw()
}

func (b *Bar) draw() error {
	if !b.dirty || !b.canPresent || b.rend == nil || b.settling {
		return nil
	}
	ok, err := b.rend.Draw(b.lay.Compose(), b.bg, &b.frame)
	if err != nil {
		return fmt.Errorf("draw: %w", err)
	}
	if !ok {
		b.Stats.NoSlot++ // Released retries
		return nil
	}
	// The spans come from the row just drawn, and the input region follows
	// them only when the draw succeeded: between a layout change and the next
	// successful draw, the compositor may still route input by the older spans.
	b.syncInputRegion()
	if err = b.present(); err != nil {
		return err
	}
	b.dirty, b.canPresent = false, false
	b.Stats.Presented++
	return nil
}

func (b *Bar) present() error {
	f := &b.frame
	if f.NewBuffer {
		img := f.Image
		buf := neferclient.Buffer{
			Width: img.Width, Height: img.Height, FourCC: gpu.DRMFormatXRGB8888, Modifier: img.Modifier, PlaneCount: 1,
		}
		buf.Planes[0] = neferclient.Plane{FD: img.FD, Offset: img.Offset, Stride: img.Stride}
		if err := b.surf.ImportBuffer(uint64(f.Slot), &buf); err != nil {
			return fmt.Errorf("import buffer: %w", err)
		}
		if err := b.surf.ImportTimeline(uint64(f.Slot), f.ReleaseTimeline); err != nil {
			return fmt.Errorf("import release timeline: %w", err)
		}
		if err := b.conn.WatchFD(f.ReleaseEventFD, uint64(f.Slot)); err != nil {
			return fmt.Errorf("watch release fd: %w", err)
		}
		b.watched[f.Slot] = f.ReleaseEventFD
	}
	if !b.haveAcquire {
		if err := b.surf.ImportTimeline(acquireTimelineID, b.rend.AcquireTimelineFD()); err != nil {
			return fmt.Errorf("import acquire timeline: %w", err)
		}
		b.haveAcquire = true
	}
	err := b.surf.Present(&neferclient.Present{
		Buffer:          uint64(f.Slot),
		AcquireTimeline: acquireTimelineID,
		ReleaseTimeline: uint64(f.Slot),
		AcquirePoint:    f.AcquirePoint,
		ReleasePoint:    f.ReleasePoint,
		Opaque:          true,
	})
	if err != nil {
		return fmt.Errorf("present: %w", err)
	}
	return nil
}

// reconcile brings the font, the layer surface height and the renderer in
// line with the current scale and feedback. It runs on every event that can
// change them and does nothing when everything already matches.
func (b *Bar) reconcile() {
	if !b.configured || b.fb == nil {
		return
	}
	if b.dev == nil {
		dev, err := gpu.Open(b.fb.MainDevice)
		if err != nil {
			b.fail(err)
			return
		}
		node, err := syncobj.Open(filepath.Join("/dev/dri", fmt.Sprintf("renderD%d", dev.RenderMinor)))
		if err != nil {
			dev.Close()
			b.fail(err)
			return
		}
		b.dev, b.node = dev, node
	}
	w, h, scale := b.surf.Size()
	px := b.cfg.Bar.Size * b.cfg.Bar.Scale * scale
	if b.face == nil || b.facePx != px {
		face, err := glyph.Load(b.fonts, px)
		if err != nil {
			b.fail(err)
			return
		}
		b.dropRenderer()
		b.face.Close()
		b.face, b.facePx = face, px
	}
	// The bar is one cell high: logical height = ceil(cell / scale).
	wantH := int32(math.Ceil(float64(b.face.CellH)/scale - 1e-9))
	if h != wantH && b.askedH == wantH && b.askedScale == scale {
		// Already asked for exactly this and got something else: the compositor
		// clamps the height. Use what we have rather than asking forever.
		b.log.Warn("the compositor gave another bar height than asked; using it", "asked", wantH, "got", h)
		wantH = h
	}
	if h != wantH {
		b.askedH, b.askedScale = wantH, scale
		b.log.Info("recreating layer surface", "height", wantH, "scale", scale)
		b.dropRenderer()
		b.closePopup()
		if err := b.surf.Close(); err != nil {
			b.fail(err)
			return
		}
		if err := b.createSurface(wantH); err != nil {
			b.fail(err)
			return
		}
		b.Stats.Recreated++
		return
	}
	pw, ph, err := b.surf.PhysicalSize()
	if err != nil {
		b.fail(err)
		return
	}
	if b.rend != nil && b.rend.W == pw && b.rend.H == ph {
		return
	}
	if b.mappedW != 0 && w != b.mappedW {
		// The logical width changed (a scale change on the output). NeferWL
		// places a layer surface from the size it had when it was mapped and
		// does not re-read it when only the buffer and viewport change, so the
		// old width would stay: centered in a narrower output, the left part cut
		// off. A new surface is mapped fresh and placed from its real size.
		b.log.Info("recreating layer surface: the output width changed", "from", b.mappedW, "to", w)
		b.dropRenderer()
		b.closePopup()
		if err := b.surf.Close(); err != nil {
			b.fail(err)
			return
		}
		if err := b.createSurface(h); err != nil {
			b.fail(err)
			return
		}
		b.Stats.Recreated++
		return
	}
	b.dropRenderer()
	if mod, err := b.chooseModifier(); err != nil {
		b.fail(err)
		return
	} else {
		b.mod = mod
	}
	rend, err := gpu.NewRenderer(b.dev, b.node, b.mod, b.face, pw, ph)
	if err != nil {
		b.fail(err)
		return
	}
	b.rend, b.mappedW = rend, w
	b.fresh = false
	b.Stats.Rebuilds++
	if b.lay == nil {
		b.lay = layout.New(rend.Cols(), b.fg, b.bg, b.pal, b.mods)
	} else {
		b.lay.Resize(rend.Cols())
	}
	b.lay.Update()
	b.dirty = true
	b.log.Info("renderer ready", "px", pw, "py", ph, "scale", scale, "cell", fmt.Sprintf("%dx%d", b.face.CellW, b.face.CellH), "cols", rend.Cols())
}

func (b *Bar) chooseModifier() (gpu.Modifier, error) {
	supported, err := b.dev.ExportableModifiers()
	if err != nil {
		return 0, err
	}
	pairs := make([]gpu.FormatPair, len(b.fb.Formats))
	for i, f := range b.fb.Formats {
		pairs[i] = gpu.FormatPair{FourCC: f.FourCC, Modifier: f.Modifier}
	}
	return gpu.ChooseModifier(pairs, supported)
}

// dropRenderer retires the renderer and every wl object that refers to its
// images.
func (b *Bar) dropRenderer() {
	if b.rend == nil {
		return
	}
	for i, fd := range b.watched {
		if fd >= 0 {
			_ = b.conn.UnwatchFD(fd)
			b.watched[i] = -1
		}
	}
	if err := b.surf.DestroyImports(); err != nil {
		b.log.Warn("destroy imports", "err", err)
	}
	b.haveAcquire = false
	b.rend.Close()
	b.rend = nil
}

func (b *Bar) fail(err error) {
	if b.fatal == nil {
		b.fatal = err
	}
}

func (b *Bar) shutdown() error {
	var errs []error
	// Stop the scripts first: cancel asks them to end, Wait gives them time to,
	// so none outlives the bar.
	if b.cancelRun != nil {
		b.cancelRun()
	}
	for _, r := range b.runners {
		r.cancel()
	}
	for _, r := range b.runners {
		r.m.Wait(3 * time.Second)
	}
	b.closePopup()
	// The warm-up renderer must be gone before the GPU state is, but a stuck
	// GPU library must not hold the shutdown.
	if !b.pop.WaitTimeout(warmupWait) {
		b.log.Warn("the popup warm-up did not end; shutting down anyway")
	}
	popup.RemoveCSS()
	b.dropRenderer()
	b.face.Close()
	if b.node != nil {
		errs = append(errs, b.node.Close())
	}
	b.dev.Close()
	if b.conn != nil {
		errs = append(errs, b.conn.Close())
	}
	return errors.Join(errs...)
}

// settleDelay is how long the bar waits for the second half of a scale or size
// change before it rebuilds.
const settleDelay = 60 * time.Millisecond

// warmupWait is how long shutdown waits for the popup warm-up.
const warmupWait = 2 * time.Second

// geometryChanged is called when the compositor reports a new scale or size.
// The first report builds at once, because there is nothing on screen yet.
// Later ones wait for the dust to settle, and nothing is drawn meanwhile.
func (b *Bar) geometryChanged() {
	if b.rend == nil && !b.fresh {
		b.reconcile()
		return
	}
	b.settling = true
	b.settleC = time.After(settleDelay)
}

// Handler methods. Events of a replaced surface are ignored.

// Configure marks the surface presentable and rebuilds as needed.
func (b *Bar) Configure(id neferclient.SurfaceID, _, _ int32) {
	if b.pop.Is(id) {
		b.pop.Configure()
		return
	}
	if id != b.sid {
		return
	}
	if !b.configured {
		b.canPresent = true
	}
	b.configured = true
	b.geometryChanged()
}

// Scale follows the monitor's preferred scale.
func (b *Bar) Scale(id neferclient.SurfaceID, _ float64) {
	if b.pop.Is(id) {
		b.pop.Scale()
		return
	}
	if id == b.sid {
		b.geometryChanged()
	}
}

// FeedbackDone records the compositor's formats; a changed main device is not
// followed.
func (b *Bar) FeedbackDone(id neferclient.SurfaceID) {
	if id != b.sid {
		return
	}
	fb := b.surf.Feedback()
	if b.fb != nil && b.fb.MainDevice != fb.MainDevice {
		b.log.Warn("dmabuf main device changed; restart the bar to follow it")
		return
	}
	if b.fb.Equal(fb) {
		return
	}
	b.fb = fb.Clone()
	b.dropRenderer() // formats changed: images must be rebuilt
	b.reconcile()
}

// Frame allows the next Present.
func (b *Bar) Frame(id neferclient.SurfaceID) {
	if b.pop.Is(id) {
		b.pop.Frame()
		return
	}
	if id == b.sid {
		b.canPresent = true
	}
}

// PopupDone is called when the compositor dismisses a popup of the bar: an
// outside click, a denied grab. A menu tells its script.
func (b *Bar) PopupDone(id neferclient.SurfaceID) {
	if b.pop.Is(id) {
		b.pop.PopupDone()
	}
}

// Key goes to the popup that has the keyboard.
func (b *Bar) Key(ev *neferclient.KeyEvent) {
	if b.pop.Is(ev.Surface) {
		b.pop.Key(ev)
	}
}

// KeyboardFocus tells a popup whether it has the keyboard.
func (b *Bar) KeyboardFocus(id neferclient.SurfaceID, focused bool) {
	if b.pop.Is(id) {
		b.pop.KeyboardFocus(focused)
	}
}

// Closed ends the loop when the compositor closes the surface.
func (b *Bar) Closed(id neferclient.SurfaceID) {
	if b.pop.Is(id) {
		b.closePopup()
		return
	}
	if id == b.sid {
		b.closed = true
	}
}

// FDReady hands a slot back once the compositor released it.
func (b *Bar) FDReady(id uint64) {
	if id >= popup.FDBase {
		b.pop.FDReady(id) // the release descriptor of a popup buffer
		return
	}
	if b.rend == nil {
		return
	}
	if err := b.rend.Released(int(id)); err != nil {
		b.fail(err)
	}
}

// syncInputRegion makes the part of the bar that takes pointer input the cells
// of the interactive modules; the rest stays click-through.
func (b *Bar) syncInputRegion() {
	_, h, scale := b.surf.Size()
	rects, changed := b.input.update(b.lay, inputKey{cellW: b.face.CellW, scale: scale, height: h})
	if !changed {
		return
	}
	if err := b.surf.SetInputRegion(rects); err != nil {
		b.log.Warn("cannot set the input region", "err", err)
		b.input.reset() // the compositor kept the old one: the next draw tries again
	}
}

// Pointer turns pointer events over the bar into lines for interactive modules.
func (b *Bar) Pointer(ev *neferclient.PointerEvent) {
	if b.pop.Is(ev.Surface) {
		b.pop.Pointer(ev)
		return
	}
	if ev.Surface != b.sid || b.lay == nil || b.face == nil {
		return
	}
	_, _, scale := b.surf.Size()
	b.handlePointer(ev, scale, b.face.CellW)
}

// handlePointer is Pointer for an event over the bar surface.
func (b *Bar) handlePointer(ev *neferclient.PointerEvent, scale float64, cellW int) {
	col := cellColumn(ev.X, scale, cellW)
	switch ev.Kind {
	case neferclient.PointerEnter, neferclient.PointerMotion:
		before := b.ptr.hover
		b.ptr.motion(b.lay, col)
		if b.ptr.hover != before {
			b.closeTooltip() // the pointer moved to another module
		}
	case neferclient.PointerLeave:
		b.ptr.leave()
		b.closeTooltip()
	case neferclient.PointerButton:
		if ev.Pressed {
			b.closePopup() // a press on the bar ends a tooltip or a menu
			b.ptr.button(b.lay, col, ev.Button, ev.Serial)
		}
	case neferclient.PointerAxis:
		delta := ev.DY
		if ev.Axis == neferclient.AxisHorizontal {
			delta = ev.DX
		}
		b.ptr.axis(b.lay, col, ev.Axis, ev.Value120, delta)
	case neferclient.PointerAxisStop:
		b.ptr.wheel.stop(ev.Axis)
	}
}

// Error logs non-fatal connection errors.
func (b *Bar) Error(err error) { b.log.Warn("wayland", "err", err) }

// Unused events.
func (b *Bar) OutputAdded(*neferclient.Output) {}
func (b *Bar) OutputRemoved(uint32)            {}
func (b *Bar) Locked()                         {}
func (b *Bar) LockFinished()                   {}
func (b *Bar) SecretChanged(int)               {}
