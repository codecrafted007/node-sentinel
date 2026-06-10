//go:build tools

// Package tools pins build-time tool dependencies (bpf2go) so `go mod tidy`
// records them in go.mod/go.sum. It is never compiled into any binary.
package tools

import _ "github.com/cilium/ebpf/cmd/bpf2go"
