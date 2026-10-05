package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"git.bnema.dev/bnema/neferbar/internal/app"
)

const appUsage = `usage: neferbar app [-once] <field>...

Prints fields of the window that has the focus: one line now, then one more
each time it changes. With several fields, a tab separates them on the line
(a title never contains a tab). Fields are empty when no window has the focus.

fields:
  id     the application id, such as com.github.bnema.dumber
  name   the short form of the id, such as dumber
  title  the window title, without control characters

flags:
`

// runApp is "neferbar app <field>": the focused window, for scripts.
func runApp(ctx context.Context, args []string, display string) error {
	fs := flag.NewFlagSet("neferbar app", flag.ContinueOnError)
	once := fs.Bool("once", false, "print the current value and exit")
	fs.Usage = func() { fmt.Fprint(os.Stderr, appUsage); fs.PrintDefaults() }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return fmt.Errorf("give at least one field: id, name or title")
	}
	var picks []func(app.Window) string
	for _, field := range fs.Args() {
		switch field {
		case "id":
			picks = append(picks, func(w app.Window) string { return app.Clean(w.ID) })
		case "name":
			picks = append(picks, func(w app.Window) string { return app.Clean(app.Name(w.ID)) })
		case "title":
			picks = append(picks, func(w app.Window) string { return app.Clean(w.Title) })
		default:
			return fmt.Errorf("%q is not a field; use id, name or title", field)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	return app.Watch(ctx, display, func(w app.Window) error {
		var line []byte
		for i, pick := range picks {
			if i > 0 {
				line = append(line, '\t')
			}
			line = append(line, pick(w)...)
		}
		if _, err := os.Stdout.Write(append(line, '\n')); err != nil {
			return err
		}
		if *once {
			cancel()
		}
		return nil
	})
}
