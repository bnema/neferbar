package config

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"time"

	"git.bnema.dev/bnema/neferbar/internal/fswatch"
)

// debounce waits for a save to finish before the file is read.
const debounce = 100 * time.Millisecond

// Watch reloads the file at path whenever it changes and sends each valid,
// different Config to out. last is the Config the caller already runs, so a
// save that changes nothing sends nothing. A broken file or a deleted file is
// logged and the caller keeps its current configuration. It blocks until ctx
// ends.
func Watch(ctx context.Context, path string, last Config, out chan<- Config, log *slog.Logger) error {
	changed := make(chan struct{}, 1)
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- fswatch.Watch(wctx, path, debounce, changed) }()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			return err
		case <-changed:
		}
		next, err := Load(path, true)
		switch {
		case errors.Is(err, os.ErrNotExist):
			log.Warn("config file removed; keeping the current config", "path", path)
		case err != nil:
			log.Warn("config reload failed; keeping the current config", "err", err)
		case reflect.DeepEqual(next, last):
			log.Debug("config unchanged")
		default:
			last = next
			select {
			case <-ctx.Done():
				return nil
			case out <- next:
			}
		}
	}
}
