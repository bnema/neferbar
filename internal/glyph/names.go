package glyph

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"

	"golang.org/x/image/font/sfnt"
	"golang.org/x/sys/unix"
)

// Names maps the glyph names stored in a font file to the runes that draw
// them, such as "fa-steam" to U+F1B6 in a Nerd Font. Only the private use
// areas are indexed: that is where icon fonts put their glyphs.
//
// The names stay in the mapped font file; a lookup does not allocate.
type Names struct {
	data []byte
	idx  []nameEntry // sorted by name
}

type nameEntry struct {
	off, n uint32 // the name is data[off:off+n]
	r      rune
}

// iconRanges are the private use areas: the BMP one and plane 15.
var iconRanges = [...][2]rune{{0xE000, 0xF8FF}, {0xF0000, 0xFFFFD}}

// LoadNames indexes the glyph names of the font file at path. A font without
// glyph names (a version 3 "post" table) gives an empty index, not an error.
func LoadNames(path string) (*Names, error) {
	data, err := mapFile(path)
	if err != nil {
		return nil, fmt.Errorf("glyph: %w", err)
	}
	n := &Names{data: data}
	f, err := sfnt.Parse(data)
	if err != nil {
		n.Close()
		return nil, fmt.Errorf("glyph: parse %s: %w", path, err)
	}
	spans := postNames(data)
	var buf sfnt.Buffer
	for _, rg := range iconRanges {
		for r := rg[0]; r <= rg[1]; r++ {
			gi, err := f.GlyphIndex(&buf, r)
			if err != nil || gi == 0 || int(gi) >= len(spans) {
				continue
			}
			if s := spans[gi]; s.n > 0 {
				n.idx = append(n.idx, nameEntry{off: s.off, n: s.n, r: r})
			}
		}
	}
	slices.SortFunc(n.idx, func(a, b nameEntry) int {
		return bytes.Compare(n.data[a.off:a.off+a.n], n.data[b.off:b.off+b.n])
	})
	return n, nil
}

// Lookup returns the rune of the glyph called name.
func (n *Names) Lookup(name string) (rune, bool) {
	if n == nil {
		return 0, false
	}
	lo, hi := 0, len(n.idx)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		e := n.idx[m]
		// string(bytes) in a comparison does not allocate.
		switch s := n.data[e.off : e.off+e.n]; {
		case string(s) < name:
			lo = m + 1
		case string(s) > name:
			hi = m
		default:
			return e.r, true
		}
	}
	return 0, false
}

// Len returns the number of indexed names.
func (n *Names) Len() int { return len(n.idx) }

// Close unmaps the font file. n must not be used afterwards.
func (n *Names) Close() {
	if n != nil && n.data != nil {
		_ = unix.Munmap(n.data)
		n.data, n.idx = nil, nil
	}
}

type span struct{ off, n uint32 }

// postNames returns, for each glyph index, where its custom name sits in
// data, from a version 2 "post" table. Glyphs with a standard Macintosh name
// (index < 258) or no table at all give empty spans. Every offset is checked
// against the file, so a damaged font gives fewer names, never a panic.
func postNames(data []byte) []span {
	u16 := func(off int) (int, bool) {
		if off < 0 || off+2 > len(data) {
			return 0, false
		}
		return int(binary.BigEndian.Uint16(data[off:])), true
	}
	u32 := func(off int) (uint32, bool) {
		if off < 0 || off+4 > len(data) {
			return 0, false
		}
		return binary.BigEndian.Uint32(data[off:]), true
	}
	tables, ok := u16(4)
	if !ok {
		return nil
	}
	post, end := -1, 0
	for i := range tables {
		rec := 12 + 16*i
		if rec+16 > len(data) {
			return nil
		}
		if string(data[rec:rec+4]) == "post" {
			off, _ := u32(rec + 8)
			n, _ := u32(rec + 12)
			if uint64(off)+uint64(n) > uint64(len(data)) {
				return nil
			}
			post, end = int(off), int(off)+int(n)
			break
		}
	}
	if v, ok := u32(post); !ok || v != 0x00020000 {
		return nil
	}
	glyphs, ok := u16(post + 32)
	if !ok || post+34+2*glyphs > end {
		return nil
	}
	// The custom names follow the index array as Pascal strings.
	var custom []uint32
	for o := post + 34 + 2*glyphs; o < end; o += 1 + int(data[o]) {
		if o+1+int(data[o]) > end {
			break
		}
		custom = append(custom, uint32(o))
	}
	spans := make([]span, glyphs)
	for g := range glyphs {
		k, _ := u16(post + 34 + 2*g)
		if k < 258 || k-258 >= len(custom) {
			continue
		}
		o := custom[k-258]
		spans[g] = span{off: o + 1, n: uint32(data[o])}
	}
	return spans
}
