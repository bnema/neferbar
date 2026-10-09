package tray

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxTipBytes bounds a tooltip text sent to the bar.
const maxTipBytes = 512

// plainText turns the markup an application puts in a tooltip into plain
// text: tags are dropped, <br> becomes a newline, and the common entities are
// resolved. The result is cut to maxTipBytes at a rune boundary. A "<" with no
// closing ">" is kept as text.
func plainText(s string) string {
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < maxTipBytes+utf8.UTFMax; {
		switch c := s[i]; {
		case c == '<':
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				b.WriteByte(c)
				i++
				continue
			}
			if isBreak(s[i+1 : i+end]) {
				b.WriteByte('\n')
			}
			i += end + 1
		case c == '&':
			if r, n, ok := entity(s[i:]); ok {
				b.WriteRune(r)
				i += n
				continue
			}
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return cutRunes(b.String(), maxTipBytes)
}

// isBreak reports whether the inside of a tag is <br>, <br/> or <br />.
func isBreak(tag string) bool {
	tag = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(tag), "/"))
	return strings.EqualFold(tag, "br")
}

// entity decodes the entity at the start of s ("&amp;", "&#65;", "&#x41;").
func entity(s string) (r rune, n int, ok bool) {
	end := strings.IndexByte(s, ';')
	if end < 2 || end > 10 {
		return 0, 0, false
	}
	switch name := s[1:end]; name {
	case "amp":
		return '&', end + 1, true
	case "lt":
		return '<', end + 1, true
	case "gt":
		return '>', end + 1, true
	case "quot":
		return '"', end + 1, true
	case "apos":
		return '\'', end + 1, true
	default:
		if name[0] != '#' {
			return 0, 0, false
		}
		num, base := name[1:], 10
		if len(num) > 0 && (num[0] == 'x' || num[0] == 'X') {
			num, base = num[1:], 16
		}
		v, err := strconv.ParseUint(num, base, 32)
		if err != nil || !utf8.ValidRune(rune(v)) || v == 0 {
			return 0, 0, false
		}
		return rune(v), end + 1, true
	}
}

// cutRunes cuts s to at most max bytes without splitting a rune.
func cutRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}
