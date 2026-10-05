#!/bin/bash
# Center: the focused window, as "icon  name  title". "neferbar app" asks the
# compositor and prints one line each time the focus or a title changes, so this
# script sleeps until something happens.
. "$(dirname "$0")/lib.sh"

MAX_TITLE=${NEFERBAR_TITLE_MAX:-60} # cells; a longer title ends with an ellipsis

# $NEFERBAR_BIN is the neferbar that started this script. A line ends the bar
# when the compositor goes away; the bar then restarts the script.
"$NEFERBAR_BIN" app name title | while IFS=$'\t' read -r name title; do
	if [ -z "$name$title" ]; then
		printf '\f'
		continue
	fi
	((${#title} > MAX_TITLE)) && title="${title:0:MAX_TITLE-1}…"
	printf '%s%s%s %s%s%s' "$(bg "$BAR")" "$(fg "$NEFERBAR_COLOR3")" "$ICON_APP" "$(fg "$NEFERBAR_FOREGROUND")" "$name" "$RESET"
	[ -n "$title" ] && printf ' %s%s%s' "$(fg "$DIM")" "$title" "$RESET"
	printf '\f'
done
