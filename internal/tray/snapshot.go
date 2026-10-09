package tray

import "sync"

// Target is what a click on the tray line reaches: one visible item and the
// columns of its icon in the printed line.
type Target struct {
	Dest   string // where to send calls: the owner's unique name, else the registered name
	Path   string // the object path of the StatusNotifierItem
	Menu   string // the object path of its dbusmenu, "" when it has none
	IsMenu bool   // the item only offers a menu: Activate means "open it"
	Start  int    // first column of the icon in the line
	Width  int    // cells the icon takes
}

// snapshot is the list of targets of the last line printed. The main loop
// writes it; the control goroutine reads it.
type snapshot struct {
	mu      sync.Mutex
	targets []Target
}

// publish replaces the targets with a copy of ts.
func (s *snapshot) publish(ts []Target) {
	s.mu.Lock()
	s.targets = append(s.targets[:0], ts...)
	s.mu.Unlock()
}

// at returns the target whose icon covers col.
func (s *snapshot) at(col int) (Target, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.targets {
		if col >= t.Start && col < t.Start+t.Width {
			return t, true
		}
	}
	return Target{}, false
}

// target returns the visible item whose icon covers column col of the line.
func (t *Tray) target(col int) (Target, bool) { return t.snap.at(col) }
