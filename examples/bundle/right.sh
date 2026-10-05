#!/bin/bash
# Right: the clock in an accent block, with a fade on its left edge.
. "$(dirname "$0")/lib.sh"

while true; do
	printf '%s%s%s%s%s %(%H:%M)T %s\f' \
		"$(fade "$BAR" "$ACCENT")" "$(bg "$ACCENT")" "$(fg "$(ink "$ACCENT")")" "$BOLD" "$ICON_CLOCK" -1 "$RESET"
	# Sleep until the next minute starts.
	sleep $((60 - 10#$(date +%S)))
done
