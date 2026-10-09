// Package popup draws the tooltips and menus that interactive modules ask for
// with control lines (see module.ParseControl). A Host owns at most one popup:
// an xdg_popup on the bar's layer surface, drawn by a nefergui renderer of its
// own.
package popup

import (
	"math"

	"github.com/bnema/neferclient"
)

// Size limits of a popup, in logical pixels.
const (
	MaxMenuWidth    = 480
	MaxTooltipWidth = 360
	// MaxHeight is the tallest popup; a taller menu scrolls.
	MaxHeight = 600
)

// CellRect is the logical-pixel rectangle of width cells starting at cell
// start of a row of cellW-physical-pixel cells on a surface of the given
// scale, rounded outward, height pixels high.
func CellRect(start, width, cellW int, scale float64, height int32) neferclient.Rect {
	const eps = 1e-9
	x0 := int32(math.Floor(float64(start)*float64(cellW)/scale + eps))
	x1 := int32(math.Ceil(float64(start+width)*float64(cellW)/scale - eps))
	return neferclient.Rect{X: x0, Y: 0, Width: x1 - x0, Height: height}
}

// Anchor is the rectangle a popup attaches to: the cells col .. col+width-1
// of a module whose shown cells start at spanStart and number spanWidth. The
// request is clamped to the shown cells. It reports false when col lies
// outside them.
func Anchor(spanStart, spanWidth, col, width, cellW int, scale float64, height int32) (neferclient.Rect, bool) {
	if col < 0 || col >= spanWidth || width <= 0 || cellW <= 0 || !(scale > 0) {
		return neferclient.Rect{}, false
	}
	width = min(width, spanWidth-col)
	return CellRect(spanStart+col, width, cellW, scale, height), true
}

// Edge is the anchor edge and the gravity that put a popup on the free side of
// a bar: below a bar at the top, above a bar at the bottom.
func Edge(bottomBar bool) neferclient.PopupEdge {
	if bottomBar {
		return neferclient.EdgeTop
	}
	return neferclient.EdgeBottom
}

// Clamp rounds a measured size up to whole pixels and limits the height to
// MaxHeight; scroll reports that the content is taller than that.
func Clamp(w, h float64) (cw, ch int32, scroll bool) {
	cw = int32(math.Ceil(max(w, 1)))
	if h > MaxHeight {
		return cw, MaxHeight, true
	}
	return cw, int32(math.Ceil(max(h, 1))), false
}

// Placement is where a popup of the given size goes for an anchor.
func Placement(anchor neferclient.Rect, bottomBar bool, w, h int32) neferclient.PopupPlacement {
	e := Edge(bottomBar)
	return neferclient.PopupPlacement{
		Anchor: anchor, Edge: e, Gravity: e,
		Adjust: neferclient.AdjustSlideX | neferclient.AdjustFlipY | neferclient.AdjustResizeY,
		Width:  w, Height: h,
	}
}
