package tray

import "math"

// histogram finds the dominant color of an icon. Colors are counted in 4096
// buckets (4 bits per channel), weighted by opacity and saturation so a logo's
// color wins over its grey outline. It is reused between icons: no
// allocation.
type histogram struct {
	weight [4096]uint32
	sum    [4096][3]uint32
}

// dominant returns the dominant color of argb, an SNI pixmap: big-endian
// ARGB32, 4 bytes per pixel. It reports false when the icon is empty, fully
// transparent or grey.
func (h *histogram) dominant(argb []byte) (RGB, bool) {
	if len(argb) < 4 {
		return RGB{}, false
	}
	clear(h.weight[:])
	clear(h.sum[:])
	for i := 0; i+3 < len(argb); i += 4 {
		a, r, g, b := uint32(argb[i]), uint32(argb[i+1]), uint32(argb[i+2]), uint32(argb[i+3])
		if a < 128 {
			continue
		}
		hi, lo := max(r, g, b), min(r, g, b)
		chroma := hi - lo
		if chroma < 40 {
			continue // grey, black or white: outline and shading
		}
		w := a * chroma >> 8
		k := (r>>4)<<8 | (g>>4)<<4 | b>>4
		h.weight[k] += w
		h.sum[k][0] += r * w
		h.sum[k][1] += g * w
		h.sum[k][2] += b * w
	}
	best := -1
	for k, w := range h.weight {
		if w > 0 && (best < 0 || w > h.weight[best]) {
			best = k
		}
	}
	if best < 0 {
		return RGB{}, false
	}
	w := h.weight[best]
	s := h.sum[best]
	return RGB{uint8(s[0] / w), uint8(s[1] / w), uint8(s[2] / w)}, true
}

// readable moves c toward fg until it has enough contrast with bg, so a dark
// logo still shows on a dark bar.
func readable(c, bg, fg RGB) RGB {
	const minContrast = 3.0 // WCAG's minimum for large text and icons
	for i := 0; i < 10 && contrast(c, bg) < minContrast; i++ {
		c = mix(c, fg, 0.25)
	}
	return c
}

func mix(a, b RGB, t float64) RGB {
	var out RGB
	for i := range out {
		out[i] = uint8(math.Round(float64(a[i]) + (float64(b[i])-float64(a[i]))*t))
	}
	return out
}

// contrast is the WCAG contrast ratio of two colors, from 1 to 21.
func contrast(a, b RGB) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func luminance(c RGB) float64 {
	lin := func(v uint8) float64 {
		x := float64(v) / 255
		if x <= 0.04045 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c[0]) + 0.7152*lin(c[1]) + 0.0722*lin(c[2])
}
