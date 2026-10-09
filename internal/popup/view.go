package popup

import (
	"strconv"

	"github.com/bnema/nefergui"

	"github.com/bnema/neferbar/internal/module"
)

// tooltipModel is what a tooltip shows.
type tooltipModel struct {
	title string
	lines []string // the body, one entry per non-empty line
}

func newTooltip(title, body string) *tooltipModel {
	m := &tooltipModel{title: title}
	start := 0
	for i := 0; i <= len(body); i++ {
		if i == len(body) || body[i] == '\n' {
			if i > start {
				m.lines = append(m.lines, body[start:i])
			}
			start = i + 1
		}
	}
	return m
}

// tooltipView draws the title in bold, then the body.
func tooltipView(f *nefergui.Frame, m *tooltipModel) {
	col := f.Root().Column()
	if m.title != "" {
		col.Text(m.title, nefergui.Class("title"), nefergui.Key("title"))
	}
	for i, l := range m.lines {
		col.Text(l, nefergui.Class("body"), nefergui.Key("b"+strconv.Itoa(i)))
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
	// scroll wraps the items in a scrolling box: they are taller than a popup.
	scroll bool
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

// menuContentHeight is the height of the box that scrolls: the popup height
// minus the stylesheet's padding and border.
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
		parent = parent.Scroll(nefergui.Key("scroll"), nefergui.Inline("height: "+strconv.Itoa(menuContentHeight)+"px"))
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
