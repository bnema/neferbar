#!/bin/bash
# Run neferbar against a nested headless NeferWL with a clean environment.
# usage: scripts/headless.sh <config> <seconds> [neferbar args...]
# env: SIZE=WxH (output size), SCALE=1.5 (output scale), OUT=dir (screenshots),
#      DISPLAY_VAR=NEFERBAR_DISPLAY (pass the socket that way), NEFERBAR=binary, LOGLINES=n (compositor log lines to show).
set -eu
cfg=$1; secs=$2; shift 2
out=${OUT:-/tmp/neferbar-shots}
root=$(mktemp -d)
mkdir -p "$root"/{run,config/neferwl,data,state} "$out"
chmod 700 "$root/run"
rm -f "$out"/*.png
if [ -n "${SCALE:-}" ]; then
	printf 'output.HEADLESS-1.scale = %s\n' "$SCALE" >"$root/config/neferwl/config"
fi
clean=(env -i PATH="$PATH" HOME="$root" XDG_RUNTIME_DIR="$root/run" XDG_CONFIG_HOME="$root/config" XDG_DATA_HOME="$root/data" XDG_STATE_HOME="$root/state" ${NEFERWL_STATE:+NEFERWL_STATE="$NEFERWL_STATE"})
"${clean[@]}" neferwl --backend=headless --no-terminal --no-xwayland --size "${SIZE:-800x200}" --timeout "$((secs + 6))s" --screenshot "$out" >"$root/neferwl.log" 2>&1 &
comp=$!
trap 'kill $comp 2>/dev/null || true; wait $comp 2>/dev/null || true' EXIT
for _ in $(seq 100); do
	sock=$(ls "$root"/run/wayland-* 2>/dev/null | grep -v lock | head -1 || true)
	[ -n "$sock" ] && break
	sleep 0.1
done
[ -n "${sock:-}" ] || { echo "no compositor socket"; cat "$root/neferwl.log"; exit 1; }
set +e
timeout "$secs" "${clean[@]}" "${DISPLAY_VAR:-WAYLAND_DISPLAY}=$sock" "${NEFERBAR:-/tmp/neferbar}" -config "$cfg" "$@"
echo "neferbar exit: $?"
echo "--- compositor log"; grep -v "not a conformant" "$root/neferwl.log" | tail -"${LOGLINES:-8}"; cp "$root/neferwl.log" /tmp/neferwl-last.log
