// Package syncobj wraps the few DRM syncobj ioctls the bar needs: one acquire
// timeline signaled from the CPU, and one release timeline per buffer whose
// points the compositor signals and an eventfd reports.
package syncobj

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ioctl numbers of DRM_IOCTL_SYNCOBJ_* (Linux drm.h, type 'd').
const (
	ioctlCreate      = uintptr(0xc00864bf)
	ioctlDestroy     = uintptr(0xc00864c0)
	ioctlHandleToFD  = uintptr(0xc01864c1)
	ioctlQuery       = uintptr(0xc01864cb)
	ioctlTimelineSig = uintptr(0xc01864cd)
	ioctlEventFD     = uintptr(0xc01864cf)
)

type createArg struct{ Handle, Flags uint32 }
type destroyArg struct{ Handle, Pad uint32 }
type handleArg struct {
	Handle, Flags uint32
	FD            int32
	Pad           uint32
	Point         uint64
}
type arrayArg struct { // drm_syncobj_timeline_array
	Handles, Points uint64
	Count, Flags    uint32
}
type eventFDArg struct {
	Handle, Flags uint32
	Point         uint64
	FD            int32
	Pad           uint32
}

// Node owns a DRM render node fd. It is used from one goroutine; its scratch
// fields live in the heap object so the addresses embedded in ioctl arguments
// stay valid and no call allocates.
type Node struct {
	fd int

	handle uint32
	point  uint64
	array  arrayArg
}

// Open opens a render node such as /dev/dri/renderD128.
func Open(path string) (*Node, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("syncobj: open %s: %w", path, err)
	}
	return &Node{fd: fd}, nil
}

// Close closes the render node.
func (n *Node) Close() error {
	if n.fd < 0 {
		return nil
	}
	err := unix.Close(n.fd)
	n.fd = -1
	return err
}

func (n *Node) ioctl(request uintptr, arg unsafe.Pointer) error {
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(n.fd), request, uintptr(arg)); errno != 0 {
		return fmt.Errorf("syncobj: ioctl %#x: %w", request, errno)
	}
	return nil
}

// Create returns a new syncobj handle.
func (n *Node) Create() (uint32, error) {
	var a createArg
	if err := n.ioctl(ioctlCreate, unsafe.Pointer(&a)); err != nil {
		return 0, err
	}
	return a.Handle, nil
}

// Destroy releases a handle.
func (n *Node) Destroy(handle uint32) error {
	a := destroyArg{Handle: handle}
	return n.ioctl(ioctlDestroy, unsafe.Pointer(&a))
}

// Export returns a new owned syncobj fd for handle; the caller closes it.
func (n *Node) Export(handle uint32) (int, error) {
	a := handleArg{Handle: handle, FD: -1}
	if err := n.ioctl(ioctlHandleToFD, unsafe.Pointer(&a)); err != nil {
		return -1, err
	}
	return int(a.FD), nil
}

// Signal signals one timeline point from the CPU.
func (n *Node) Signal(handle uint32, point uint64) error {
	n.handle, n.point = handle, point
	n.array = arrayArg{
		Handles: uint64(uintptr(unsafe.Pointer(&n.handle))),
		Points:  uint64(uintptr(unsafe.Pointer(&n.point))),
		Count:   1,
	}
	return n.ioctl(ioctlTimelineSig, unsafe.Pointer(&n.array))
}

// Query returns the last signaled point of a timeline.
func (n *Node) Query(handle uint32) (uint64, error) {
	n.handle, n.point = handle, 0
	n.array = arrayArg{
		Handles: uint64(uintptr(unsafe.Pointer(&n.handle))),
		Points:  uint64(uintptr(unsafe.Pointer(&n.point))),
		Count:   1,
	}
	if err := n.ioctl(ioctlQuery, unsafe.Pointer(&n.array)); err != nil {
		return 0, err
	}
	return n.point, nil
}

// EventFD arms efd to become readable once point is signaled. The registration
// is one-shot and returns at once; the point need not be submitted yet.
func (n *Node) EventFD(handle uint32, point uint64, efd int) error {
	a := eventFDArg{Handle: handle, Point: point, FD: int32(efd)}
	return n.ioctl(ioctlEventFD, unsafe.Pointer(&a))
}
