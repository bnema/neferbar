#!/bin/bash
# Left: the active workspace as an accent block that fades into the bar, then
# the other workspaces with one dot per window. On NeferWL it also shows the
# stash. Without NeferWL's state file it shows nothing.
. "$(dirname "$0")/lib.sh"

DOT=$ICON_DOT STASH=$ICON_STASH
[ -r "${NEFERWL_STATE:-}" ] || {
	echo
	exec sleep infinity
}

draw() {
	# First line: "<active> <count>" of the focused output. Then one line per
	# workspace: "<n> <windows> <stashed>".
	local data active count n w s line pips
	data=$(jq -r '
		.output as $o
		| (.outputs[] | select(.name == $o)) as $out
		| [.windows[] | select(.output == $o)] as $ws
		| "\($out.active) \($out.count)",
		  (range(1; $out.count + 1) as $n
		   | [$ws[] | select(.workspace == $n)] as $here
		   | "\($n) \($here | length) \([$here[] | select(.stash_count > 0)] | length)")' \
		"$NEFERWL_STATE" 2>/dev/null) || return
	read -r active count <<<"$(head -1 <<<"$data")"
	[ -n "$active" ] || return

	line="$(bg "$ACCENT")$(fg "$(ink "$ACCENT")")$BOLD  $active $RESET$(fade "$ACCENT" "$BAR")$(bg "$BAR")"
	while read -r n w s; do
		pips=""
		((w > 0)) && printf -v pips "$DOT%.0s" $(seq 1 $((w > 4 ? 4 : w)))
		if ((n == active)); then
			line+="$(fg "$NEFERBAR_FOREGROUND") $n $(fg "$ACCENT")$pips"
			((s > 0)) && line+="$(fg "$NEFERBAR_COLOR2") $STASH$s"
			line+=" "
		elif ((w > 0)); then
			line+="$(fg "$NEFERBAR_FOREGROUND") $n $(fg "$DIM")$pips "
		else
			line+="$(fg "$DIM") $n "
		fi
	done < <(tail -n +2 <<<"$data")
	printf '%s%s\f' "$line" "$RESET"
}

state_changed_loop draw
