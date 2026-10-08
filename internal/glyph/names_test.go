package glyph

import (
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

func TestNamesWithoutIcons(t *testing.T) {
	// A font without icon names indexes nothing, and that is not an error.
	paths, err := FindFonts("monospace")
	if err != nil {
		t.Skip(err)
	}
	if _, err := os.Stat(paths[0]); err != nil {
		t.Skip(err)
	}
	n, err := LoadNames(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	var nilNames *Names
	if _, ok := nilNames.Lookup("fa-steam"); ok {
		t.Error("a nil index found a name")
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
	b := make([]byte, 12+16+40)
	b[5] = 1 // one table
	copy(b[12:], "post")
	b[12+11] = 28 // offset 28
	b[12+15] = 40 // length 40
	b[28+1] = 2   // version 2.0
	b[28+32] = 0xff
	if spans := postNames(b); spans != nil {
		t.Fatalf("got %d spans from a bad table", len(spans))
	}
}
