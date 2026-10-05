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

// Style is a set of text attributes. Bold and Italic pick a font file;
// Underline and Strike are drawn into the glyph bitmap.
type Style uint8

const (
	Bold Style = 1 << iota
	Italic
	Underline
	Strike
)

// fontBits is the part of a Style that selects one of the four font files.
const fontBits = Bold | Italic

// Paths holds the font file of each weight and slant, indexed by Bold|Italic.
// An empty entry falls back to the nearest available face.
type Paths [4]string

// Face rasterizes runes of one font family at one pixel size. All glyphs are
// clipped to a CellW x CellH box. It is used from one goroutine.
type Face struct {
	faces [4]font.Face
	// CellW and CellH are the cell size in physical pixels.
	CellW, CellH int
	ascent       fixed.Int26_6
	ascentPx     int
	thick        int // line thickness for underline and strike, in pixels
}

// fcPatterns are the fontconfig suffixes for Paths' four entries.
var fcPatterns = [4]string{"", ":bold", ":italic", ":bold:italic"}

// FindFonts asks fontconfig for the four files of a family. Styles the family
// lacks resolve to whatever fontconfig picks, usually the regular file.
func FindFonts(family string) (Paths, error) {
	var p Paths
	for i, suffix := range fcPatterns {
		out, err := exec.Command("fc-match", "-f", "%{file}", family+suffix).Output()
		if err != nil {
			return p, fmt.Errorf("fontconfig lookup of %q failed: %w", family+suffix, err)
		}
		p[i] = strings.TrimSpace(string(out))
		if p[i] == "" {
			return p, fmt.Errorf("fontconfig found no file for %q", family+suffix)
		}
	}
	return p, nil
}

// FamilyOf reports the family fontconfig resolved a request to, so callers can
// warn when it is not the family that was asked for.
func FamilyOf(request string) string {
	out, err := exec.Command("fc-match", "-f", "%{family[0]}", request).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
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

// Load opens the font files at px pixels. The cell size comes from the regular
// face: the advance of "M", and ascent plus descent.
func Load(paths Paths, px float64) (*Face, error) {
	if px < 4 || px > 512 {
		return nil, fmt.Errorf("glyph: unsupported font size %.1f px", px)
	}
	if paths[0] == "" {
		return nil, fmt.Errorf("glyph: no regular font file")
	}
	f := &Face{}
	byPath := map[string]font.Face{}
	for i, path := range paths {
		if path == "" {
			continue // filled from a neighbour below
		}
		if face, ok := byPath[path]; ok {
			f.faces[i] = face
			continue
		}
		face, err := loadFace(path, px)
		if err != nil {
			f.Close()
			return nil, err
		}
		byPath[path], f.faces[i] = face, face
	}
	// A missing bold-italic uses bold, then italic; any other gap uses regular.
	for i := range f.faces {
		if f.faces[i] != nil {
			continue
		}
		switch {
		case Style(i) == Bold|Italic && f.faces[Bold] != nil:
			f.faces[i] = f.faces[Bold]
		case Style(i) == Bold|Italic && f.faces[Italic] != nil:
			f.faces[i] = f.faces[Italic]
		default:
			f.faces[i] = f.faces[0]
		}
	}
	m := f.faces[0].Metrics()
	adv, ok := f.faces[0].GlyphAdvance('M')
	if !ok || adv <= 0 {
		f.Close()
		return nil, fmt.Errorf("glyph: font has no usable advance for 'M'")
	}
	f.CellW = int(math.Round(float64(adv) / 64))
	f.CellH = int(math.Ceil(float64(m.Ascent+m.Descent) / 64))
	f.ascent = m.Ascent
	f.ascentPx = int(math.Round(float64(m.Ascent) / 64))
	f.thick = max(1, int(math.Round(px/14)))
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
	closed := map[font.Face]bool{}
	for i, face := range f.faces {
		if face != nil && !closed[face] {
			closed[face] = true
			face.Close()
		}
		f.faces[i] = nil
	}
}

// Rasterize draws r into dst, which must hold CellW*CellH bytes (stride
// CellW). Pixels outside the cell are clipped; a missing glyph leaves dst
// empty. Underline and Strike add a full-width line in the foreground.
func (f *Face) Rasterize(r rune, style Style, dst []byte) {
	clear(dst[:f.CellW*f.CellH])
	face := f.faces[style&fontBits]
	if dr, mask, maskp, _, ok := face.Glyph(fixed.Point26_6{X: 0, Y: f.ascent}, r); ok {
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
	if style&Underline != 0 {
		// Below the baseline, a third of the way into the descent.
		descent := f.CellH - f.ascentPx
		f.line(dst, f.ascentPx+max(1, descent/3))
	}
	if style&Strike != 0 {
		// Through the middle of the lowercase letters.
		f.line(dst, f.ascentPx-max(1, f.ascentPx*3/10)-f.thick/2)
	}
}

// line fills f.thick rows starting at y, clipped to the cell.
func (f *Face) line(dst []byte, y int) {
	for row := y; row < y+f.thick; row++ {
		if row < 0 || row >= f.CellH {
			continue
		}
		for x := 0; x < f.CellW; x++ {
			dst[row*f.CellW+x] = 255
		}
	}
}
