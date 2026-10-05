package layout

var cube = [6]uint8{0, 95, 135, 175, 215, 255}

// palette maps an xterm 256-color index to RGB. The first 16 come from the
// theme; the color cube and the gray ramp are fixed, as in any terminal.
func palette(i int, base *[16][3]uint8) [3]uint8 {
	switch {
	case i < 0:
		return [3]uint8{}
	case i < 16:
		return base[i]
	case i < 232:
		i -= 16
		return [3]uint8{cube[i/36], cube[i/6%6], cube[i%6]}
	case i < 256:
		v := uint8(8 + (i-232)*10)
		return [3]uint8{v, v, v}
	}
	return [3]uint8{0xff, 0xff, 0xff}
}
