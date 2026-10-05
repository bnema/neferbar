// Package fswatch reports changes to one file with inotify.
//
// It watches the parent directory, so a file that is replaced by a rename (the
// way editors and NeferWL write) is noticed, and it follows symlinks, so a
// dotfile-managed file is noticed at its target.
package fswatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// retryEvery is how often a missing directory is looked for again.
	retryEvery = 2 * time.Second
	// pollEvery bounds how long a wait lasts, so ctx is noticed.
	pollEvery = 200 // ms
	// mask is every way a file can be written, replaced or removed. IN_MODIFY
	// restarts the debounce on each write, so a truncating in-place save is
	// never read half written.
	mask = unix.IN_CLOSE_WRITE | unix.IN_MODIFY | unix.IN_MOVED_TO | unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_FROM
)

// files lists path and every symlink target it goes through.
func files(path string) []string {
	out := []string{filepath.Clean(path)}
	for len(out) <= 40 { // the kernel's symlink limit
		cur := out[len(out)-1]
		target, err := os.Readlink(cur)
		if err != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(cur), target)
		}
		target = filepath.Clean(target)
		if slices.Contains(out, target) {
			break
		}
		out = append(out, target)
	}
	return out
}

// Watch sends on out after path changed and stayed quiet for debounce. A
// debounce of 0 reports each change at once. The send never blocks: if out is
// full, the pending signal already says "something changed", which is all a
// reader needs. Use a channel with a buffer of 1. Watch blocks until ctx ends.
func Watch(ctx context.Context, path string, debounce time.Duration, out chan<- struct{}) error {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	names := map[int32]map[string]bool{} // watch -> file names that matter in its directory
	var watched []string
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
			watched = files(path)
			for _, file := range watched {
				wd, err := unix.InotifyAddWatch(fd, filepath.Dir(file), mask)
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

		timeout := pollEvery
		if !retry.IsZero() {
			timeout = min(timeout, int(time.Until(retry).Milliseconds())+1)
		}
		if !pending.IsZero() {
			remaining := time.Until(pending)
			if remaining <= 0 {
				pending = time.Time{}
				select {
				case out <- struct{}{}:
				default:
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
					if names[ev.Wd][string(name)] && ev.Mask&mask != 0 {
						pending = time.Now().Add(debounce)
						if !slices.Equal(watched, files(path)) {
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
