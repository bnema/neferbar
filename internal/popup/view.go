package popup

import (
	"strconv"
	"strings"

	"github.com/bnema/nefergui"

	"github.com/bnema/neferbar/internal/module"
)

// tooltipModel is what a tooltip shows.
type tooltipModel struct {
	title string
	lines []string   // the body, one entry per line; an empty one is a blank line
	rows  [][]string // the table
	cols  int        // cells of the longest row
}

func newTooltip(title, body string, rows [][]string) *tooltipModel {
	m := &tooltipModel{title: title, rows: rows}
	if body = strings.Trim(body, "\n"); body != "" {
		m.lines = strings.Split(body, "\n")
	}
	for _, r := range rows {
		m.cols = max(m.cols, len(r))
	}
	return m
}

// tooltipView draws the title in bold, the body, then the table. The table is
// laid out column by column, so every column is as wide as its widest cell
// and the cells of a row line up.
func tooltipView(f *nefergui.Frame, m *tooltipModel) {
	col := f.Root().Column()
	if m.title != "" {
		col.Text(m.title, nefergui.Class("title"), nefergui.Key("title"))
	}
	for i, l := range m.lines {
		if l == "" {
			l = " " // keeps the height of a line
		}
		col.Text(l, nefergui.Class("body"), nefergui.Key("b"+strconv.Itoa(i)))
	}
	if m.cols == 0 {
		return
	}
	table := col.Row(nefergui.Class("table"), nefergui.Key("table"))
	for c := 0; c < m.cols; c++ {
		class := "value"
		if c == 0 && m.cols > 1 {
			class = "key"
		}
		cells := table.Column(nefergui.Class(class), nefergui.Key("c"+strconv.Itoa(c)))
		for r, row := range m.rows {
			s := " "
			if c < len(row) && row[c] != "" {
				s = row[c]
			}
			cells.Text(s, nefergui.Key("r"+strconv.Itoa(r)))
		}
	}
}

// menuModel is a menu and where the user is in it. Submenus open in place:
// path holds the indexes of the submenus entered, and only the level at the
// end of the path is shown.
type menuModel struct {
	items  []module.MenuItem
	path   []int
	token  uint32
	chosen int32 // the id of the activated item, -1 for none
	// changed is set when the level changed: the popup must be resized.
	changed bool
	// scroll wraps the items in a scrolling box of scrollH pixels: they are
	// taller than the popup.
	scroll  bool
	scrollH int
}

// fitTo is the fallback when the popup could not be resized for a level that
// needs wantH pixels: if the surface keeps curH, the items scroll inside it.
func (m *menuModel) fitTo(wantH, curH int32) {
	if wantH > curH {
		m.scroll, m.scrollH = true, max(int(curH)-2*Inset, 1)
	}
}

func newMenu(items []module.MenuItem, token uint32) *menuModel {
	return &menuModel{items: items, token: token, chosen: -1}
}

// level is the items of the submenu the path leads to.
func (m *menuModel) level() []module.MenuItem {
	items := m.items
	for _, i := range m.path {
		if i < 0 || i >= len(items) {
			return nil
		}
		items = items[i].Items
	}
	return items
}

// inSubmenu reports whether the shown level is a submenu.
func (m *menuModel) inSubmenu() bool { return len(m.path) > 0 }

// push enters the submenu at index i of the shown level.
func (m *menuModel) push(i int) bool {
	lv := m.level()
	if i < 0 || i >= len(lv) || len(lv[i].Items) == 0 || len(m.path) >= module.MaxMenuDepth {
		return false
	}
	m.path = append(m.path, i)
	m.changed = true
	return true
}

// pop goes back to the parent level.
func (m *menuModel) pop() bool {
	if len(m.path) == 0 {
		return false
	}
	m.path = m.path[:len(m.path)-1]
	m.changed = true
	return true
}

// choose records the activation of a leaf item.
func (m *menuModel) choose(id int32) { m.chosen = id }

// levelKey prefixes the keys of a level, so a control keeps no hover or focus
// state from another level.
func (m *menuModel) levelKey() string {
	k := "l"
	for _, i := range m.path {
		k += "." + strconv.Itoa(i)
	}
	return k + "/"
}

// menuContentHeight is the height of the box that scrolls in a popup of
// MaxHeight: the popup height minus the stylesheet's padding and border.
const menuContentHeight = MaxHeight - 2*Inset

const (
	backLabel   = "‹ Back"
	submenuMark = "  ›"
)

// menuView draws the shown level. Activations happen here, while the view
// runs: Back and submenu entries move the path, other entries record the id.
func menuView(f *nefergui.Frame, m *menuModel) {
	parent := f.Root()
	if m.scroll {
		parent = parent.Scroll(nefergui.Key("scroll"), nefergui.Inline("height: "+strconv.Itoa(m.scrollH)+"px"))
	}
	col := parent.Column()
	if m.inSubmenu() {
		if col.Button(backLabel, nefergui.Key("back"), nefergui.Class("item")).Activated() {
			m.pop()
		}
	}
	prefix := m.levelKey()
	for i, it := range m.level() {
		key := nefergui.Key(prefix + strconv.Itoa(i))
		switch {
		case it.Kind == module.KindSeparator:
			col.Separator(key)
		case len(it.Items) > 0:
			if col.Button(it.Label+submenuMark, key, nefergui.Class("item"), nefergui.Disabled(!it.Enabled)).Activated() {
				m.push(i)
			}
		case it.Kind == module.KindCheck || it.Kind == module.KindRadio:
			checked := it.Checked
			if col.Checkbox(it.Label, &checked, key, nefergui.Class("item"), nefergui.Disabled(!it.Enabled)).Changed() {
				m.choose(it.ID)
			}
		default:
			if col.Button(it.Label, key, nefergui.Class("item"), nefergui.Disabled(!it.Enabled)).Activated() {
				m.choose(it.ID)
			}
		}
	}
}
