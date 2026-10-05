package gpu

import (
	"os"
	"syscall"
	"testing"

	"git.bnema.dev/bnema/neferbar/internal/glyph"
	"git.bnema.dev/bnema/neferbar/internal/racecheck"
	"git.bnema.dev/bnema/neferbar/internal/syncobj"
)

// newTestRenderer needs a Vulkan device with DMA-BUF export; it skips without one.
func newTestRenderer(t *testing.T) (*Renderer, *Device, *syncobj.Node) {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat("/dev/dri/renderD128", &st); err != nil {
		t.Skip("no render node")
	}
	dev, err := Open(uint64(st.Rdev))
	if err != nil {
		t.Skipf("no matching Vulkan device: %v", err)
	}
	t.Cleanup(dev.Close)
	node, err := syncobj.Open("/dev/dri/renderD128")
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { node.Close() })
	mods, err := dev.ExportableModifiers()
	if err != nil || len(mods) == 0 {
		t.Skip("no exportable modifier")
	}
	var mod Modifier
	for m := range mods {
		mod = Modifier(m)
		break
	}
	paths, err := glyph.FindFonts("monospace")
	if err != nil {
		t.Skip(err)
	}
	if _, err := os.Stat(paths[0]); err != nil {
		t.Skip(err)
	}
	face, err := glyph.Load(paths, 16)
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(face.Close)
	r, err := NewRenderer(dev, node, mod, face, 800, int32(face.CellH))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r, dev, node
}

// TestDrawSteadyStateAllocs reports the allocations of one Draw once the
// atlas is warm: the budget is what the Vulkan bindings force per call.
func TestDrawSteadyStateAllocs(t *testing.T) {
	if racecheck.Enabled {
		t.Skip("the race detector allocates")
	}
	r, _, node := newTestRenderer(t)
	cells := make([]Cell, r.Cols())
	for i := range cells {
		cells[i] = Cell{Rune: rune('a' + i%26), FG: [3]uint8{255, 255, 255}, BG: [3]uint8{0, 0, 40}}
	}
	var f Frame
	step := func() {
		ok, err := r.Draw(cells, [3]uint8{}, &f)
		if err != nil || !ok {
			t.Fatalf("draw ok=%v err=%v", ok, err)
		}
		// Play the compositor: signal the release point, then hand it back.
		s := &r.slots[f.Slot]
		if err := node.Signal(s.releaseHandle, s.releasePoint); err != nil {
			t.Fatal(err)
		}
		if err := r.Released(f.Slot); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 8; i++ { // warm: atlas upload, lazily created state
		step()
	}
	got := testing.AllocsPerRun(200, step)
	t.Logf("allocations per Draw+Released: %.1f", got)
	if got > 0 {
		t.Errorf("steady-state Draw allocates %.1f objects; want 0", got)
	}
}
