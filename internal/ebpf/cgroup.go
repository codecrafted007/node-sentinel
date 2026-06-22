//go:build linux

package ebpf

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
)

// Cgroup lifecycle event ops — keep in sync with cgroup_monitor.bpf.c.
const (
	CgroupOpMkdir = 0
	CgroupOpRmdir = 1
)

// CgroupEvent is one cgroup lifecycle event from the kernel (issue #2): a cgroup
// directory was created or removed. Path is the cgroup path the kernel reported;
// user space parses the container ID from it.
type CgroupEvent struct {
	CgroupID uint64
	Op       uint32
	Path     string
}

// CgroupObserver loads cgroup_monitor, attaches the cgroup_mkdir/rmdir
// tracepoints, and streams lifecycle events from a ring buffer.
type CgroupObserver struct {
	objs   cgroupObjects
	links  []link.Link
	reader *ringbuf.Reader
}

// LoadCgroup loads the embedded objects, attaches the lifecycle tracepoints, and
// opens the ring buffer for reading.
func LoadCgroup() (*CgroupObserver, error) {
	o := &CgroupObserver{}
	if err := loadCgroupObjects(&o.objs, nil); err != nil {
		return nil, fmt.Errorf("load cgroup objects: %w", err)
	}

	for name, prog := range map[string]*ebpf.Program{
		"cgroup_mkdir": o.objs.HandleCgroupMkdir,
		"cgroup_rmdir": o.objs.HandleCgroupRmdir,
	} {
		l, err := link.AttachTracing(link.TracingOptions{Program: prog})
		if err != nil {
			o.Close()
			return nil, fmt.Errorf("attach %s: %w", name, err)
		}
		o.links = append(o.links, l)
	}

	rd, err := ringbuf.NewReader(o.objs.CgroupEvents)
	if err != nil {
		o.Close()
		return nil, fmt.Errorf("open cgroup_events ringbuf: %w", err)
	}
	o.reader = rd
	return o, nil
}

// Read blocks for the next lifecycle event. It returns an error once Close has
// been called (ringbuf.ErrClosed), which a reader loop should treat as "stop".
func (o *CgroupObserver) Read() (CgroupEvent, error) {
	rec, err := o.reader.Read()
	if err != nil {
		return CgroupEvent{}, err
	}
	var raw cgroupCgroupEvent
	if err := binary.Read(bytes.NewReader(rec.RawSample), binary.LittleEndian, &raw); err != nil {
		return CgroupEvent{}, fmt.Errorf("decode cgroup event: %w", err)
	}
	return CgroupEvent{
		CgroupID: raw.CgroupId,
		Op:       raw.Op,
		Path:     cString(raw.Path[:]),
	}, nil
}

// Close stops the reader (unblocking Read), detaches the programs, and frees the
// BPF objects.
func (o *CgroupObserver) Close() {
	if o.reader != nil {
		_ = o.reader.Close()
	}
	for _, l := range o.links {
		_ = l.Close()
	}
	o.objs.Close()
}

// cString turns a NUL-terminated C char array into a Go string.
func cString(b []int8) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}
