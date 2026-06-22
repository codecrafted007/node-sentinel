// PodID is portable (no build tag) so the TTL cache and its tests build on any
// OS; the CRI-backed resolver that fills it (resolver.go) is Linux only.
package cgroup

// PodID identifies the pod/container a cgroup belongs to.
type PodID struct {
	Namespace string
	Pod       string
	Container string
	PodUID    string
	// RequestMilliCPU is the container's CPU request in millicores (0 if unknown
	// or best-effort). Used to compute a pod's fair share of CPU.
	RequestMilliCPU int64
}

func (p PodID) String() string {
	if p.Namespace == "" {
		return "unknown"
	}
	return p.Namespace + "/" + p.Pod + "/" + p.Container
}
