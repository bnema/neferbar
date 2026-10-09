package tray

import (
	"strings"
	"testing"
	"time"
)

func TestPlainText(t *testing.T) {
	for in, want := range map[string]string{
		"":                      "",
		"plain":                 "plain",
		"<b>bold</b> text":      "bold text",
		"a<br>b":                "a\nb",
		"a<br/>b<BR />c":        "a\nb\nc",
		"<p style=\"x\">hi</p>": "hi",
		"Tom &amp; Jerry":       "Tom & Jerry",
		"&lt;tag&gt; &quot;q&quot; &apos;s&apos;": "<tag> \"q\" 's'",
		"&#65;&#x42;&#X43;":                       "ABC",
		"&#0;&#xD800;&#99999999999;":              "&#0;&#xD800;&#99999999999;",
		"&unknown; &amp":                          "&unknown; &amp",
		"1 < 2":                                   "1 < 2",
		"<<b>x</b>":                               "x",
		"<img src='x>y'>z":                        "y'>z",
		"\u00e9&#233;":                            "éé",
	} {
		if got := plainText(in); got != want {
			t.Errorf("plainText(%q) = %q, want %q", in, got, want)
		}
	}
	long := plainText(strings.Repeat("é", 1000))
	if len(long) > maxTipBytes || len(long)%2 != 0 || strings.ContainsRune(long, '\uFFFD') {
		t.Errorf("long text: %d bytes", len(long))
	}
	if got := plainText(strings.Repeat("<b>", 100000) + "x"); got != "x" {
		t.Errorf("many tags = %q", got)
	}
}

// controlLine waits for a control line with the given type and returns its JSON.
func controlLine(t *testing.T, out *lines, typ string, within time.Duration) string {
	t.Helper()
	timeout := time.After(within)
	for {
		select {
		case s, ok := <-out.ch:
			if !ok {
				t.Fatalf("output ended while waiting for a %s line", typ)
			}
			if j, found := strings.CutPrefix(s, controlPrefix); found && strings.HasSuffix(j, "\x07") &&
				strings.Contains(j, `"type":"`+typ+`"`) {
				return strings.TrimSuffix(j, "\x07")
			}
		case <-timeout:
			t.Fatalf("no %s control line within %v", typ, within)
		}
	}
}

// noControlLine fails when a control line arrives within d.
func noControlLine(t *testing.T, out *lines, d time.Duration) {
	t.Helper()
	timeout := time.After(d)
	for {
		select {
		case s, ok := <-out.ch:
			if ok && strings.HasPrefix(s, controlPrefix) {
				t.Fatalf("unexpected control line %q", s)
			}
		case <-timeout:
			return
		}
	}
}

func TestTooltipAfterHover(t *testing.T) {
	steam, _, in, out := setup(t)
	steam.setTip(t, "Steam", "<b>3</b> friends online<br>&amp; 1 game")
	// Wait for the refresh the NewToolTip signal triggers.
	deadline := time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		in.send(t, "hover 0")
		select {
		case s := <-out.ch:
			if strings.HasPrefix(s, controlPrefix) {
				got = s
			}
		case <-time.After(900 * time.Millisecond):
		}
		if got != "" {
			break
		}
		in.send(t, "leave")
	}
	want := controlPrefix + `{"type":"tooltip","col":0,"width":1,"title":"Steam","body":"3 friends online\n& 1 game"}` + "\x07"
	if got != want {
		t.Fatalf("tooltip line = %q\nwant %q", got, want)
	}
	// The pointer leaves: the tooltip closes.
	in.send(t, "leave")
	if j := controlLine(t, out, "close", time.Second); j != `{"type":"close"}` {
		t.Fatalf("close line = %q", j)
	}
}

func TestTooltipNotSentWhenLeftEarlyOrOnClick(t *testing.T) {
	steam, obs, in, out := setup(t)
	steam.run(t, func() { steam.tip = &[2]string{"Steam", "x"} })
	obs.run(t, func() { obs.title = "Obsidian" })
	steam.setTip(t, "Steam", "x") // refresh both through the signal path
	obs.setTip(t, "", "")
	time.Sleep(200 * time.Millisecond)

	in.send(t, "hover 0")
	time.Sleep(150 * time.Millisecond)
	in.send(t, "leave")
	noControlLine(t, out, 700*time.Millisecond)

	// A click before the delay cancels the tooltip too, without a close line.
	in.send(t, "hover 0")
	time.Sleep(100 * time.Millisecond)
	in.send(t, "click left 0 1")
	expectCall(t, steam, "Activate(0,0)")
	noControlLine(t, out, 700*time.Millisecond)

	// Moving between the gap and another icon restarts the delay.
	in.send(t, "hover 1", "hover 2")
	start := time.Now()
	j := controlLine(t, out, "tooltip", 2*time.Second)
	if d := time.Since(start); d < 400*time.Millisecond {
		t.Fatalf("the tooltip came after %v, want about 500ms", d)
	}
	// An item with no tooltip shows its Title.
	if !strings.Contains(j, `"title":"Obsidian"`) || !strings.Contains(j, `"col":2`) {
		t.Fatalf("tooltip = %q", j)
	}
	// A click after it was shown closes it.
	in.send(t, "click left 2 2")
	controlLine(t, out, "close", time.Second)
}

func TestTooltipWithNothingToSayIsNotSent(t *testing.T) {
	_, _, in, out := setup(t) // fake items without ToolTip or Title
	in.send(t, "hover 0")
	noControlLine(t, out, 900*time.Millisecond)
	in.send(t, "leave")
	noControlLine(t, out, 100*time.Millisecond)
}
