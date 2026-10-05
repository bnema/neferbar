#!/bin/bash
# Center: the focused window. NeferWL's state file has the app id but no title;
# a title needs the compositor's toplevel protocol, which the bar does not read yet.
. "$(dirname "$0")/lib.sh"

[ -r "${NEFERWL_STATE:-}" ] || {
	echo
	exec sleep infinity
}

draw() {
	local app
	# An app id is chosen by the application: drop control characters, which
	# would otherwise reach the bar as escape sequences.
	app=$(jq -r '(.window.app_id // empty) | explode | map(select(. >= 32 and . != 127)) | implode' "$NEFERWL_STATE" 2>/dev/null) || return
	if [ -n "$app" ]; then
		printf '%s%s%s %s%s%s\f' "$(bg "$BAR")" "$(fg "$NEFERBAR_COLOR3")" "$ICON_APP" "$(fg "$NEFERBAR_FOREGROUND")" "$app" "$RESET"
	else
		printf '\f'
	fi
}

state_changed_loop draw
