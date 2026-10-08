package glyph

import (
	"encoding/binary"
	"os"
	"strings"
	"testing"
)

// nerdFont returns the path of an installed Nerd Font, or skips the test.
func nerdFont(t *testing.T) string {
	t.Helper()
	paths, err := FindFonts("JetBrainsMono Nerd Font Mono")
	if err != nil || !strings.Contains(strings.ToLower(paths[0]), "nerd") {
		t.Skip("no Nerd Font installed")
	}
	return paths[0]
}

func TestNamesLookup(t *testing.T) {
	n, err := LoadNames(nerdFont(t))
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	if n.Len() < 1000 {
		t.Fatalf("only %d names indexed", n.Len())
	}
	for name, want := range map[string]rune{"fa-steam": 0xF1B6, "md-steam": 0xF04D3} {
		if r, ok := n.Lookup(name); !ok || r != want {
			t.Errorf("Lookup(%q) = %U, %v; want %U", name, r, ok, want)
		}
	}
	if _, ok := n.Lookup("no-such-glyph"); ok {
		t.Error("an unknown name was found")
	}
	if allocs := testing.AllocsPerRun(100, func() { n.Lookup("fa-steam") }); allocs != 0 {
		t.Errorf("Lookup allocates %v times", allocs)
	}
}

func TestNilNamesLookup(t *testing.T) {
	var n *Names
	if _, ok := n.Lookup("fa-steam"); ok {
		t.Error("a nil index found a name")
	}
}

// postFont builds a font file holding only a "post" table of the given
// version, with one glyph index per entry of indices and the custom names
// after them.
func postFont(version uint32, indices []uint16, names ...string) []byte {
	post := make([]byte, 34+2*len(indices))
	binary.BigEndian.PutUint32(post, version)
	binary.BigEndian.PutUint16(post[32:], uint16(len(indices)))
	for i, k := range indices {
		binary.BigEndian.PutUint16(post[34+2*i:], k)
	}
	for _, s := range names {
		post = append(post, byte(len(s)))
		post = append(post, s...)
	}
	b := make([]byte, 12+16, 12+16+len(post))
	b[5] = 1 // one table
	copy(b[12:], "post")
	binary.BigEndian.PutUint32(b[12+8:], 28)
	binary.BigEndian.PutUint32(b[12+12:], uint32(len(post)))
	return append(b, post...)
}

func TestPostNames(t *testing.T) {
	// Glyph 0 has a standard name, 1 and 2 custom ones, 3 a custom index
	// past the names.
	data := postFont(0x00020000, []uint16{3, 259, 258, 300}, "first", "second")
	spans := postNames(data)
	if len(spans) != 4 {
		t.Fatalf("%d spans, want 4", len(spans))
	}
	name := func(g int) string { return string(data[spans[g].off : spans[g].off+spans[g].n]) }
	if spans[0].n != 0 || spans[3].n != 0 {
		t.Errorf("standard or missing names gave spans: %v", spans)
	}
	if name(1) != "second" || name(2) != "first" {
		t.Errorf("names = %q, %q", name(1), name(2))
	}
}

func TestPostNamesVersion3(t *testing.T) {
	// A version 3 table stores no names: nothing to index, no error.
	if spans := postNames(postFont(0x00030000, []uint16{259}, "x")); spans != nil {
		t.Fatalf("got %d spans", len(spans))
	}
}

func TestPostNamesDamagedFont(t *testing.T) {
	// Truncated or garbage files give no names and never panic.
	var good []byte
	if paths, err := FindFonts("monospace"); err == nil {
		good, _ = os.ReadFile(paths[0])
	}
	inputs := [][]byte{nil, {0, 1, 0, 0}, make([]byte, 64)}
	for _, cut := range []int{12, 100, 1000, len(good) / 2} {
		if cut <= len(good) {
			inputs = append(inputs, good[:cut])
		}
	}
	for _, b := range inputs {
		_ = postNames(b)
	}
	// A "post" table that claims more glyphs than it holds.
	b := postFont(0x00020000, nil)
	binary.BigEndian.PutUint16(b[28+32:], 0xff)
	if spans := postNames(b); spans != nil {
		t.Fatalf("got %d spans from a bad table", len(spans))
	}
}

func FuzzPostNames(f *testing.F) {
	f.Add(postFont(0x00020000, []uint16{3, 259, 258, 300}, "first", "second"))
	f.Add(postFont(0x00030000, nil))
	f.Add([]byte{0, 1, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		for g, s := range postNames(data) {
			if uint64(s.off)+uint64(s.n) > uint64(len(data)) {
				t.Fatalf("glyph %d: span %v past %d bytes", g, s, len(data))
			}
		}
	})
}
