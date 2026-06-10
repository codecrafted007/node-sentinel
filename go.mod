module github.com/codecrafted007/node-sentinal

go 1.25

// Deps are pinned for Go 1.25 (k8s.io/cri-api >= v0.33 needs Go 1.26). Run
// `go mod tidy` after changing them. The eBPF agent builds on Linux only; the
// metrics package is portable and unit-tested anywhere.

require (
	github.com/cilium/ebpf v0.21.0
	google.golang.org/grpc v1.69.4
	k8s.io/cri-api v0.32.3
)

require (
	github.com/gogo/protobuf v1.3.2 // indirect
	golang.org/x/net v0.46.0 // indirect
	golang.org/x/sys v0.37.0 // indirect
	golang.org/x/text v0.30.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20241015192408-796eee8c2d53 // indirect
	google.golang.org/protobuf v1.35.1 // indirect
)
