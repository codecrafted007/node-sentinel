//go:build linux

package ebpf

import (
	"fmt"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
)

// NetObserver loads net_monitor, attaches its TCP hooks, and exposes the
// per-cgroup network counters.
type NetObserver struct {
	objs  netObjects
	links []link.Link
}

// LoadNet loads the embedded BPF objects and attaches the TCP retransmit and
// sendmsg hooks.
func LoadNet() (*NetObserver, error) {
	o := &NetObserver{}
	if err := loadNetObjects(&o.objs, nil); err != nil {
		return nil, fmt.Errorf("load net objects: %w", err)
	}

	for name, prog := range map[string]*ebpf.Program{
		"tcp_retransmit": o.objs.HandleTcpRetransmit,
		"tcp_sendmsg":    o.objs.HandleTcpSendmsg,
	} {
		l, err := link.AttachTracing(link.TracingOptions{Program: prog})
		if err != nil {
			o.Close()
			return nil, fmt.Errorf("attach %s: %w", name, err)
		}
		o.links = append(o.links, l)
	}

	return o, nil
}

// Read snapshots the per-cgroup network counters: sum the per-CPU copies, then
// delete each key so the next interval starts clean.
func (o *NetObserver) Read() ([]CgroupNet, error) {
	m := o.objs.NetStatsMap

	var (
		key     uint64
		percpu  []netNetStats
		results []CgroupNet
		keys    []uint64
	)

	it := m.Iterate()
	for it.Next(&key, &percpu) {
		st := CgroupNet{CgroupID: key}
		for _, cpu := range percpu {
			st.Retransmits += cpu.Retransmits
			st.TxBytes += cpu.TxBytes
			st.TxSegs += cpu.TxSegs
		}
		results = append(results, st)
		keys = append(keys, key)
	}
	if err := it.Err(); err != nil {
		return nil, fmt.Errorf("iterate net_stats_map: %w", err)
	}

	for i := range keys {
		_ = m.Delete(&keys[i])
	}
	return results, nil
}

// Close detaches all programs and frees BPF resources.
func (o *NetObserver) Close() {
	for _, l := range o.links {
		_ = l.Close()
	}
	o.objs.Close()
}
