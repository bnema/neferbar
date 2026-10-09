package module

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"
)

// Limits of a control line. ParseControl refuses what exceeds them.
const (
	// MaxMenuDepth is how many levels of submenus a menu may nest.
	MaxMenuDepth = 8
	// MaxMenuItems is how many items a whole menu may hold, submenus included.
	MaxMenuItems = 512
	// MaxLabel is the longest item label or tooltip title, in bytes; longer
	// ones are cut at a rune boundary.
	MaxLabel = 256
	// MaxText is the longest tooltip body, in bytes; longer ones are cut at a
	// rune boundary.
	MaxText = 1024
	// MaxRows and MaxCells bound the table of a tooltip; a cell is cut like a
	// label.
	MaxRows  = 32
	MaxCells = 4
	// maxColumn bounds the column and width of a control line.
	maxColumn = 1 << 20
)

// Control types.
const (
	ControlTooltip = "tooltip"
	ControlMenu    = "menu"
	ControlClose   = "close"
)

// Menu item kinds.
const (
	KindNormal    = "normal"
	KindSeparator = "separator"
	KindCheck     = "check"
	KindRadio     = "radio"
)

// Control is one parsed control line of a script.
type Control struct {
	Type  string
	Col   int // first cell of the anchor inside the module
	Width int // cells of the anchor
	// Title and Body are the text of a tooltip, Rows its table: one entry
	// per row, one string per cell.
	Title, Body string
	Rows        [][]string
	// Click is the token of the press a menu answers.
	Click uint32
	Items []MenuItem
}

// MenuItem is one entry of a menu.
type MenuItem struct {
	ID      int32
	Label   string
	Kind    string
	Enabled bool
	Checked bool
	Items   []MenuItem // a submenu
}

type wireControl struct {
	Type  string     `json:"type"`
	Col   int        `json:"col"`
	Width int        `json:"width"`
	Title string     `json:"title"`
	Body  string     `json:"body"`
	Rows  [][]string `json:"rows"`
	Click uint32     `json:"click"`
	Items []wireItem `json:"items"`
}

type wireItem struct {
	ID      int32      `json:"id"`
	Label   string     `json:"label"`
	Kind    string     `json:"kind"`
	Enabled *bool      `json:"enabled"`
	Checked bool       `json:"checked"`
	Items   []wireItem `json:"items"`
}

// ParseControl reads the JSON of a control line (see TakeControl) and checks
// every limit. Control characters are removed from the texts, labels are cut
// to MaxLabel bytes, and an item without "enabled" is enabled.
func ParseControl(b []byte) (Control, error) {
	if len(b) > MaxControl {
		return Control{}, fmt.Errorf("control line of %d bytes exceeds %d", len(b), MaxControl)
	}
	var w wireControl
	if err := json.Unmarshal(b, &w); err != nil {
		return Control{}, fmt.Errorf("control line: %w", err)
	}
	if w.Col < 0 || w.Col > maxColumn || w.Width < 0 || w.Width > maxColumn {
		return Control{}, errors.New("control line: column or width out of range")
	}
	c := Control{Type: w.Type, Col: w.Col, Width: w.Width}
	switch w.Type {
	case ControlClose:
		return c, nil
	case ControlTooltip:
		if w.Width == 0 {
			return Control{}, errors.New("tooltip: width must be positive")
		}
		c.Title = clean(w.Title, MaxLabel, false)
		c.Body = clean(w.Body, MaxText, true)
		if len(w.Rows) > MaxRows {
			return Control{}, fmt.Errorf("tooltip: more than %d rows", MaxRows)
		}
		for _, row := range w.Rows {
			if len(row) > MaxCells {
				return Control{}, fmt.Errorf("tooltip: more than %d cells in a row", MaxCells)
			}
			cells := make([]string, len(row))
			for i, s := range row {
				cells[i] = clean(s, MaxLabel, false)
			}
			c.Rows = append(c.Rows, cells)
		}
		if c.Title == "" && c.Body == "" && len(c.Rows) == 0 {
			return Control{}, errors.New("tooltip: no text")
		}
		return c, nil
	case ControlMenu:
		if w.Width == 0 {
			return Control{}, errors.New("menu: width must be positive")
		}
		c.Click = w.Click
		count := 0
		items, err := convertItems(w.Items, 1, &count)
		if err != nil {
			return Control{}, fmt.Errorf("menu: %w", err)
		}
		if len(items) == 0 {
			return Control{}, errors.New("menu: no items")
		}
		c.Items = items
		return c, nil
	}
	return Control{}, fmt.Errorf("control line: unknown type %q", w.Type)
}

func convertItems(in []wireItem, depth int, count *int) ([]MenuItem, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if depth > MaxMenuDepth {
		return nil, fmt.Errorf("more than %d levels", MaxMenuDepth)
	}
	out := make([]MenuItem, 0, len(in))
	for i := range in {
		if *count++; *count > MaxMenuItems {
			return nil, fmt.Errorf("more than %d items", MaxMenuItems)
		}
		w := &in[i]
		it := MenuItem{ID: w.ID, Label: clean(w.Label, MaxLabel, false), Kind: w.Kind, Enabled: w.Enabled == nil || *w.Enabled, Checked: w.Checked}
		if w.ID < 0 {
			return nil, fmt.Errorf("item %d: negative id", w.ID)
		}
		switch w.Kind {
		case "":
			it.Kind = KindNormal
		case KindNormal, KindSeparator, KindCheck, KindRadio:
		default:
			return nil, fmt.Errorf("item %d: unknown kind %q", w.ID, w.Kind)
		}
		sub, err := convertItems(w.Items, depth+1, count)
		if err != nil {
			return nil, err
		}
		it.Items = sub
		out = append(out, it)
	}
	return out, nil
}

// clean drops control characters (and anything Unicode calls a format or
// unassigned control, such as bidi overrides), keeps newlines when keepNL, and
// cuts the result to max bytes at a rune boundary.
func clean(s string, max int, keepNL bool) string {
	needs := len(s) > max
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			needs = true
			break
		}
	}
	if !needs {
		return s
	}
	out := make([]byte, 0, min(len(s), max))
	for _, r := range s {
		switch {
		case r == '\n' && keepNL:
		case r == utf8.RuneError, unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			continue
		}
		if len(out)+utf8.RuneLen(r) > max {
			break
		}
		out = utf8.AppendRune(out, r)
	}
	return string(out)
}
