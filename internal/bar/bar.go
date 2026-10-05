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
	"syscall"

	"github.com/bnema/neferclient"

	"git.bnema.dev/bnema/neferbar/internal/config"
	"git.bnema.dev/bnema/neferbar/internal/glyph"
	"git.bnema.dev/bnema/neferbar/internal/gpu"
	"git.bnema.dev/bnema/neferbar/internal/layout"
	"git.bnema.dev/bnema/neferbar/internal/module"
	"git.bnema.dev/bnema/neferbar/internal/syncobj"
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
	regularPath, boldPath string
	face                  *glyph.Face
	facePx                float64

	dev  *gpu.Device
	node *syncobj.Node
	fb   *neferclient.Feedback
	rend *gpu.Renderer
	mod  gpu.Modifier

	haveAcquire bool
	frame       gpu.Frame
	watched     [gpu.SlotCount]int // watched eventfds, -1 when none

	mods   []*module.Module
	lay    *layout.Layout
	modCh  chan struct{}
	Stats  Stats
	fatal  error
	closed bool
}

// New builds a bar from a validated config.
// display names the Wayland socket; empty uses $WAYLAND_DISPLAY.
func New(cfg config.Config, log *slog.Logger, display string) (*Bar, error) {
	b := &Bar{cfg: cfg, log: log, display: display, modCh: make(chan struct{}, 1)}
	var err error
	if b.fg, err = config.ParseColor(cfg.Bar.Foreground); err != nil {
		return nil, err
	}
	if b.bg, err = config.ParseColor(cfg.Bar.Background); err != nil {
		return nil, err
	}
	if b.regularPath, err = glyph.FindFont(cfg.Bar.Font, false); err != nil {
		return nil, err
	}
	if b.boldPath, err = glyph.FindFont(cfg.Bar.Font, true); err != nil {
		b.boldPath = b.regularPath
	}
	for i := range b.watched {
		b.watched[i] = -1
	}
	zones := map[string]module.Zone{"left": module.Left, "center": module.Center, "right": module.Right}
	for _, m := range cfg.Module {
		b.mods = append(b.mods, module.New(m.Name, zones[m.Zone], m.Exec, b.modCh, log))
	}
	return b, nil
}

// Run connects to the compositor and serves until ctx ends or the surface is
// closed. It returns the first fatal error.
func (b *Bar) Run(ctx context.Context) (err error) {
	b.conn, err = neferclient.Connect(ctx, b.display)
	if err != nil {
		return connectError(b.display, err)
	}
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
	for _, m := range b.mods {
		go m.Run(runCtx)
	}
	for !b.closed {
		select {
		case <-ctx.Done():
			return nil
		case <-b.conn.Wake():
			if err = b.conn.Dispatch(b); err != nil {
				return fmt.Errorf("dispatch: %w", err)
			}
		case <-b.modCh:
			b.Stats.ModuleWake++
		}
		if b.fatal != nil {
			return b.fatal
		}
		if err = b.step(); err != nil {
			return err
		}
	}
	return nil
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

func (b *Bar) createSurface(h int32) error {
	surf, err := b.conn.NewLayerSurface(neferclient.LayerConfig{
		Output:        b.cfg.Bar.Output,
		Namespace:     "neferbar",
		Level:         neferclient.LayerTop,
		Anchors:       neferclient.AnchorTop | neferclient.AnchorLeft | neferclient.AnchorRight,
		ExclusiveZone: h,
		Height:        h,
		InputRects:    []neferclient.Rect{}, // read only: click-through
	})
	if err != nil {
		return fmt.Errorf("layer surface: %w", err)
	}
	b.surf, b.sid = surf, surf.ID()
	b.configured, b.canPresent = false, false
	return nil
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
	if !b.dirty || !b.canPresent || b.rend == nil {
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
	_, h, scale := b.surf.Size()
	px := b.cfg.Bar.Size * b.cfg.Bar.Scale * scale
	if b.face == nil || b.facePx != px {
		face, err := glyph.Load(b.regularPath, b.boldPath, px)
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
	if h != wantH {
		b.log.Info("recreating layer surface", "height", wantH, "scale", scale)
		b.dropRenderer()
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
	b.rend = rend
	b.Stats.Rebuilds++
	if b.lay == nil {
		b.lay = layout.New(rend.Cols(), b.fg, b.bg, b.mods)
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

// Handler methods. Events of a replaced surface are ignored.

// Configure marks the surface presentable and rebuilds as needed.
func (b *Bar) Configure(id neferclient.SurfaceID, _, _ int32) {
	if id != b.sid {
		return
	}
	if !b.configured {
		b.canPresent = true
	}
	b.configured = true
	b.reconcile()
}

// Scale follows the monitor's preferred scale.
func (b *Bar) Scale(id neferclient.SurfaceID, _ float64) {
	if id == b.sid {
		b.reconcile()
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
	if id == b.sid {
		b.canPresent = true
	}
}

// Closed ends the loop when the compositor closes the surface.
func (b *Bar) Closed(id neferclient.SurfaceID) {
	if id == b.sid {
		b.closed = true
	}
}

// FDReady hands a slot back once the compositor released it.
func (b *Bar) FDReady(id uint64) {
	if b.rend == nil {
		return
	}
	if err := b.rend.Released(int(id)); err != nil {
		b.fail(err)
	}
}

// Error logs non-fatal connection errors.
func (b *Bar) Error(err error) { b.log.Warn("wayland", "err", err) }

// Unused events.
func (b *Bar) OutputAdded(*neferclient.Output)           {}
func (b *Bar) OutputRemoved(uint32)                      {}
func (b *Bar) Locked()                                   {}
func (b *Bar) LockFinished()                             {}
func (b *Bar) Pointer(*neferclient.PointerEvent)         {}
func (b *Bar) Key(*neferclient.KeyEvent)                 {}
func (b *Bar) KeyboardFocus(neferclient.SurfaceID, bool) {}
func (b *Bar) SecretChanged(int)                         {}
