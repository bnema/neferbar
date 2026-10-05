package app

import (
	"encoding/binary"
	"testing"
)

func state(vals ...uint32) []byte {
	var b []byte
	for _, v := range vals {
		b = binary.NativeEndian.AppendUint32(b, v)
	}
	return b
}

// open maps a window: its properties and its state, then the done that applies them.
func open(t *Tracker, h uint32, id, title string, st ...uint32) {
	t.ID(h, id)
	t.Title(h, title)
	t.State(h, state(st...))
	t.Done(h)
}

func TestNothingFocusedIsTheZeroWindow(t *testing.T) {
	tr := NewTracker()
	if tr.Focused() != (Window{}) {
		t.Fatal("an empty tracker must have no focus")
	}
	open(tr, 1, "a", "A") // mapped, not activated
	if tr.Focused() != (Window{}) {
		t.Fatalf("a window without the activated state has no focus: %+v", tr.Focused())
	}
}

func TestFocusFollowsTheActivatedState(t *testing.T) {
	tr := NewTracker()
	open(tr, 1, "org.mozilla.firefox", "Docs", 0, stateActivated) // maximized + activated
	open(tr, 2, "kitty", "~")
	if got := tr.Focused(); got != (Window{"org.mozilla.firefox", "Docs"}) {
		t.Fatalf("focused = %+v", got)
	}
	// Focus moves: 1 loses it, 2 gets it.
	open(tr, 1, "org.mozilla.firefox", "Docs", 0)
	open(tr, 2, "kitty", "~", stateActivated)
	if got := tr.Focused(); got != (Window{"kitty", "~"}) {
		t.Fatalf("after the focus moved: %+v", got)
	}
}

func TestATitleChangeAppliesOnlyAtDone(t *testing.T) {
	tr := NewTracker()
	open(tr, 1, "kitty", "one", stateActivated)
	tr.Title(1, "two")
	if got := tr.Focused().Title; got != "one" {
		t.Fatalf("title changed before done: %q", got)
	}
	tr.Done(1)
	if got := tr.Focused().Title; got != "two" {
		t.Fatalf("title after done: %q", got)
	}
}

func TestClosingTheFocusedWindowClearsTheFocus(t *testing.T) {
	tr := NewTracker()
	open(tr, 1, "kitty", "~", stateActivated)
	tr.Closed(1)
	if tr.Focused() != (Window{}) {
		t.Fatalf("a closed window still has the focus: %+v", tr.Focused())
	}
}

func TestTheWindowThatGotTheFocusLastWins(t *testing.T) {
	tr := NewTracker()
	open(tr, 1, "a", "A", stateActivated)
	open(tr, 2, "b", "B", stateActivated) // two seats, two focused windows
	if got := tr.Focused().ID; got != "b" {
		t.Fatalf("focused = %q, want b (the newest)", got)
	}
	open(tr, 1, "a", "A2", stateActivated) // still active, only its title changed
	if got := tr.Focused().ID; got != "b" {
		t.Fatalf("a title change must not steal the focus: %q", got)
	}
}

func TestCleanRemovesControlCharacters(t *testing.T) {
	in := "fire\x1b[2Jfox\n\ttab\x07 ok \u00e9"
	if got, want := Clean(in), "fire[2Jfoxtab ok \u00e9"; got != want {
		t.Fatalf("Clean = %q, want %q", got, want)
	}
}

func TestNameIsTheLastPartOfTheID(t *testing.T) {
	for id, want := range map[string]string{
		"com.github.bnema.dumber": "dumber",
		"org.mozilla.firefox":     "firefox",
		"kitty":                   "kitty",
		"":                        "",
		"trailing.":               "trailing.",
	} {
		if got := Name(id); got != want {
			t.Errorf("Name(%q) = %q, want %q", id, got, want)
		}
	}
}
