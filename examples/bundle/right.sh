#!/bin/bash
# Right: the clock in an accent block, with a fade on its left edge.
. "$(dirname "$0")/lib.sh"

while true; do
	# One reading of the clock, for the text and for the sleep, so they cannot
	# straddle a minute boundary.
	printf -v now '%(%s)T' -1
	printf '%s%s%s%s %(%H:%M)T %s\f' \
		"$(fade "$BAR" "$ACCENT")" "$(bg "$ACCENT")" "$(fg "$(ink "$ACCENT")")" "$BOLD" "$now" "$RESET"
	sleep $((60 - now % 60))
done
