#!/usr/bin/env python3
"""Volume, Wi-Fi and battery icons with tooltips and right-click menus.

    [[module]]
    name = "status"
    zone = "right"
    exec = "~/.config/neferbar/modules/status.py"
    interactive = true

Each icon changes with its level; the numbers are in the tooltips.

    volume   wheel: volume   left: mute      middle: mixer    right: menu
    wifi                     left: settings                   right: networks
    battery                                                   right: power profile

Needs wpctl (PipeWire), nmcli (NetworkManager) and powerprofilesctl; an icon
whose tool is missing is not shown. The mixer (pavucontrol) and the network
settings (nmtui in $TERMINAL) are optional. Edit the commands below to taste.
"""

import json
import os
import re
import select
import shutil
import subprocess
import sys
import time

TERMINAL = os.environ.get("TERMINAL", "kitty")
MIXER = ["pavucontrol"]
NETWORK_SETTINGS = ["nmtui"]  # runs in TERMINAL; impala if you use iwd
VOLUME_STEP = 2  # percent per wheel step
TOOLTIP_DELAY = 0.4  # seconds of hover before a tooltip
REFRESH = 5.0  # seconds between two reads of volume and battery
WIFI_REFRESH = 10.0

SINK = "@DEFAULT_AUDIO_SINK@"

# Nerd Font (Material Design) icons, from empty to full.
VOLUME_ICONS = ["󰕿", "󰖀", "󰕾"]
VOLUME_MUTED = "󰝟"
WIFI_ICONS = ["󰤯", "󰤟", "󰤢", "󰤥", "󰤨"]
WIFI_OFF, WIFI_DOWN, ETHERNET = "󰖪", "󰤮", "󰈀"
BATTERY_ICONS = ["󰂎", "󰁺", "󰁻", "󰁼", "󰁽", "󰁾", "󰁿", "󰂀", "󰂁", "󰂂", "󰁹"]
BATTERY_CHARGING = ["󰢟", "󰢜", "󰂆", "󰂇", "󰂈", "󰢝", "󰂉", "󰢞", "󰂊", "󰂋", "󰂅"]

BOLD, RESET = "\033[1m", "\033[0m"


# --- helpers -----------------------------------------------------------------

def run(*cmd):
    """Output of a command, or "" when it fails."""
    try:
        return subprocess.run(cmd, capture_output=True, text=True, errors="replace",
                              timeout=3).stdout
    except (OSError, subprocess.SubprocessError):
        return ""


def act(*cmd):
    """Run an action the user asked for. A failure goes to stderr, which the
    bar writes to its log."""
    try:
        done = subprocess.run(cmd, capture_output=True, text=True, errors="replace",
                              timeout=10)
    except (OSError, subprocess.SubprocessError) as err:
        print(f"{cmd[0]}: {err}", file=sys.stderr, flush=True)
        return False
    if done.returncode != 0:
        print(f"{' '.join(cmd)}: {done.stderr.strip()}", file=sys.stderr, flush=True)
    return done.returncode == 0


def launch(*cmd):
    """Start a program detached from the bar (its output never reaches the bar)."""
    try:
        subprocess.Popen(cmd, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                         stderr=subprocess.DEVNULL, start_new_session=True)
    except OSError as err:
        print(f"{cmd[0]}: {err}", file=sys.stderr, flush=True)


def control(message):
    """Ask the bar for a popup: a control line is never shown as text."""
    print(f"\033]777;neferbar;{json.dumps(message)}\007", flush=True)


def level(icons, percent):
    """The icon of icons that matches percent (0..100)."""
    percent = max(0, min(100, percent))
    return icons[min(len(icons) - 1, percent * len(icons) // 101)]


def read(path):
    try:
        with open(path) as f:
            return f.read().strip()
    except OSError:
        return ""


def number(s, kind=int):
    """s as a number, 0 when it is not one."""
    try:
        return kind(s)
    except ValueError:
        return 0


def rgb(hex_color):
    h = hex_color.lstrip("#")
    return f"{int(h[0:2], 16)};{int(h[2:4], 16)};{int(h[4:6], 16)}"


def foreground(hex_color):
    return f"\033[38;2;{rgb(hex_color)}m"


def background(hex_color):
    return f"\033[48;2;{rgb(hex_color)}m"


# The bar drops blank cells at the end of a module; one painted with the bar's
# own background is kept, and keeps the last icon off the next module.
GAP = background(os.environ.get("NEFERBAR_BACKGROUND", "#000000")) + " " + "\033[0m"


# --- volume ------------------------------------------------------------------

class Volume:
    available = shutil.which("wpctl") is not None

    def read(self):
        # "Volume: 0.42" or "Volume: 0.42 [MUTED]"
        out = run("wpctl", "get-volume", SINK).split()
        self.percent = round(number(out[1], float) * 100) if len(out) > 1 else 0
        self.muted = "[MUTED]" in out

    def icon(self):
        if self.muted or self.percent == 0:
            return VOLUME_MUTED
        return level(VOLUME_ICONS, self.percent)

    def tooltip(self):
        device = re.search(r'node\.description = "(.*)"', run("wpctl", "inspect", SINK))
        return "Volume", [
            ("Level", f"{self.percent}%"),
            ("Muted", "yes" if self.muted else "no"),
            ("Output", device.group(1) if device else "?"),
        ]

    def menu(self):
        items = [("Mute", "check", self.muted, lambda: self.mute())]
        steps = [(f"{p}%", "radio", abs(self.percent - p) < 5, lambda p=p: self.set(p))
                 for p in (25, 50, 75, 100)]
        items += [None, ("Volume", steps), None, ("Mixer", lambda: launch(*MIXER))]
        return items

    def click(self, button):
        if button == "left":
            self.mute()
        elif button == "middle":
            launch(*MIXER)

    def scroll(self, direction, steps):
        sign = "+" if direction in ("up", "right") else "-"
        act("wpctl", "set-volume", "-l", "1.0", SINK, f"{steps * VOLUME_STEP}%{sign}")

    def mute(self):
        act("wpctl", "set-mute", SINK, "toggle")

    def set(self, percent):
        act("wpctl", "set-volume", SINK, f"{percent}%")


# --- wifi --------------------------------------------------------------------

class Wifi:
    available = shutil.which("nmcli") is not None

    def read(self):
        self.enabled = run("nmcli", "-t", "-f", "WIFI", "radio").strip() == "enabled"
        self.ethernet = False
        self.ssid, self.signal, self.freq = "", 0, ""
        for line in run("nmcli", "-t", "-f", "TYPE,STATE", "device").splitlines():
            kind, _, state = line.partition(":")
            if kind == "ethernet" and state == "connected":
                self.ethernet = True
        self.networks = []
        seen = set()
        # SSID comes last: it is the only field that can hold a ':'.
        for line in run("nmcli", "-t", "-f", "IN-USE,SIGNAL,FREQ,SSID",
                        "device", "wifi", "list", "--rescan", "no").splitlines():
            fields = line.split(":", 3)
            if len(fields) < 4:
                continue
            in_use, signal, freq = fields[0], number(fields[1]), fields[2]
            ssid = re.sub(r"\\(.)", r"\1", fields[3])  # nmcli writes ':' as '\:'
            if in_use == "*":
                self.ssid, self.signal, self.freq = ssid, signal, freq
            if ssid and ssid not in seen:
                seen.add(ssid)
                self.networks.append((ssid, signal))
        self.networks.sort(key=lambda n: -n[1])

    def icon(self):
        if self.ethernet:
            return ETHERNET
        if not self.enabled:
            return WIFI_OFF
        return level(WIFI_ICONS, self.signal) if self.ssid else WIFI_DOWN

    def tooltip(self):
        if self.ethernet:
            return "Ethernet", [("State", "connected")]
        if not self.enabled:
            return "Wi-Fi", [("State", "off")]
        if not self.ssid:
            return "Wi-Fi", [("State", "not connected")]
        return "Wi-Fi", [
            ("Network", self.ssid),
            ("Signal", f"{self.signal}%"),
            ("Frequency", self.freq),
        ]

    def menu(self):
        networks = [(f"{ssid}  {signal}%", "radio", ssid == self.ssid,
                     lambda ssid=ssid: self.connect(ssid))
                    for ssid, signal in self.networks[:15]]
        items = [("Wi-Fi", "check", self.enabled, lambda: self.toggle())]
        if self.enabled and networks:
            items += [None, ("Networks", networks)]
        items += [None, ("Network settings", lambda: self.settings())]
        return items

    def click(self, button):
        if button == "left":
            self.settings()

    def scroll(self, direction, steps):
        pass

    def toggle(self):
        act("nmcli", "radio", "wifi", "off" if self.enabled else "on")

    def connect(self, ssid):
        # Uses the saved profile; a new secured network needs the settings TUI.
        # The module waits for nmcli (at most 8 s) before it reads events again.
        if not act("nmcli", "--wait", "8", "device", "wifi", "connect", ssid):
            self.settings()

    def settings(self):
        launch(TERMINAL, "-e", *NETWORK_SETTINGS)


# --- battery -----------------------------------------------------------------

def find_battery():
    root = "/sys/class/power_supply"
    for name in sorted(os.listdir(root)) if os.path.isdir(root) else []:
        if read(f"{root}/{name}/type") == "Battery" and read(f"{root}/{name}/capacity"):
            return f"{root}/{name}"
    return ""


class Battery:
    path = find_battery()
    available = path != ""

    def read(self):
        value = lambda name: number(read(f"{self.path}/{name}"))
        self.percent = value("capacity")
        self.status = read(f"{self.path}/status")
        # Some batteries report a current and a charge (µA, µAh) instead of a
        # power and an energy (µW, µWh); some report them negative.
        volts = value("voltage_now") / 1_000_000
        self.watts = abs(value("power_now") or value("current_now") * volts) / 1_000_000
        self.now = value("energy_now") or value("charge_now") * volts
        self.full = value("energy_full") or value("charge_full") * volts

    def icon(self):
        icon = level(BATTERY_CHARGING if self.status == "Charging" else BATTERY_ICONS, self.percent)
        if self.status == "Discharging" and self.percent <= 20:
            color = foreground(os.environ.get("NEFERBAR_COLOR15", "#ffffff"))
            return f"{BOLD}{color}{icon}{RESET}"
        return icon

    def tooltip(self):
        rows = [("Charge", f"{self.percent}%"), ("State", self.status)]
        if self.watts:
            rows.append(("Power", f"{self.watts:.1f} W"))
        if self.watts and self.now:
            if self.status == "Discharging":
                rows.append(("Time left", hours(self.now / (self.watts * 1e6))))
            elif self.status == "Charging":
                rows.append(("Full in", hours((self.full - self.now) / (self.watts * 1e6))))
        profile = run("powerprofilesctl", "get").strip()
        if profile:
            rows.append(("Profile", profile))
        return "Battery", rows

    def menu(self):
        current = run("powerprofilesctl", "get").strip()
        profiles = re.findall(r"^\*?\s*([\w-]+):$", run("powerprofilesctl", "list"), re.M)
        # Switching profiles needs an active session (polkit): run the bar
        # from the compositor, not from an ssh shell.
        return [(p, "radio", p == current, lambda p=p: act("powerprofilesctl", "set", p))
                for p in profiles]

    def click(self, button):
        pass

    def scroll(self, direction, steps):
        pass


def hours(h):
    return f"{int(h)}h{int(h * 60) % 60:02d}"


# --- the module --------------------------------------------------------------

class Module:
    def __init__(self):
        self.parts = [p() for p in (Volume, Wifi, Battery) if p.available]
        self.cols = {}  # column -> part
        self.last_frame = None
        self.hovered = None
        self.tooltip_at = None
        self.tooltip_shown = False
        self.actions = {}  # menu item id -> function, for the open menu
        self.menu_token = None
        self.menu_part = None
        self.read(all_parts=True)

    def read(self, all_parts=False):
        for part in self.parts:
            if all_parts or not isinstance(part, Wifi):
                part.read()

    def draw(self):
        # One icon per part, separated by a space: part i is at column 2*i.
        self.cols = {2 * i: part for i, part in enumerate(self.parts)}
        frame = " ".join(part.icon() for part in self.parts) + GAP
        if frame != self.last_frame:
            self.last_frame = frame
            print(frame, flush=True)
        if self.tooltip_shown:
            self.show_tooltip()  # the bar ignores an unchanged tooltip

    def col_of(self, part):
        return next(c for c, p in self.cols.items() if p is part)

    def show_tooltip(self):
        part = self.hovered
        if part is None:
            return
        # A tooltip has a bold title, then a "body" (text, "\n" for a new
        # line) and/or "rows", a table whose columns line up.
        title, rows = part.tooltip()
        control({"type": "tooltip", "col": self.col_of(part), "width": 1,
                 "title": title, "rows": rows})
        self.tooltip_shown = True

    def hide_tooltip(self):
        if self.tooltip_shown:
            control({"type": "close"})
        self.tooltip_shown = False
        self.tooltip_at = None

    def open_menu(self, part, token):
        self.actions = {}
        items = self.items(part.menu())
        if not items:
            return  # the bar refuses an empty menu
        self.menu_token, self.menu_part = token, part
        control({"type": "menu", "col": self.col_of(part), "width": 1,
                 "click": token, "items": items})

    def items(self, entries):
        """Turn (label, action), (label, kind, checked, action), (label, [entries])
        and None (a separator) into menu items, numbering the actions."""
        out = []
        for entry in entries:
            if entry is None:
                out.append({"id": 0, "kind": "separator"})
            elif isinstance(entry[1], list):
                out.append({"id": 0, "label": entry[0], "items": self.items(entry[1])})
            else:
                if len(entry) == 2:  # (label, action)
                    entry = (entry[0], "normal", False, entry[1])
                label, kind, checked, action = entry
                item_id = len(self.actions) + 1
                self.actions[item_id] = action
                out.append({"id": item_id, "label": label, "kind": kind, "checked": checked})
        return out

    def handle(self, line):
        event, *args = line.split()
        part = self.cols.get(int(args[-1])) if event in ("hover", "scroll") and args else None
        if event == "hover":
            if part is not self.hovered:
                self.hide_tooltip()
                self.hovered = part
                self.tooltip_at = time.monotonic() + TOOLTIP_DELAY if part else None
            return
        if event == "leave":
            self.hovered = None
            self.hide_tooltip()
            return
        if event == "click":
            button, col, token = args
            self.hide_tooltip()
            part = self.cols.get(int(col))
            if part is None:
                return
            if button == "right":
                self.open_menu(part, int(token))
                return
            part.click(button)
        elif event == "scroll" and part is not None:
            direction, steps, _ = args
            part.scroll(direction, int(steps))
            if self.tooltip_shown:
                # The bar opens one tooltip per 250 ms: show the new level
                # once the wheel stops, in case the last one was dropped.
                self.tooltip_at = time.monotonic() + TOOLTIP_DELAY
        elif event in ("menu-activate", "menu-closed"):
            if int(args[0]) != self.menu_token:
                return  # an older menu
            action = self.actions.get(int(args[1])) if event == "menu-activate" else None
            part, self.actions, self.menu_token, self.menu_part = self.menu_part, {}, None, None
            if action is None:
                return
            action()
        else:
            return
        part.read()
        self.draw()

    def run(self):
        self.draw()
        pending = b""
        next_read = next_wifi = time.monotonic()
        while True:
            now = time.monotonic()
            deadlines = [next_read, next_wifi] + ([self.tooltip_at] if self.tooltip_at else [])
            ready, _, _ = select.select([0], [], [], max(0, min(deadlines) - now))
            if ready:
                chunk = os.read(0, 4096)
                if not chunk:  # stdin is empty when the module is not interactive
                    sys.exit("status.py needs interactive = true")
                pending += chunk
                while b"\n" in pending:
                    line, pending = pending.split(b"\n", 1)
                    self.handle(line.decode(errors="replace"))
            now = time.monotonic()
            if self.tooltip_at and now >= self.tooltip_at:
                self.tooltip_at = None
                self.show_tooltip()
            if now >= next_wifi:
                next_wifi = now + WIFI_REFRESH
                next_read = now + REFRESH
                self.read(all_parts=True)
                self.draw()
            elif now >= next_read:
                next_read = now + REFRESH
                self.read()
                self.draw()


if __name__ == "__main__":
    Module().run()
