#!/bin/sh
# 60 fps animation: a scrolling rainbow. Each frame ends with a form feed.
# usage: rainbow.sh [fps] [width]
fps=${1:-60}
width=${2:-40}
exec awk -v fps="$fps" -v w="$width" 'BEGIN {
	pi = 3.14159265; step = 1.0 / fps; t = 0
	while (1) {
		line = ""
		for (i = 0; i < w; i++) {
			p = (i * 0.25) - (t * 6)
			r = int(127 + 127 * sin(p)); g = int(127 + 127 * sin(p + 2 * pi / 3)); b = int(127 + 127 * sin(p + 4 * pi / 3))
			line = line sprintf("\033[48;2;%d;%d;%dm \033[0m", r, g, b)
		}
		printf "%s\f", line
		fflush()
		system("sleep " step)
		t += step
	}
}'
