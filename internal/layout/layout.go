// Package layout turns the ANSI frames of modules into one row of cells.
package layout

import (
	vt "github.com/bnema/vev-vt"

	"github.com/bnema/neferbar/internal/glyph"
	"github.com/bnema/neferbar/internal/gpu"
	"github.com/bnema/neferbar/internal/module"
)

// Source is one module and the cells parsed from its latest frame.
type Source struct {
	M *module.Module

	screen *vt.Screen
	frame  []byte // raw ANSI of the latest frame
	input  []byte // reset + erase + frame, reused for each Write
	cells  []gpu.Cell
	failed bool
}

// Layout owns the sources and the composed row.
type Layout struct {
	cols    int
	fg, bg  [3]uint8
	pal     [16][3]uint8
	sources []*Source
	row     []gpu.Cell
}

// New builds a layout for a row of cols cells.
func New(cols int, fg, bg [3]uint8, pal [16][3]uint8, mods []*module.Module) *Layout {
	l := &Layout{cols: cols, fg: fg, bg: bg, pal: pal, row: make([]gpu.Cell, cols)}
	l.SetModules(mods)
	return l
}

func newSource(m *module.Module, cols int) *Source {
	s := &Source{
		M:      m,
		screen: vt.NewScreen(cols, 1),
		frame:  make([]byte, 0, module.MaxFrame),
		input:  make([]byte, 0, module.MaxFrame+16),
		cells:  make([]gpu.Cell, 0, cols),
	}
	s.screen.Write([]byte("\x1b[?7l")) // no autowrap: long text clips
	return s
}

// SetModules replaces the module list. A module that is already shown keeps
// its parsed text; new ones start empty. The order of mods is the order
// within each zone.
func (l *Layout) SetModules(mods []*module.Module) {
	old := make(map[*module.Module]*Source, len(l.sources))
	for _, s := range l.sources {
		old[s.M] = s
	}
	l.sources = make([]*Source, 0, len(mods))
	for _, m := range mods {
		if s, ok := old[m]; ok {
			l.sources = append(l.sources, s)
		} else {
			l.sources = append(l.sources, newSource(m, l.cols))
		}
	}
}

// SetColors changes the default colors and the 16-color palette, and reparses
// every module's last frame.
func (l *Layout) SetColors(fg, bg [3]uint8, pal [16][3]uint8) {
	l.fg, l.bg, l.pal = fg, bg, pal
	for _, s := range l.sources {
		if s.failed {
			s.cells = l.marker(s.cells[:0], s.M.Name)
		} else if len(s.frame) > 0 {
			s.parse(fg, bg, l.pal)
		}
	}
}

// Resize changes the row width and reparses the last frame of every module.
func (l *Layout) Resize(cols int) {
	l.cols = cols
	l.row = make([]gpu.Cell, cols)
	for _, s := range l.sources {
		s.screen = vt.NewScreen(cols, 1)
		s.screen.Write([]byte("\x1b[?7l"))
		s.cells = make([]gpu.Cell, 0, cols)
		if s.failed {
			s.cells = l.marker(s.cells, s.M.Name)
		} else if len(s.frame) > 0 {
			s.parse(l.fg, l.bg, l.pal)
		}
	}
}

// Update pulls new frames from every module and reparses the ones that
// changed. It reports whether the composed row may have changed.
func (l *Layout) Update() bool {
	changed := false
	for _, s := range l.sources {
		var ch, failed bool
		s.frame, ch, failed = s.M.Take(s.frame)
		if !ch {
			continue
		}
		changed = true
		s.failed = failed
		if failed {
			s.cells = l.marker(s.cells[:0], s.M.Name)
			continue
		}
		s.parse(l.fg, l.bg, l.pal)
	}
	return changed
}

// marker writes a short red "[name!]" for a module whose script is down.
func (l *Layout) marker(dst []gpu.Cell, name string) []gpu.Cell {
	red := [3]uint8{0xf3, 0x8b, 0xa8}
	add := func(r rune) { dst = append(dst, gpu.Cell{Rune: r, FG: red, BG: l.bg}) }
	add('[')
	for _, r := range name {
		add(r)
	}
	add('!')
	add(']')
	return dst
}

func (s *Source) parse(fg, bg [3]uint8, pal [16][3]uint8) {
	s.input = append(s.input[:0], "\x1b[0m\r\x1b[2K"...)
	s.input = append(s.input, s.frame...)
	// A frame that ends inside an escape sequence (a script bug, or the frame
	// cap cutting one) would leave the parser waiting, and it would swallow the
	// next frames. ESC \ is the string terminator: it closes any such sequence,
	// and is harmless after a complete one.
	s.input = append(s.input, "\x1b\\"...)
	s.screen.Write(s.input)
	cols := s.screen.Columns()
	// Trim trailing blank default cells so modules do not claim empty space.
	end := cols
	for end > 0 {
		c := s.screen.Cell(end-1, 0)
		if (c.Rune == ' ' || c.Rune == 0) && c.Style.Background < 0 && !c.Style.HasBackgroundRGB && !c.Style.Inverse {
			end--
			continue
		}
		break
	}
	s.cells = s.cells[:0]
	for x := 0; x < end; x++ {
		s.cells = append(s.cells, convert(s.screen.Cell(x, 0), fg, bg, &pal))
	}
}

func convert(c vt.Cell, defFG, defBG [3]uint8, pal *[16][3]uint8) gpu.Cell {
	st := &c.Style
	fg, bg := defFG, defBG
	switch {
	case st.HasForegroundRGB:
		fg = [3]uint8{st.ForegroundRGB.R, st.ForegroundRGB.G, st.ForegroundRGB.B}
	case st.Foreground >= 0:
		fg = palette(st.Foreground, pal)
	}
	switch {
	case st.HasBackgroundRGB:
		bg = [3]uint8{st.BackgroundRGB.R, st.BackgroundRGB.G, st.BackgroundRGB.B}
	case st.Background >= 0:
		bg = palette(st.Background, pal)
	}
	if st.Inverse {
		fg, bg = bg, fg
	}
	if st.Attrs&vt.AttrDim != 0 {
		for i := range fg {
			fg[i] = uint8((int(fg[i])*2 + int(bg[i])) / 3)
		}
	}
	r := c.Rune
	if r == 0 || c.Continuation {
		r = ' '
	}
	var style glyph.Style
	if st.Bold {
		style |= glyph.Bold
	}
	if st.Italic {
		style |= glyph.Italic
	}
	if st.Attrs&vt.AttrUnderline != 0 || st.UnderlineStyle != vt.UnderlineNone {
		style |= glyph.Underline
	}
	if st.Attrs&vt.AttrStrikethrough != 0 {
		style |= glyph.Strike
	}
	return gpu.Cell{Rune: r, FG: fg, BG: bg, Style: style}
}

// Compose lays the zones out in one row: left from the left edge, right to
// the right edge, center in the gap between them. Overlap is cut from the
// center first, then from the left.
func (l *Layout) Compose() []gpu.Cell {
	blank := gpu.Cell{Rune: ' ', FG: l.fg, BG: l.bg}
	for i := range l.row {
		l.row[i] = blank
	}
	var left, center, right int
	for _, s := range l.sources {
		n := len(s.cells)
		switch s.M.Zone {
		case module.Left:
			left += n
		case module.Center:
			center += n
		default:
			right += n
		}
	}
	right = min(right, l.cols)
	left = min(left, l.cols-right)
	// Right zone, packed against the right edge.
	x := l.cols - right
	for _, s := range l.sources {
		if s.M.Zone == module.Right {
			x += copy(l.row[x:], s.cells[:min(len(s.cells), l.cols-x)])
		}
	}
	// Left zone from the left edge, clipped to its budget.
	x, limit := 0, left
	for _, s := range l.sources {
		if s.M.Zone == module.Left && x < limit {
			x += copy(l.row[x:limit], s.cells)
		}
	}
	// Center zone: on the middle of the bar, and only as far from it as the
	// left and right zones force. Centering it in the space they leave would
	// shift it whenever the two zones differ in width.
	gap := l.cols - right - left
	if center = min(center, gap); center > 0 {
		start := (l.cols - center) / 2          // exactly on the middle
		start = max(start, left)                // not over the left zone
		start = min(start, l.cols-right-center) // not over the right zone
		x, limit = start, start+center
		for _, s := range l.sources {
			if s.M.Zone == module.Center && x < limit {
				x += copy(l.row[x:limit], s.cells)
			}
		}
	}
	return l.row
}
