// Package glyph rasterizes single glyphs of a monospace font into fixed-size
// cell bitmaps (one byte of coverage per pixel).
package glyph

import (
	"fmt"
	"image"
	"math"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Face rasterizes runes of one font at one pixel size. All glyphs are clipped
// to a CellW x CellH box. It is used from one goroutine.
type Face struct {
	regular, bold font.Face
	// CellW and CellH are the cell size in physical pixels.
	CellW, CellH int
	ascent       fixed.Int26_6
}

// FindFont asks fontconfig for the file of a family and weight.
func FindFont(family string, bold bool) (string, error) {
	pattern := family
	if bold {
		pattern += ":bold"
	}
	out, err := exec.Command("fc-match", "-f", "%{file}", pattern).Output()
	if err != nil {
		return "", fmt.Errorf("glyph: fc-match %q: %w", pattern, err)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("glyph: no font file for %q", pattern)
	}
	return path, nil
}

func loadFace(path string, px float64) (font.Face, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("glyph: %w", err)
	}
	f, err := opentype.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("glyph: parse %s: %w", path, err)
	}
	return opentype.NewFace(f, &opentype.FaceOptions{Size: px, DPI: 72, Hinting: font.HintingFull})
}

// Load opens a regular and an optional bold font file at px pixels. The cell
// size comes from the regular face: the advance of "M" and ascent plus descent.
func Load(regularPath, boldPath string, px float64) (*Face, error) {
	if px < 4 || px > 512 {
		return nil, fmt.Errorf("glyph: unsupported font size %.1f px", px)
	}
	f := &Face{}
	var err error
	if f.regular, err = loadFace(regularPath, px); err != nil {
		return nil, err
	}
	f.bold = f.regular
	if boldPath != "" && boldPath != regularPath {
		if f.bold, err = loadFace(boldPath, px); err != nil {
			f.regular.Close()
			return nil, err
		}
	}
	m := f.regular.Metrics()
	adv, ok := f.regular.GlyphAdvance('M')
	if !ok || adv <= 0 {
		f.Close()
		return nil, fmt.Errorf("glyph: font has no usable advance for 'M'")
	}
	f.CellW = int(math.Round(float64(adv) / 64))
	f.CellH = int(math.Ceil(float64(m.Ascent+m.Descent) / 64))
	f.ascent = m.Ascent
	if f.CellW < 1 || f.CellH < 1 {
		f.Close()
		return nil, fmt.Errorf("glyph: degenerate cell %dx%d", f.CellW, f.CellH)
	}
	return f, nil
}

// Close releases the font faces.
func (f *Face) Close() {
	if f == nil {
		return
	}
	if f.bold != nil && f.bold != f.regular {
		f.bold.Close()
	}
	if f.regular != nil {
		f.regular.Close()
	}
	f.regular, f.bold = nil, nil
}

// Rasterize draws r into dst, which must hold CellW*CellH bytes (stride
// CellW). Pixels outside the cell are clipped; a missing glyph leaves dst
// empty.
func (f *Face) Rasterize(r rune, bold bool, dst []byte) {
	clear(dst[:f.CellW*f.CellH])
	face := f.regular
	if bold {
		face = f.bold
	}
	dr, mask, maskp, _, ok := face.Glyph(fixed.Point26_6{X: 0, Y: f.ascent}, r)
	if !ok {
		return
	}
	alpha, isAlpha := mask.(*image.Alpha)
	for y := dr.Min.Y; y < dr.Max.Y; y++ {
		if y < 0 || y >= f.CellH {
			continue
		}
		for x := dr.Min.X; x < dr.Max.X; x++ {
			if x < 0 || x >= f.CellW {
				continue
			}
			mx, my := maskp.X+x-dr.Min.X, maskp.Y+y-dr.Min.Y
			if isAlpha {
				dst[y*f.CellW+x] = alpha.Pix[alpha.PixOffset(mx, my)]
				continue
			}
			_, _, _, a := mask.At(mx, my).RGBA()
			dst[y*f.CellW+x] = uint8(a >> 8)
		}
	}
}
