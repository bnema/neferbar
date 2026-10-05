# neferbar

A one-cell-high, terminal-style status bar for Wayland (wlr-layer-shell). Modules are scripts that print ANSI text; the bar draws it with a monospace Nerd Font through raw Vulkan. It is read-only, follows the monitor's fractional scale, and renders only when a module changes, so an animated module can reach the compositor's frame rate and an idle bar costs nothing.

## Requirements

- A compositor with `zwlr_layer_shell_v1`, `zwp_linux_dmabuf_v1` v4 and `wp_linux_drm_syncobj_v1` (and, for fractional scale, `wp_fractional_scale_v1` with `wp_viewporter`). GNOME is not supported.
- Linux 6.6 or newer, a Vulkan 1.3 driver with DMA-BUF export, and a Nerd Font installed (`fc-match` must find it).
- Go 1.27.

## Build and run

```sh
go build -o neferbar ./cmd/neferbar
./neferbar -config examples/config.toml
```

Flags: `-config <file>`, `-pprof <loopback addr>`, `-memstats <interval>`.

## Configuration

`$XDG_CONFIG_HOME/neferbar/config.toml`:

```toml
[bar]
font = "JetBrainsMono Nerd Font Mono"
size = 14            # logical pixels
scale = 1.0          # multiplier on top of the monitor scale
output = ""          # wl_output name; empty lets the compositor choose
background = "#1e1e2e"
foreground = "#cdd6f4"

[[module]]
name = "clock"
zone = "center"      # left | center | right
exec = "clock.sh"
```

## Modules

A module is a long-running `/bin/sh -c` command. Its stdout is ANSI text (SGR colors, bold, inverse, dim). A frame ends at a newline or a form feed, and each frame replaces the previous one. Only the latest frame matters: a script that prints faster than the bar draws overwrites its pending frame.

- Animate by printing frames quickly, ending each with `\f`. See `examples/modules/rainbow.sh`.
- A module that exits is restarted with backoff and shows `[name!]` while it is down. Its stderr goes to the bar's log.
- Left and right zones keep their text; the center is cut first when they would overlap.

## Height and scale

The bar is exactly one cell high: `cell = ceil(size × scale × monitor scale)` physical pixels, and the layer surface height is `ceil(cell / monitor scale)` logical pixels. When the monitor scale changes the font is rebuilt, and the surface is recreated if its height changes.

## Profiling

`-pprof localhost:6060` serves the pprof handlers on a loopback address and records every allocation. `-memstats 5s` logs allocation counters. The tests assert zero allocations per frame for the draw path, the layout and the module hand-off.

## Testing

```sh
go test ./...
scripts/headless.sh examples/config.toml 5   # nested headless NeferWL, screenshots in /tmp/neferbar-shots
SCALE=1.5 SIZE=1200x200 scripts/headless.sh examples/config.toml 5
```

The GPU tests skip when no render node or Vulkan device is available.

## License

MIT.
