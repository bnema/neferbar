// Package module runs scripts that print ANSI frames on stdout.
//
// A frame ends at '\n' or '\f'. Only the most recent frame matters: a module
// that prints faster than the bar draws simply overwrites its pending frame,
// so a chatty script costs no memory and no allocation.
package module

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// MaxFrame bounds one frame; longer lines are truncated.
	MaxFrame = 16 << 10
	// MaxControl bounds one control line (see ParseControl); longer ones are
	// dropped.
	MaxControl = 64 << 10
	readSize   = 32 << 10

	minBackoff = 500 * time.Millisecond
	maxBackoff = 30 * time.Second
	// stableRun is how long a script must run before its backoff resets.
	stableRun = 10 * time.Second
)

// Zone is where a module's text is placed.
type Zone uint8

const (
	Left Zone = iota
	Center
	Right
)

// Module is one running script.
type Module struct {
	Name string
	Zone Zone
	Exec string
	// Env is added to the script's environment, as KEY=value entries.
	Env []string
	// Interactive gives the script a stdin that carries Send lines. Set it
	// before Start.
	Interactive bool

	mu      sync.Mutex
	pending []byte // latest complete frame
	dirty   bool
	failed  bool // the script is not running; show an error marker

	input   chan event    // lines for the script's stdin
	control chan []byte   // control lines of the script, see TakeControl
	gen     atomic.Uint64 // counts script starts; see Gen

	// overLogged: an oversize control line was already logged at Warn in this
	// run of the script. Touched by runOnce and readFrames, which never overlap.
	overLogged bool

	wake chan<- struct{}
	log  *slog.Logger

	done sync.WaitGroup // Start adds one; Run releases it when its process is gone
}

// maxLine is the longest line Send accepts, without its newline.
const maxLine = 127

// event is one stdin line, newline included.
type event struct {
	b [maxLine + 1]byte
	n uint8
}

// New creates a module. wake receives a non-blocking signal after each frame.
func New(name string, zone Zone, command string, wake chan<- struct{}, log *slog.Logger) *Module {
	return &Module{Name: name, Zone: zone, Exec: command, wake: wake, log: log,
		pending: make([]byte, 0, MaxFrame), input: make(chan event, 32), control: make(chan []byte, 8)}
}

// Gen counts the times the script was started. A value that changed means a
// new process, which knows nothing of the pointer state sent to the old one.
// It does not allocate.
func (m *Module) Gen() uint64 { return m.gen.Load() }

// Send queues line (without a newline) for the script's stdin. It never
// blocks and does not allocate. It reports false when the module is not
// interactive, the line is longer than 127 bytes, or 32 lines are already
// waiting (the script is not reading).
func (m *Module) Send(line []byte) bool {
	if !m.Interactive || len(line) > maxLine {
		return false
	}
	var ev event
	n := copy(ev.b[:], line)
	ev.b[n] = '\n'
	ev.n = uint8(n + 1)
	select {
	case m.input <- ev:
		return true
	default:
		return false
	}
}

// Take copies the pending frame into dst (reusing its capacity) and clears the
// dirty flag. changed is false when nothing new arrived. failed reports that
// the script is down.
func (m *Module) Take(dst []byte) (_ []byte, changed, failed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	failed = m.failed
	if !m.dirty {
		return dst, false, failed
	}
	m.dirty = false
	return append(dst[:0], m.pending...), true, failed
}

func (m *Module) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Module) publish(frame []byte) {
	m.mu.Lock()
	m.pending = append(m.pending[:0], frame...)
	m.dirty = true
	m.failed = false
	m.mu.Unlock()
	m.signal()
}

func (m *Module) setFailed() {
	m.mu.Lock()
	m.failed = true
	m.dirty = true
	m.mu.Unlock()
	m.signal()
}

// Start runs the module on its own goroutine until ctx ends. Wait returns once
// the script and the processes it started are gone.
func (m *Module) Start(ctx context.Context) {
	m.done.Add(1)
	go func() {
		defer m.done.Done()
		m.Run(ctx)
	}()
}

// Wait blocks until a module started with Start has stopped, or timeout passes.
func (m *Module) Wait(timeout time.Duration) {
	ch := make(chan struct{})
	go func() { m.done.Wait(); close(ch) }()
	select {
	case <-ch:
	case <-time.After(timeout):
	}
}

// Run starts the script and restarts it with backoff until ctx ends. It
// blocks, so call it on its own goroutine; use Start to also be able to Wait.
func (m *Module) Run(ctx context.Context) {
	backoff := minBackoff
	for ctx.Err() == nil {
		started := time.Now()
		err := m.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		m.log.Warn("module exited", "module", m.Name, "err", err)
		m.setFailed()
		if time.Since(started) > stableRun {
			backoff = minBackoff
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (m *Module) runOnce(ctx context.Context) error {
	m.gen.Add(1)
	m.overLogged = false // readFrames of the previous run has ended
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", m.Exec)
	cmd.Stderr = &logWriter{log: m.log, name: m.Name}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = append(append(os.Environ(), m.Env...), "NEFERBAR_MODULE="+m.Name)
	// Scripts call "$NEFERBAR_BIN app title" and friends: the path of the very
	// binary that started them, which need not be in $PATH.
	if exe, err := os.Executable(); err == nil {
		cmd.Env = append(cmd.Env, "NEFERBAR_BIN="+exe)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var in io.WriteCloser
	if m.Interactive {
		if in, err = cmd.StdinPipe(); err != nil {
			return err
		}
		// Events queued while no script was running are stale.
		for drained := false; !drained; {
			select {
			case <-m.input:
			default:
				drained = true
			}
		}
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	var stopWriter func()
	if in != nil {
		stopWriter = m.pumpInput(in)
	}
	m.readFrames(out)
	if stopWriter != nil {
		stopWriter()
	}
	err = cmd.Wait()
	// The script may have started children that kept running (a "while" loop
	// in a subshell, a "sleep"). They share the process group: end them too.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	return err
}

// pumpInput writes queued events to w until the returned stop function is
// called or a write fails. stop waits for the writer and closes w.
func (m *Module) pumpInput(w io.WriteCloser) (stop func()) {
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		for {
			select {
			case ev := <-m.input:
				if _, err := w.Write(ev.b[:ev.n]); err != nil {
					return
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		close(done)
		// A write blocked on a script that does not read ends when w closes.
		_ = w.Close()
		<-exited
	}
}

// controlPrefix starts a control line: an OSC 777 sequence of the "neferbar"
// application, ended by controlEnd (BEL) before the newline.
const (
	controlPrefix = "\x1b]777;neferbar;"
	controlEnd    = '\x07'
)

// TakeControl returns the oldest control line the script printed, the JSON
// between the prefix and the BEL, or false when there is none. It never blocks.
func (m *Module) TakeControl() ([]byte, bool) {
	select {
	case b := <-m.control:
		return b, true
	default:
		return nil, false
	}
}

// queueControl keeps a copy of the JSON of a control line of an interactive
// module. A full queue drops the
// line: the bar is not draining it.
func (m *Module) queueControl(json []byte) {
	if !m.Interactive {
		return // only interactive modules open popups: do not keep or parse the line
	}
	select {
	case m.control <- append([]byte(nil), json...):
		m.signal()
	default:
		m.log.Debug("module control queue full; line dropped", "module", m.Name)
	}
}

// line handles one complete line: a control line, or else a frame.
func (m *Module) line(line []byte, over bool) {
	if bytes.HasPrefix(line, []byte(controlPrefix)) {
		switch {
		case over:
			// A script can repeat this at will: say it once per run.
			if !m.overLogged {
				m.overLogged = true
				m.log.Warn("module control line too long; dropped (shown once per run)", "module", m.Name, "max", MaxControl)
			} else {
				m.log.Debug("module control line too long; dropped", "module", m.Name, "max", MaxControl)
			}
			return
		case len(line) > 0 && line[len(line)-1] == controlEnd:
			m.queueControl(line[len(controlPrefix) : len(line)-1])
			return
		}
		// No BEL: not a control line; it is shown like any text.
	}
	m.publish(line[:min(len(line), MaxFrame)])
}

// readFrames splits r into frames and publishes each one. Control lines go to
// the control queue instead.
func (m *Module) readFrames(r io.Reader) {
	buf := make([]byte, readSize)
	line := make([]byte, 0, MaxFrame)
	over := false // the current line is longer than MaxControl
	for {
		n, err := r.Read(buf)
		chunk := buf[:n]
		for len(chunk) > 0 {
			i := bytes.IndexAny(chunk, "\n\f")
			if i < 0 {
				line, over = appendCapped(line, chunk, over)
				break
			}
			line, over = appendCapped(line, chunk[:i], over)
			m.line(line, over)
			line, over = line[:0], false
			chunk = chunk[i+1:]
		}
		if err != nil {
			return
		}
	}
}

// appendCapped appends src to dst up to MaxControl bytes and reports whether
// anything was left out.
func appendCapped(dst, src []byte, over bool) ([]byte, bool) {
	if room := MaxControl - len(dst); len(src) > room {
		src, over = src[:max(room, 0)], true
	}
	return append(dst, src...), over
}

// logWriter forwards a script's stderr lines to the bar's log.
type logWriter struct {
	log  *slog.Logger
	name string
}

func (w *logWriter) Write(p []byte) (int, error) {
	text := bytes.TrimRight(p, "\n")
	if len(text) > 1024 {
		text = append(text[:1024:1024], "..."...)
	}
	w.log.Info("module stderr", "module", w.name, "text", string(text))
	return len(p), nil
}

// SetGenForTest makes the module look as if its script was restarted.
func SetGenForTest(m *Module, gen uint64) { m.gen.Store(gen) }

// DrainForTest empties the queue of Send lines and returns them without their
// newlines.
func DrainForTest(m *Module) []string {
	var out []string
	for {
		select {
		case ev := <-m.input:
			out = append(out, string(ev.b[:ev.n-1]))
		default:
			return out
		}
	}
}

// ReadFramesForTest feeds r to the module as its script's stdout.
func (m *Module) ReadFramesForTest(r io.Reader) { m.readFrames(r) }

// PublishForTest injects a frame as if the script had printed it.
func PublishForTest(m *Module, frame []byte) { m.publish(frame) }

// ScriptDir is the directory of the program a module's command starts, or ""
// when it has none to watch: a bare name such as "date" is found through $PATH,
// and a command that cannot be split into words is left alone. A leading "~/"
// is the home directory, as the shell reads it, and a relative path is taken
// from the current directory.
func ScriptDir(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 || !strings.Contains(fields[0], "/") {
		return ""
	}
	path := fields[0]
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		path = filepath.Join(home, rest)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	return filepath.Dir(abs)
}
