package tray

import "testing"

// pixmap builds an ARGB32 icon from pixels of the given colors, n of each.
func pixmap(colors map[[4]uint8]int) []byte {
	var b []byte
	for c, n := range colors {
		for range n {
			b = append(b, c[:]...)
		}
	}
	return b
}

func TestDominant(t *testing.T) {
	var h histogram
	// A blue logo with a larger grey outline and a transparent background.
	got, ok := h.dominant(pixmap(map[[4]uint8]int{
		{255, 30, 60, 200}:   40, // blue
		{255, 128, 128, 128}: 100,
		{0, 255, 0, 0}:       500, // transparent red
	}))
	if !ok || got != (RGB{30, 60, 200}) {
		t.Fatalf("dominant = %v, %v", got, ok)
	}
	if _, ok := h.dominant(pixmap(map[[4]uint8]int{{255, 200, 200, 200}: 10})); ok {
		t.Fatal("a grey icon has no dominant color")
	}
	if _, ok := h.dominant(nil); ok {
		t.Fatal("an empty icon has no dominant color")
	}
	icon := pixmap(map[[4]uint8]int{{255, 30, 60, 200}: 1024})
	if allocs := testing.AllocsPerRun(50, func() { h.dominant(icon) }); allocs != 0 {
		t.Fatalf("dominant allocates %v times", allocs)
	}
}

func TestReadable(t *testing.T) {
	bg, fg := RGB{0x1e, 0x1e, 0x2e}, RGB{0xcd, 0xd6, 0xf4}
	dark := RGB{0x17, 0x1a, 0x21} // Steam's near-black blue
	got := readable(dark, bg, fg)
	if contrast(got, bg) < 3 {
		t.Fatalf("readable(%v) = %v, contrast %.2f", dark, got, contrast(got, bg))
	}
	bright := RGB{0xf9, 0xe2, 0xaf}
	if readable(bright, bg, fg) != bright {
		t.Fatal("a readable color must not change")
	}
}
