# Shared helpers for the bundled modules. Source this file: . "$(dirname "$0")/lib.sh"
#
# The bar gives every script its colors, each as #rrggbb, so a script never
# hard-codes one:
#   NEFERBAR_BACKGROUND  the bar's own background
#   NEFERBAR_FOREGROUND  the bar's text color
#   NEFERBAR_COLOR0..15  the theme's 16 ANSI colors

: "${NEFERBAR_BACKGROUND:=#26263a}" "${NEFERBAR_FOREGROUND:=#cdd6f4}"
: "${NEFERBAR_COLOR0:=#45475a}" "${NEFERBAR_COLOR2:=#a6e3a1}" "${NEFERBAR_COLOR3:=#f9e2af}"
: "${NEFERBAR_COLOR4:=#89b4fa}" "${NEFERBAR_COLOR15:=#a6adc8}"

RESET=$'\e[0m' BOLD=$'\e[1m'

# Nerd Font icons as raw UTF-8 bytes. Unlike \u escapes, these do not depend on
# the locale of the process that runs the script.
ICON_DOT=$'\xef\x84\x91'     # U+F111 circle
ICON_STASH=$'\xef\x86\x87'   # U+F187 archive
ICON_APP=$'\xef\x8b\x90'     # U+F2D0 window
ICON_CLOCK=$'\xef\x80\x97'   # U+F017 clock

# rgb "#rrggbb" -> "r;g;b"
rgb() {
	local h=${1#\#}
	printf '%d;%d;%d' "0x${h:0:2}" "0x${h:2:2}" "0x${h:4:2}"
}
fg() { printf '\e[38;2;%sm' "$(rgb "$1")"; }
bg() { printf '\e[48;2;%sm' "$(rgb "$1")"; }

# mix "#a" "#b" PERCENT -> "#rrggbb", PERCENT of the way from a to b
mix() {
	local a=${1#\#} b=${2#\#} p=$3 i out=#
	for i in 0 2 4; do
		out+=$(printf '%02x' $(((0x${a:i:2} * (100 - p) + 0x${b:i:2} * p) / 100)))
	done
	printf '%s' "$out"
}

# fade FROM TO -> four blank cells that blend FROM into TO
fade() {
	local i
	for i in 20 40 60 80; do printf '%s ' "$(bg "$(mix "$1" "$2" "$i")")"; done
}

BAR=$NEFERBAR_BACKGROUND
ACCENT=$NEFERBAR_COLOR4
DIM=$(mix "$BAR" "$NEFERBAR_FOREGROUND" 45)

# ink "#accent" -> the text color for a block of that color: the theme's black
# on a light accent, its white on a dark one.
ink() {
	local h=${1#\#}
	if (((0x${h:0:2} * 299 + 0x${h:2:2} * 587 + 0x${h:4:2} * 114) / 1000 > 140)); then
		printf '%s' "$NEFERBAR_COLOR0"
	else
		printf '%s' "$NEFERBAR_COLOR15"
	fi
}

# state_changed_loop CMD... runs CMD now and again whenever $NEFERWL_STATE
# (NeferWL's state file) changes. NeferWL replaces the file on every change, so
# a newer modification time is enough, and "read -t" waits without forking.
state_changed_loop() {
	local stamp tick
	stamp=$(mktemp) && trap 'rm -f "$stamp"' EXIT
	exec {tick}<> <(:)
	"$@"
	touch -r "$NEFERWL_STATE" "$stamp" 2>/dev/null
	while read -r -t 0.1 -u "$tick" || true; do
		if [[ $NEFERWL_STATE -nt $stamp ]]; then
			touch -r "$NEFERWL_STATE" "$stamp"
			"$@"
		fi
	done
}
