package glyph

import (
	"os"
	"testing"
)

func testFace(t *testing.T) *Face {
	t.Helper()
	paths, err := FindFonts("monospace")
	if err != nil {
		t.Skip(err)
	}
	if _, err := os.Stat(paths[0]); err != nil {
		t.Skip(err)
	}
	f, err := Load(paths, 16)
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(f.Close)
	return f
}

func rowSum(f *Face, bmp []byte, y int) (sum int) {
	for x := 0; x < f.CellW; x++ {
		sum += int(bmp[y*f.CellW+x])
	}
	return
}

func TestUnderlineAndStrikeDrawFullWidthLines(t *testing.T) {
	f := testFace(t)
	plain := make([]byte, f.CellW*f.CellH)
	f.Rasterize(' ', 0, plain) // a space: only the lines can ink
	for _, b := range plain {
		if b != 0 {
			t.Fatal("a plain space must be empty")
		}
	}
	for name, style := range map[string]Style{"underline": Underline, "strike": Strike} {
		bmp := make([]byte, len(plain))
		f.Rasterize(' ', style, bmp)
		full := 0
		for y := 0; y < f.CellH; y++ {
			if rowSum(f, bmp, y) == 255*f.CellW {
				full++
			}
		}
		if full != f.thick {
			t.Errorf("%s: %d full rows, want %d", name, full, f.thick)
		}
	}
	both := make([]byte, len(plain))
	f.Rasterize(' ', Underline|Strike, both)
	var u, s []byte = make([]byte, len(plain)), make([]byte, len(plain))
	f.Rasterize(' ', Underline, u)
	f.Rasterize(' ', Strike, s)
	for i := range both {
		if both[i] != max(u[i], s[i]) {
			t.Fatal("underline and strike must not overlap or cancel")
		}
	}
}

func TestItalicAndBoldPickAFontFile(t *testing.T) {
	paths, err := FindFonts("monospace")
	if err != nil {
		t.Skip(err)
	}
	f := testFace(t)
	// Each style slot must be usable even when the family lacks the file.
	for style := Style(0); style <= fontBits; style++ {
		bmp := make([]byte, f.CellW*f.CellH)
		f.Rasterize('A', style, bmp)
		ink := 0
		for _, b := range bmp {
			ink += int(b)
		}
		if ink == 0 {
			t.Errorf("style %02b drew nothing (files: %v)", style, paths)
		}
	}
}
