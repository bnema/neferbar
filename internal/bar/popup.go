package bar

import (
	"time"

	"github.com/bnema/neferbar/internal/layout"
	"github.com/bnema/neferbar/internal/module"
	"github.com/bnema/neferbar/internal/popup"
)

// popupPoll is how long the loop waits before it asks again for a popup frame
// that is waiting for the GPU.
const popupPoll = 2 * time.Millisecond

// updatePopupStyle writes the stylesheet of the popups when the bar's colors,
// font or size changed. Without a stylesheet no popup opens.
func (b *Bar) updatePopupStyle() {
	if b.pop == nil {
		return
	}
	css := popup.CSS(popup.Style{
		Font: b.cfg.Bar.Font, Size: b.cfg.Bar.Size * b.cfg.Bar.Scale,
		FG: b.fg, BG: b.bg, Accent: b.pal[b.cfg.Bar.Accent&15],
	})
	if css == b.popCSS {
		return
	}
	path, err := popup.WriteCSS(css)
	if err != nil {
		b.log.Warn("popups are off: cannot write their stylesheet", "err", err)
		b.pop.SetStyles("")
		b.popCSS = ""
		return
	}
	b.popCSS = css
	b.pop.SetStyles(path)
}

// closePopup dismisses the tooltip or menu being shown.
func (b *Bar) closePopup() { b.pop.Close() }

// closeTooltip dismisses the popup when it is a tooltip; a menu stays.
func (b *Bar) closeTooltip() {
	if b.pop.Kind() == popup.KindTooltip {
		b.pop.Close()
	}
}

// stepPopup runs after every wake-up of the loop: it starts the GPU warm-up,
// carries out what the modules asked for, and draws the popup.
func (b *Bar) stepPopup() {
	if b.pop == nil {
		return
	}
	b.popC = nil
	if !b.warmed && b.fb != nil && b.popCSS != "" && b.anyInteractive() {
		b.warmed = true
		b.pop.Warm(b.fb)
	}
	for _, m := range b.mods {
		for {
			raw, ok := m.TakeControl()
			if !ok {
				break
			}
			b.control(m, raw)
		}
	}
	if b.pop.OwnerRestarted() {
		b.closePopup()
	}
	if !b.pop.Active() {
		return
	}
	b.pop.Draw()
	if b.pop.Pending() {
		b.popC = time.After(popupPoll)
	}
}

func (b *Bar) anyInteractive() bool {
	for _, m := range b.mods {
		if m.Interactive {
			return true
		}
	}
	return false
}

// control handles one control line of module m.
func (b *Bar) control(m *module.Module, raw []byte) {
	ctrl, err := module.ParseControl(raw)
	if err != nil {
		if !b.badCtl[m] {
			if b.badCtl == nil {
				b.badCtl = map[*module.Module]bool{}
			}
			b.badCtl[m] = true
			b.log.Warn("module sent an invalid control line; ignored (shown once per module)", "module", m.Name, "err", err)
		}
		return
	}
	if !m.Interactive {
		return // only modules that asked for the pointer may open popups
	}
	switch ctrl.Type {
	case module.ControlClose:
		if b.pop.Kind() == popup.KindTooltip && b.pop.Owner() == m {
			b.pop.Close()
		}
	case module.ControlTooltip:
		// Only for the module under the pointer, and never over a menu.
		if m != b.ptr.hover || b.pop.Kind() == popup.KindMenu {
			return
		}
		b.openPopup(m, popup.KindTooltip, ctrl, 0)
	case module.ControlMenu:
		serial, ok := b.ptr.takeMenuPress(m, ctrl.Click, time.Now())
		if !ok {
			b.log.Debug("menu dropped: it does not answer the latest press", "module", m.Name, "click", ctrl.Click)
			return
		}
		b.openPopup(m, popup.KindMenu, ctrl, serial)
	}
}

// openPopup shows ctrl as a popup of module m, anchored to the cells it names.
func (b *Bar) openPopup(m *module.Module, kind popup.Kind, ctrl module.Control, serial uint32) {
	if !b.configured || b.fb == nil || b.face == nil || b.lay == nil {
		return
	}
	b.spanBuf = b.lay.Spans(b.spanBuf[:0])
	var span layout.Span
	for _, s := range b.spanBuf {
		if s.M == m {
			span = s
		}
	}
	if span.M == nil {
		return // the module shows nothing now
	}
	_, h, scale := b.surf.Size()
	anchor, ok := popup.Anchor(span.Start, span.Width, ctrl.Col, ctrl.Width, b.face.CellW, scale, h)
	if !ok {
		b.log.Debug("popup dropped: its column is outside the module", "module", m.Name, "col", ctrl.Col)
		return
	}
	err := b.pop.Open(popup.Request{
		Kind: kind, Owner: m, Ctrl: ctrl, Anchor: anchor, Parent: b.surf,
		BottomBar: b.cfg.Bar.Position == "bottom", Feedback: b.fb, Serial: serial,
	})
	if err != nil {
		b.log.Warn("cannot open a popup", "module", m.Name, "err", err)
	}
}
