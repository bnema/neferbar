# neferbar

A dead simple status bar for Wayland: **one line of terminal text, nothing else.**

Each part of the bar is a script. A script prints text, and neferbar shows it. If you can write `echo`, you can write a module.

```
 workspace 2   firefox                     14:32:07                     cpu 12%  vol 40%  bat 88%
```

- **Text only.** ASCII, Unicode and Nerd Font icons. No images, no widgets, no clicks.
- **Exactly one character high.** The bar is as tall as a terminal row, and it follows your monitor's scale.
- **Scripts do the work.** Any language, anything that prints to stdout.
- **Fast when you ask.** Print 60 times a second and the bar redraws 60 times a second. Print nothing and it uses no CPU or GPU.

It needs a compositor with `wlr-layer-shell`, such as Sway, Hyprland, Niri, River or NeferWL. GNOME is not supported.

## Install

You need Go 1.27, a Vulkan 1.3 GPU driver, and a [Nerd Font](https://www.nerdfonts.com/).

```sh
go build -o neferbar ./cmd/neferbar
./neferbar
```

Start it from your compositor's autostart. For NeferWL:

```
startup = /full/path/to/neferbar
```

## Configure

Create `~/.config/neferbar/config.toml`:

```toml
[bar]
font = "JetBrainsMono Nerd Font Mono"   # use a "Mono" Nerd Font so icons fit one cell
size = 14                                # text size
background = "#1e1e2e"
foreground = "#cdd6f4"

[[module]]
name = "clock"          # a unique name
zone = "center"         # left, center or right
exec = "~/.config/neferbar/clock.sh"
```

Add one `[[module]]` block per script. Save the file and the bar updates immediately. If you make a typo, the bar keeps the old config and prints the error.

| Setting | Default | Meaning |
|---|---|---|
| `bar.font` | `JetBrainsMono Nerd Font Mono` | A font family known to fontconfig (`fc-list`). |
| `bar.size` | `14` | Text size in logical pixels. |
| `bar.scale` | `1.0` | Extra zoom on top of the monitor scale. |
| `bar.output` | any | A monitor name such as `HDMI-A-1`. Needs a restart. |
| `bar.background`, `bar.foreground` | catppuccin | `#rrggbb`. |

If the font is not installed, neferbar warns and falls back to another font, and icons may be missing.

## Write a script

A module is any program that prints lines. **Each line replaces what the module showed before.**

The smallest module:

```sh
#!/bin/sh
echo "hello"
sleep 100000
```

A clock, updated every second:

```sh
#!/bin/sh
while true; do
    date +%H:%M:%S
    sleep 1
done
```

Make it executable (`chmod +x clock.sh`) and put its path in `exec`. That's all.

Rules:

1. Print one line, then print another line later. The new line replaces the old one.
2. A frame ends at a newline (`\n`) or a form feed (`\f`). They mean the same thing. Text after the last one, with no ending yet, is not shown until its frame is finished.
3. Keep the script running. A script that exits is restarted after a short delay, and the bar shows `[name!]` in the meantime.
4. Print errors to stderr. They go to the bar's log and not to the screen.
5. Frames are cut to 16 KiB, and text wider than the bar is clipped.

### Colors and styles

Use ordinary terminal escape codes. If it works in a terminal, it works here.

```sh
printf '\033[32mOK\033[0m  \033[1;31mFAIL\033[0m\n'
```

| Code | Effect |
|---|---|
| `\033[1m` | **bold** |
| `\033[2m` | dim |
| `\033[3m` | *italic* |
| `\033[4m` | underline |
| `\033[7m` | inverse |
| `\033[9m` | ~~strikethrough~~ |
| `\033[30m`..`\033[37m` | foreground colors |
| `\033[40m`..`\033[47m` | background colors |
| `\033[38;5;Nm` / `\033[48;5;Nm` | 256-color palette |
| `\033[38;2;R;G;Bm` / `\033[48;2;R;G;Bm` | true color |
| `\033[0m` | reset |

Italic, bold and bold italic use the font's own files when it has them.

### Icons

Nerd Font icons are ordinary characters. Print them from a script with their UTF-8 bytes:

```sh
printf '\357\200\227 %s\n' "$(date +%H:%M)"     # clock icon
```

Find icon codes at <https://www.nerdfonts.com/cheat-sheet>.

### Animation

Print faster. Each frame ends with `\f`:

```sh
#!/bin/sh
# a spinner at 20 fps
while true; do
    for c in '|' '/' '-' '\'; do
        printf '%s\f' "$c"
        sleep 0.05
    done
done
```

neferbar draws at most as often as your monitor refreshes. If a script prints faster than that, only the latest frame is drawn. A script that prints nothing costs nothing.

### Example modules

The `examples/modules` directory has three:

- `static.sh`: one line, then idle.
- `clock.sh`: a clock with an icon.
- `rainbow.sh [fps] [width]`: a 60 fps scrolling rainbow.

## Layout

The bar has three zones. Modules in the same zone appear side by side, in the order of the config file.

- **left** starts at the left edge.
- **right** ends at the right edge.
- **center** sits in the middle of the space that is left. If the bar is too narrow, the center is cut first.

## Command line

```
neferbar [-config file] [-display socket] [-pprof addr] [-memstats interval]
```

| Flag | Meaning |
|---|---|
| `-config` | Config file. Default: `$XDG_CONFIG_HOME/neferbar/config.toml`. |
| `-display` | Wayland socket name or path. Default: `$NEFERBAR_DISPLAY`, then `$WAYLAND_DISPLAY`. |
| `-pprof` | Serve Go profiling on a loopback address, such as `localhost:6060`. |
| `-memstats` | Log allocation counters at this interval. |

## Test without touching your desktop

```sh
go test ./...
scripts/headless.sh examples/config.toml 5        # runs in a nested headless NeferWL
SCALE=1.5 SIZE=1200x200 scripts/headless.sh examples/config.toml 5
```

Screenshots land in `/tmp/neferbar-shots`.

## License

MIT.
