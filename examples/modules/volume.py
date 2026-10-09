#!/usr/bin/env python3
"""Volume module with a right-click menu, for an interactive module:

    [[module]]
    name = "volume"
    zone = "right"
    exec = "~/.config/neferbar/modules/volume.py"
    interactive = true

Wheel changes the volume, a left click mutes, a right click opens a menu.
Uses wpctl (WirePlumber).
"""

import json
import subprocess
import sys

SINK = "@DEFAULT_AUDIO_SINK@"

# Menu item ids, chosen by this script.
MUTE, VOL_25, VOL_50, VOL_100 = 1, 2, 3, 4


def wpctl(*args):
    return subprocess.run(["wpctl", *args], capture_output=True, text=True).stdout


def state():
    # "Volume: 0.42" or "Volume: 0.42 [MUTED]"
    out = wpctl("get-volume", SINK).split()
    volume = round(float(out[1]) * 100) if len(out) > 1 else 0
    return volume, "[MUTED]" in out


def show():
    volume, muted = state()
    icon = "󰖁" if muted else "󰕾"
    print(f"{icon} {volume}%", flush=True)


def control(message):
    """Print a control line: the bar reads it as a popup request, not as text."""
    print(f"\033]777;neferbar;{json.dumps(message)}\007", flush=True)


def open_menu(token):
    _, muted = state()
    control({
        "type": "menu",
        "col": 0,
        "width": 1,  # the icon
        "click": token,  # answers this click
        "items": [
            {"id": MUTE, "label": "Mute", "kind": "check", "checked": muted},
            {"id": 0, "kind": "separator"},
            {"id": 0, "label": "Set volume", "items": [
                {"id": VOL_25, "label": "25%"},
                {"id": VOL_50, "label": "50%"},
                {"id": VOL_100, "label": "100%"},
            ]},
        ],
    })


def activate(item):
    if item == MUTE:
        wpctl("set-mute", SINK, "toggle")
    elif item in (VOL_25, VOL_50, VOL_100):
        level = {VOL_25: "0.25", VOL_50: "0.5", VOL_100: "1.0"}[item]
        wpctl("set-volume", SINK, level)


def main():
    show()
    # One event per line: "click right 0 7", "scroll up 1 0", "menu-activate 7 1", ...
    for line in sys.stdin:
        event, *args = line.split()
        if event == "click":
            button, _col, token = args
            if button == "left":
                wpctl("set-mute", SINK, "toggle")
            elif button == "right":
                open_menu(int(token))
        elif event == "scroll":
            direction, steps, _col = args
            sign = "+" if direction == "up" else "-"
            wpctl("set-volume", "-l", "1.0", SINK, f"{int(steps) * 2}%{sign}")
        elif event == "menu-activate":
            _token, item = args
            activate(int(item))
        else:
            continue  # hover, leave, menu-closed
        show()


if __name__ == "__main__":
    main()
