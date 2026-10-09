package popup

// xkb keysyms the menu reacts to.
const (
	keysymBackSpace = 0xff08
	keysymTab       = 0xff09
	keysymEscape    = 0xff1b
	keysymLeft      = 0xff51
	keysymUp        = 0xff52
	keysymDown      = 0xff54
)

// keyAction is what a key press does in a menu.
type keyAction uint8

const (
	keyForward  keyAction = iota // pass the key to nefergui unchanged
	keyNext                      // move the focus to the next entry (Tab)
	keyPrevious                  // move the focus to the previous entry (Shift+Tab)
	keyBack                      // leave the submenu
	keyClose                     // dismiss the menu
)

// mapKey says what a key does. Up and Down walk the entries, Left and
// BackSpace leave a submenu, Escape dismisses; everything else (Tab, Return,
// Space…) goes to nefergui. Left and BackSpace do nothing at the top level.
func mapKey(keysym uint32, inSubmenu bool) keyAction {
	switch keysym {
	case keysymDown:
		return keyNext
	case keysymUp:
		return keyPrevious
	case keysymEscape:
		return keyClose
	case keysymLeft, keysymBackSpace:
		if inSubmenu {
			return keyBack
		}
		return keyForward
	}
	return keyForward
}
