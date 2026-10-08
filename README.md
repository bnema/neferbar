# neferbar

A dead simple status bar for Wayland: **one line of terminal text, nothing else.**

Each part of the bar is a script. A script prints text, and neferbar shows it. If you can write `echo`, you can write a module.

![The bundled bar on a NeferWL desktop with a terminal and an empty Firefox: workspaces on the left, the focused app and its title in the middle, the clock on the right](docs/img/bundle.png)

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
theme = "auto"                           # colors of your terminal, or the path of a theme file

[[module]]
name = "clock"          # a unique name
zone = "center"         # left, center or right
exec = "~/.config/neferbar/clock.sh"
```

Add one `[[module]]` block per script. Save the file and the bar updates immediately. If you make a typo, the bar keeps the old config and prints the error.

The bar also watches the folder of each script. Edit a script, or a helper it sources from the same folder, and the modules in that folder restart by themselves. Hidden files (`.name`) and backups (`name~`) are ignored, so an editor's swap file does nothing.

| Setting | Default | Meaning |
|---|---|---|
| `bar.font` | `JetBrainsMono Nerd Font Mono` | A font family known to fontconfig (`fc-list`). |
| `bar.size` | `14` | Text size in logical pixels. |
| `bar.scale` | `1.0` | Extra zoom on top of the monitor scale. |
| `bar.position` | `top` | `top` or `bottom`: the screen edge the bar sits on. Applies live. |
| `bar.output` | any | A monitor name such as `HDMI-A-1`. Needs a restart. |
| `bar.theme` | `auto` | `auto` reads the colors of the terminal you use. Or the path of a theme file (see Colors). |
| `bar.accent` | `4` | Which of the theme's 16 colors (0-15) the highlights use. 4 is the ANSI blue; try 2 (green), 6 (cyan) or 5 (magenta) to match your theme. |
| `bar.background`, `bar.foreground` | from the theme | `#rrggbb`. Set them to override the theme. |

If the font is not installed, neferbar warns and falls back to another font, and icons may be missing.

## Colors

The bar takes its colors from your terminal, so it matches the rest of your desktop.

```toml
theme = "auto"                                  # the terminal you use
theme = "~/.config/kitty/themes/mytheme.conf"   # or one file
```

`auto` reads the theme of the terminal named by the **`$TERMINAL`** environment variable, and nothing else. It knows kitty, ghostty, foot and alacritty (`TERMINAL=kitty`, or a path such as `/usr/bin/foot`). A theme file can be any of those formats, and the bar recognizes it by its content. Includes and ghostty `theme =` names are followed. Colors the file does not set come from a built-in dark theme.

**Set `$TERMINAL` for your whole session**, not only in a shell. A variable you `set -x` in an open terminal is not seen by programs your compositor starts. The simplest place is a systemd user environment file, read when a session starts, whatever your shell is:

```
# ~/.config/environment.d/10-terminal.conf
TERMINAL=kitty
```

Then start a new session. If `$TERMINAL` is missing, the bar says so in its log and uses the built-in colors.

The bar watches every file it read: change your terminal's theme and the bar changes with it. If the theme cannot be read, the bar uses the built-in colors and says why in its log.

Scripts get the colors as environment variables, each `#rrggbb`, so a script never hard-codes one:

| Variable | Color |
|---|---|
| `NEFERBAR_BACKGROUND`, `NEFERBAR_FOREGROUND` | the bar's own background and text |
| `NEFERBAR_ACCENT` | the highlight color, the theme color that `bar.accent` picks |
| `NEFERBAR_COLOR0` .. `NEFERBAR_COLOR15` | the theme's 16 ANSI colors |

The 16 ANSI codes a script prints (`\033[31m` red, `\033[44m` blue background and so on) use the theme as well.

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

Nerd Font icons are ordinary characters, but GitHub cannot draw them in a code block, so the examples here write them as escape sequences. Give `printf` the icon's code point:

```sh
printf '\Uf017 %s\n' "$(date +%H:%M)"      # clock icon (U+F017)
```

`\U` works in bash. Some minimal shells do not support it; the UTF-8 bytes work everywhere: `printf '\357\200\227 %s\n' "$(date +%H:%M)"`.

Find icon names and code points at <https://www.nerdfonts.com/cheat-sheet>. In an editor with a Nerd Font you can also paste the icon itself into the script.

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

### The bundled bar

`examples/bundle` is a ready-made bar: workspaces on the left, the focused app in the middle, the clock on the right, in your terminal's colors.

 Copy it to `~/.config/neferbar/` (the install steps are at the top of `examples/bundle/config.toml`).

Its helper `lib.sh` has what a themed script needs: `fg`/`bg` to set a color from `#rrggbb`, `mix` to blend two, `fade` for the soft edge, and `state_changed_loop` to react to NeferWL. Read the three scripts: each is about thirty lines.

On NeferWL the workspaces show one dot per window and the stash. On other compositors the workspace and focus modules show nothing yet, and the clock still works.

### Reacting to your compositor (workspace effects)

A module can watch anything, including the compositor's own state. NeferWL keeps a JSON file up to date and passes its path to every program it starts as `$NEFERWL_STATE`. It replaces the file on each change, so a module only has to check the file's modification time:

```sh
stamp=$(mktemp)
touch -r "$NEFERWL_STATE" "$stamp"
while sleep 0.1; do
    if [[ $NEFERWL_STATE -nt $stamp ]]; then       # the file changed
        touch -r "$NEFERWL_STATE" "$stamp"
        jq -r '.outputs[] | "\(.name) is on workspace \(.active)"' "$NEFERWL_STATE"
    fi
done
```

That is all it takes to draw an effect when you switch workspace. `examples/bundle/left.sh` redraws the workspaces that way.

### Example modules

The `examples/modules` directory has these:

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
| `-pprof` | Serve Go profiling on a loopback address, such as `localhost:6060`. Default: `$NEFERBAR_PPROF`, so a bar started by the compositor can be profiled. |
| `-memstats` | Log allocation counters at this interval. |

## Reading the focused window

`neferbar app` prints fields of the window that has the focus: one line now, then a new one each time the focus or a title changes. It works on compositors that offer `zwlr_foreign_toplevel_manager_v1` (NeferWL, Sway, Hyprland, River, Niri).

```
neferbar app title            # Dumber - docs
neferbar app id               # com.github.bnema.dumber
neferbar app name             # dumber (the part of the id after the last dot)
neferbar app name title       # several fields, separated by a tab
neferbar app -once title      # print once and exit
```

A line is empty when no window has the focus. Control characters are removed from every field, so a title cannot move a terminal's cursor. Scripts started by the bar find the binary in `$NEFERBAR_BIN`.

## Test without touching your desktop

```sh
go test ./...
scripts/headless.sh examples/config.toml 5        # runs in a nested headless NeferWL
SCALE=1.5 SIZE=1200x200 scripts/headless.sh examples/config.toml 5
```

Screenshots land in `/tmp/neferbar-shots`.

## License

MIT.
