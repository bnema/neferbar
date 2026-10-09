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
whose tool is missing is not shown. Edit the commands below to taste.
"""

import json
import os
import re
import select
import shutil
import subprocess
import time

TERMINAL = os.environ.get("TERMINAL", "kitty")
MIXER = ["pavucontrol"]
NETWORK_SETTINGS = ["nmtui"]  # runs in TERMINAL; impala if you use iwd
VOLUME_STEP = 2  # percent per wheel step
TOOLTIP_DELAY = 0.4  # seconds of hover before a tooltip
REFRESH = 2.0  # seconds between two reads of volume and battery
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
        return subprocess.run(cmd, capture_output=True, text=True, timeout=3).stdout
    except (OSError, subprocess.SubprocessError):
        return ""


def launch(*cmd):
    """Start a program detached from the bar (its output never reaches the bar)."""
    try:
        subprocess.Popen(cmd, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                         stderr=subprocess.DEVNULL, start_new_session=True)
    except OSError:
        pass


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


def foreground(hex_color):
    h = hex_color.lstrip("#")
    return f"\033[38;2;{int(h[0:2], 16)};{int(h[2:4], 16)};{int(h[4:6], 16)}m"


# --- volume ------------------------------------------------------------------

class Volume:
    available = shutil.which("wpctl") is not None

    def read(self):
        # "Volume: 0.42" or "Volume: 0.42 [MUTED]"
        out = run("wpctl", "get-volume", SINK).split()
        self.percent = round(float(out[1]) * 100) if len(out) > 1 else 0
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
        run("wpctl", "set-volume", "-l", "1.0", SINK, f"{steps * VOLUME_STEP}%{sign}")

    def mute(self):
        run("wpctl", "set-mute", SINK, "toggle")

    def set(self, percent):
        run("wpctl", "set-volume", SINK, f"{percent}%")


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
        for line in run("nmcli", "-t", "-f", "IN-USE,SSID,SIGNAL,FREQ,SECURITY",
                        "device", "wifi", "list", "--rescan", "no").splitlines():
            in_use, ssid, signal, freq, security = split_terse(line)
            if in_use == "*":
                self.ssid, self.signal, self.freq = ssid, int(signal or 0), freq
            if ssid and ssid not in seen:
                seen.add(ssid)
                self.networks.append((ssid, int(signal or 0), security))
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
                    for ssid, signal, _ in self.networks[:15]]
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
        run("nmcli", "radio", "wifi", "off" if self.enabled else "on")

    def connect(self, ssid):
        # Uses the saved profile; a new secured network needs the settings TUI.
        if not run("nmcli", "device", "wifi", "connect", ssid):
            self.settings()

    def settings(self):
        launch(TERMINAL, "-e", *NETWORK_SETTINGS)


def split_terse(line):
    """Split an nmcli -t line: fields are separated by ':', and '\\:' is a colon."""
    fields = [f.replace("\\:", ":").replace("\\\\", "\\")
              for f in re.split(r"(?<!\\):", line)]
    return (fields + [""] * 5)[:5]


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
        self.percent = int(read(f"{self.path}/capacity") or 0)
        self.status = read(f"{self.path}/status")
        power = int(read(f"{self.path}/power_now") or 0)
        if not power:  # some batteries report a current instead of a power
            current = int(read(f"{self.path}/current_now") or 0)
            power = current * int(read(f"{self.path}/voltage_now") or 0) // 1_000_000
        self.watts = power / 1_000_000
        self.now = int(read(f"{self.path}/energy_now") or 0)
        self.full = int(read(f"{self.path}/energy_full") or 0)

    def icon(self):
        icons = BATTERY_CHARGING if self.status == "Charging" else BATTERY_ICONS
        icon = icons[min(10, self.percent // 10)]
        if self.status == "Discharging" and self.percent <= 20:
            color = foreground(os.environ.get("NEFERBAR_COLOR15", "#ffffff"))
            return f"{BOLD}{color}{icon}{RESET}"
        return icon

    def tooltip(self):
        rows = [("Charge", f"{self.percent}%"), ("State", self.status)]
        if self.watts:
            rows.append(("Power", f"{self.watts:.1f} W"))
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
        return [(p, "radio", p == current, lambda p=p: run("powerprofilesctl", "set", p))
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
        self.read(all_parts=True)

    def read(self, all_parts=False):
        for part in self.parts:
            if all_parts or not isinstance(part, Wifi):
                part.read()

    def draw(self):
        # One icon per part, separated by a space: part i is at column 2*i.
        self.cols = {2 * i: part for i, part in enumerate(self.parts)}
        # A trailing space keeps the last icon off the next module.
        frame = " ".join(part.icon() for part in self.parts) + " "
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
        part.read()
        self.actions = {}
        control({"type": "menu", "col": self.col_of(part), "width": 1,
                 "click": token, "items": self.items(part.menu())})

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
        elif event == "menu-activate":
            action = self.actions.pop(int(args[1]), None)
            self.actions = {}
            if action:
                action()
        else:
            return  # menu-closed
        self.read(all_parts=True)
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
                if not chunk:
                    return  # the bar is gone
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
