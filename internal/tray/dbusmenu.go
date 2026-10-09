package tray

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/bnema/zerobus"
)

const (
	menuIface = "com.canonical.dbusmenu"

	// The limits of the menu the bar accepts (module.ParseControl).
	maxMenuDepth = 8
	maxMenuItems = 512
	maxMenuLabel = 256
	// maxMenuLine keeps the control line under the bar's 64 KiB, with room
	// for the fields around the items.
	maxMenuLine = 60 << 10

	layoutSig = "(ia{sv}av)"

	// noMenuPath is what some toolkits put in Menu to say "no menu".
	noMenuPath = "/NO_DBUSMENU"
)

// menuItem is one entry of the menu sent to the bar.
type menuItem struct {
	ID      int32      `json:"id"`
	Label   string     `json:"label"`
	Kind    string     `json:"kind"`
	Enabled bool       `json:"enabled"`
	Checked bool       `json:"checked"`
	Items   []menuItem `json:"items,omitempty"`
}

// menuLine is the control line that opens a menu.
type menuLine struct {
	Type  string     `json:"type"`
	Col   int        `json:"col"`
	Width int        `json:"width"`
	Click uint32     `json:"click"`
	Items []menuItem `json:"items"`
}

// lastMenu is the menu the bar was last given: what a menu-activate or
// menu-closed line may refer to.
type lastMenu struct {
	token uint32
	dest  string
	path  string
	ids   []int32 // the ids the bar can choose from, sorted
}

// layoutDecoder reads the reply of GetLayout.
type layoutDecoder struct {
	r     *zerobus.Reader
	count int
	ids   []int32
}

// decodeLayout reads a GetLayout reply body, "u(ia{sv}av)": the top level
// items, and the ids among them. Invisible items are dropped; the tree is
// cut at maxMenuDepth levels and maxMenuItems items. Children whose signature
// is not (ia{sv}av) are skipped.
func decodeLayout(r *zerobus.Reader) (items []menuItem, ids []int32, err error) {
	r.Uint32() // revision
	d := &layoutDecoder{r: r}
	root, _ := d.node(0)
	if err = r.Err(); err != nil {
		return nil, nil, err
	}
	slices.Sort(d.ids)
	return root.Items, d.ids, nil
}

// node reads one (ia{sv}av) struct at the given depth (the root is 0). It
// reports false for an item that is not kept: invisible, or over a limit.
func (d *layoutDecoder) node(depth int) (menuItem, bool) {
	r := d.r
	r.Struct()
	it := menuItem{ID: r.Int32(), Kind: "normal", Enabled: true}
	visible := true
	toggle := ""
	checked := false
	end := r.Array('{')
	for r.More(end) {
		r.Struct()
		key := r.Str()
		sig := r.Variant()
		switch {
		case key == "label" && sig == "s":
			it.Label = cleanLabel(r.Str())
		case key == "enabled" && sig == "b":
			it.Enabled = r.Bool()
		case key == "visible" && sig == "b":
			visible = r.Bool()
		case key == "type" && sig == "s":
			if r.Str() == "separator" {
				it.Kind = "separator"
			}
		case key == "toggle-type" && sig == "s":
			toggle = r.Str()
		case key == "toggle-state" && sig == "i":
			checked = r.Int32() == 1
		default:
			r.Skip(sig)
		}
	}
	if it.Kind != "separator" {
		switch toggle {
		case "checkmark":
			it.Kind = "check"
		case "radio":
			it.Kind = "radio"
		}
		it.Checked = checked && it.Kind != "normal"
	}
	kids := r.Array('v')
	keep := depth == 0 || (visible && it.ID >= 0 && d.count < maxMenuItems)
	if depth > 0 && keep {
		d.count++
		d.ids = append(d.ids, it.ID)
	}
	if !keep || depth >= maxMenuDepth {
		r.SkipTo(kids)
		return it, keep
	}
	for r.More(kids) {
		sig := r.Variant()
		if sig != layoutSig {
			r.Skip(sig)
			continue
		}
		if child, ok := d.node(depth + 1); ok {
			it.Items = append(it.Items, child)
		}
	}
	return it, true
}

// cleanLabel strips the mnemonic markers of a dbusmenu label: "_x" is "x" and
// "__" is "_". The control characters are the bar's to remove.
func cleanLabel(s string) string {
	if !strings.Contains(s, "_") {
		return cutRunes(s, maxMenuLabel)
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '_' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) {
			i++
			b.WriteByte(s[i]) // "_x" and "__" both keep the second byte
		}
	}
	return cutRunes(b.String(), maxMenuLabel)
}

// encodeMenu builds the control line for a menu. When the JSON would exceed
// the bar's limit, items are dropped from the end of the menu (in reading
// order) until it fits.
func encodeMenu(l menuLine) ([]byte, []int32, error) {
	total := countItems(l.Items)
	for {
		b, err := json.Marshal(l)
		if err != nil {
			return nil, nil, err
		}
		if len(b) <= maxMenuLine {
			var ids []int32
			collectIDs(l.Items, &ids)
			slices.Sort(ids)
			return b, ids, nil
		}
		if total <= 1 {
			return nil, nil, errors.New("tray: the menu is too large")
		}
		total = total * 3 / 4
		keep := total
		l.Items = keepFirst(l.Items, &keep)
	}
}

func countItems(items []menuItem) int {
	n := len(items)
	for i := range items {
		n += countItems(items[i].Items)
	}
	return n
}

func collectIDs(items []menuItem, ids *[]int32) {
	for i := range items {
		*ids = append(*ids, items[i].ID)
		collectIDs(items[i].Items, ids)
	}
}

// keepFirst keeps the first *n items in reading order, parents before their
// children, and returns the trimmed list.
func keepFirst(items []menuItem, n *int) []menuItem {
	var out []menuItem
	for _, it := range items {
		if *n <= 0 {
			break
		}
		*n--
		it.Items = keepFirst(it.Items, n)
		out = append(out, it)
	}
	return out
}
