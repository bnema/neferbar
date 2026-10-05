#!/bin/bash
# Left: the active workspace as an accent block that fades into the bar, then
# the workspaces with one dot per window. The active workspace shows only its
# dots, and the dot of the focused window is in the accent color. On NeferWL it
# also shows the stash. Without NeferWL's state file it shows nothing.
. "$(dirname "$0")/lib.sh"

DOT=$ICON_DOT STASH=$ICON_STASH
[ -r "${NEFERWL_STATE:-}" ] || {
	echo
	exec sleep infinity
}

draw() {
	# First line: "<active> <count>" of the focused output. Then one line per
	# workspace: "<n> <windows> <stashed> <focused>", where <focused> is the
	# position (from 1) of the focused window among the workspace's windows, or 0.
	local data active count n w s f line pips i color
	data=$(jq -r '
		.output as $o
		| (.outputs[] | select(.name == $o)) as $out
		| [.windows[] | select(.output == $o)] as $ws
		| (if .window.output == $o then .window.id else null end) as $focus
		| "\($out.active) \($out.count)",
		  (range(1; $out.count + 1) as $n
		   | [$ws[] | select(.workspace == $n)] as $here
		   | (if $focus == null then null else ($here | map(.id) | index($focus)) end) as $at
		   | "\($n) \($here | length) \([$here[] | select(.stash_count > 0)] | length) \(if $at == null then 0 else $at + 1 end)")' \
		"$NEFERWL_STATE" 2>/dev/null) || return
	read -r active count <<<"$(head -1 <<<"$data")"
	# The state file is data, not code: bash arithmetic runs command
	# substitutions, so only digits may reach it.
	[[ $active =~ ^[0-9]+$ && $count =~ ^[0-9]+$ ]] || return

	line="$(bg "$ACCENT")$(fg "$(ink "$ACCENT")")$BOLD  $active $RESET$(fade "$ACCENT" "$BAR")$(bg "$BAR")"
	while read -r n w s f; do
		[[ $n =~ ^[0-9]+$ && $w =~ ^[0-9]+$ && $s =~ ^[0-9]+$ && $f =~ ^[0-9]+$ ]] || continue
		# At most 4 dots; a focused window past the 4th is shown on the 4th.
		pips=""
		for ((i = 1; i <= (w > 4 ? 4 : w); i++)); do
			color=$DIM
			((n == active)) && color=$NEFERBAR_FOREGROUND
			((i == (f > 4 ? 4 : f))) && color=$ACCENT
			pips+="$(fg "$color")$DOT"
		done
		if ((n == active)); then
			# The number is already in the accent block on the left.
			line+=" $pips"
			((s > 0)) && line+="$(fg "$NEFERBAR_COLOR2") $STASH$s"
			line+=" "
		elif ((w > 0)); then
			line+="$(fg "$NEFERBAR_FOREGROUND") $n $pips "
		else
			line+="$(fg "$DIM") $n "
		fi
	done < <(tail -n +2 <<<"$data")
	printf '%s%s\f' "$line" "$RESET"
}

state_changed_loop draw
