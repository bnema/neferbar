package tray

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"path/filepath"
	"testing"

	"github.com/bnema/zerobus"
)

// wb builds D-Bus wire data by hand (little endian), so tests can send bodies
// that zerobus's own encoder refuses to produce: truncated or inconsistent.
type wb struct{ b []byte }

func (w *wb) pad(n int) {
	for len(w.b)%n != 0 {
		w.b = append(w.b, 0)
	}
}

func (w *wb) u32(v uint32) { w.pad(4); w.b = binary.LittleEndian.AppendUint32(w.b, v) }
func (w *wb) i32(v int32)  { w.u32(uint32(v)) }

func (w *wb) str(s string) {
	w.u32(uint32(len(s)))
	w.b = append(append(w.b, s...), 0)
}

func (w *wb) sig(s string) { w.b = append(append(w.b, byte(len(s))), append([]byte(s), 0)...) }

// lenPrefixed writes an array whose data starts aligned to align and is filled by fn.
func (w *wb) lenPrefixed(align int, fn func()) {
	w.u32(0)
	at := len(w.b) - 4
	w.pad(align)
	start := len(w.b)
	fn()
	binary.LittleEndian.PutUint32(w.b[at:], uint32(len(w.b)-start))
}

// layoutBody is the body of a GetLayout reply for root: the revision, then the tree.
func layoutBody(root *tnode) []byte {
	var w wb
	w.u32(1)
	w.node(root)
	return w.b
}

func (w *wb) node(n *tnode) {
	w.pad(8)
	w.i32(n.id)
	w.lenPrefixed(8, func() {
		for _, p := range n.props {
			w.pad(8)
			w.str(p.k)
			switch v := p.v.(type) {
			case string:
				w.sig("s")
				w.str(v)
			case bool:
				w.sig("b")
				if v {
					w.u32(1)
				} else {
					w.u32(0)
				}
			case int32:
				w.sig("i")
				w.i32(v)
			}
		}
	})
	w.lenPrefixed(1, func() {
		if n.junk {
			w.sig("s")
			w.str("not a node")
		}
		for i := range n.kids {
			w.sig(layoutSig)
			w.node(&n.kids[i])
		}
	})
}

// frame is a method return of the given serials, with signature sig and body.
func frame(serial, replySerial uint32, sig string, body []byte) []byte {
	// Header field 5 (REPLY_SERIAL, u) and 8 (SIGNATURE, g).
	var fields wb
	fields.pad(8)
	fields.b = append(fields.b, 5)
	fields.sig("u")
	fields.u32(replySerial)
	if sig != "" {
		fields.pad(8)
		fields.b = append(fields.b, 8)
		fields.sig("g")
		fields.sig(sig)
	}
	h := []byte{'l', 2, 0, 1}
	h = binary.LittleEndian.AppendUint32(h, uint32(len(body)))
	h = binary.LittleEndian.AppendUint32(h, serial)
	h = binary.LittleEndian.AppendUint32(h, uint32(len(fields.b)))
	h = append(h, fields.b...)
	for len(h)%8 != 0 {
		h = append(h, 0)
	}
	return append(h, body...)
}

// reply is what the raw bus answers to the next method call.
type reply struct {
	sig  string
	body []byte
}

// rawBus is a bus with one client that answers every method call after Hello
// with the next queued reply. It returns the address to dial.
func rawBus(t testing.TB) (addr string, next chan<- reply) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "bus")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	ch := make(chan reply, 16)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		if _, err = r.ReadString('\n'); err != nil { // \0AUTH EXTERNAL ...
			return
		}
		if _, err = io.WriteString(c, "OK 0123456789abcdef\r\n"); err != nil {
			return
		}
		if _, err = r.ReadString('\n'); err != nil { // BEGIN
			return
		}
		var hello wb
		hello.str(":1.7")
		serial := uint32(1)
		first := true
		for {
			var hdr [16]byte
			if _, err = io.ReadFull(r, hdr[:]); err != nil {
				return
			}
			fields := binary.LittleEndian.Uint32(hdr[12:])
			rest := (16+int(fields)+7)/8*8 - 16 + int(binary.LittleEndian.Uint32(hdr[4:]))
			if _, err = io.CopyN(io.Discard, r, int64(rest)); err != nil {
				return
			}
			callSerial := binary.LittleEndian.Uint32(hdr[8:])
			var out []byte
			if first {
				first = false
				out = frame(serial, callSerial, "s", hello.b)
			} else {
				rp, ok := <-ch
				if !ok {
					return
				}
				out = frame(serial, callSerial, rp.sig, rp.body)
			}
			serial++
			if _, err = c.Write(out); err != nil {
				return
			}
		}
	}()
	return "unix:path=" + sock, ch
}

// rawDial connects to a rawBus.
func rawDial(t testing.TB, addr string) *zerobus.Conn {
	t.Helper()
	c, err := zerobus.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
