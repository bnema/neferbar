package config

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	debounce = 100 * time.Millisecond
	// retryEvery is how often a missing directory is looked for again.
	retryEvery = 2 * time.Second
	// inotify mask: every way a file can be written, replaced or removed.
	// IN_MODIFY restarts the debounce on each write, so a truncating in-place
	// save is never read half written.
	watchMask = unix.IN_CLOSE_WRITE | unix.IN_MODIFY | unix.IN_MOVED_TO | unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_FROM
)

// watchedFiles lists the config path and every symlink target it goes through
// (dotfile managers), so retargeting a link is noticed.
func watchedFiles(path string) []string {
	files := []string{filepath.Clean(path)}
	for len(files) <= 40 { // the kernel's symlink limit
		cur := files[len(files)-1]
		target, err := os.Readlink(cur)
		if err != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(cur), target)
		}
		target = filepath.Clean(target)
		if slices.Contains(files, target) {
			break
		}
		files = append(files, target)
	}
	return files
}

// Watch reloads the file at path whenever it changes and sends each valid,
// different Config to out. last is the Config the caller already runs, so a
// save that changes nothing sends nothing. A broken file or a deleted file is
// logged and the caller keeps its current configuration.
//
// It watches the parent directory, so atomic replacements (editors that
// write a temp file and rename it) are detected. If the directory is missing
// it retries every two seconds. It blocks until ctx ends.
func Watch(ctx context.Context, path string, last Config, out chan<- Config, log *slog.Logger) error {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	names := map[int32]map[string]bool{} // watch -> file names that matter in its directory
	var files []string
	var retry, pending time.Time
	rewatch := true
	buf := make([]byte, 4096)
	fds := make([]unix.PollFd, 1)

	for ctx.Err() == nil {
		if !retry.IsZero() && !time.Now().Before(retry) {
			rewatch = true
		}
		if rewatch {
			rewatch, retry = false, time.Time{}
			for wd := range names {
				_, _ = unix.InotifyRmWatch(fd, uint32(wd))
			}
			clear(names)
			files = watchedFiles(path)
			for _, file := range files {
				wd, err := unix.InotifyAddWatch(fd, filepath.Dir(file), watchMask)
				if errors.Is(err, unix.ENOENT) {
					retry = time.Now().Add(retryEvery)
					continue
				}
				if err != nil {
					return err
				}
				if names[int32(wd)] == nil {
					names[int32(wd)] = map[string]bool{}
				}
				names[int32(wd)][filepath.Base(file)] = true
			}
		}

		timeout := 200 // ms: also the cadence at which ctx is checked
		if !retry.IsZero() {
			timeout = min(timeout, int(time.Until(retry).Milliseconds())+1)
		}
		if !pending.IsZero() {
			remaining := time.Until(pending)
			if remaining <= 0 {
				pending = time.Time{}
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
				continue
			}
			timeout = min(timeout, int(remaining.Milliseconds())+1)
		}

		fds[0] = unix.PollFd{Fd: int32(fd), Events: unix.POLLIN}
		if _, err = unix.Poll(fds, timeout); err != nil {
			if err == unix.EINTR {
				continue
			}
			return err
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			continue
		}
		for {
			n, err := unix.Read(fd, buf)
			if err == unix.EAGAIN || n == 0 {
				break
			}
			if err == unix.EINTR {
				continue
			}
			if err != nil {
				return err
			}
			for off := 0; off+unix.SizeofInotifyEvent <= n; {
				ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
				size := unix.SizeofInotifyEvent + int(ev.Len)
				if off+size > n {
					break
				}
				// One of our watches vanished (its directory was removed).
				if ev.Mask&unix.IN_IGNORED != 0 && names[ev.Wd] != nil {
					rewatch = true
				}
				if ev.Len > 0 {
					name := buf[off+unix.SizeofInotifyEvent : off+size]
					for len(name) > 0 && name[len(name)-1] == 0 {
						name = name[:len(name)-1]
					}
					if names[ev.Wd][string(name)] && ev.Mask&watchMask != 0 {
						pending = time.Now().Add(debounce)
						if !slices.Equal(files, watchedFiles(path)) {
							rewatch = true // a symlink now points elsewhere
						}
					}
				}
				off += size
			}
		}
	}
	return nil
}
