package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
)

// ServeLocal serves the latest snapshot as JSON at GET /snapshot over a unix
// socket, for the on-node sentinelctl CLI. Blocks until ctx is cancelled.
func ServeLocal(ctx context.Context, socketPath string, store *Store) error {
	if dir := filepath.Dir(socketPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	_ = os.Remove(socketPath) // clear a stale socket from a previous run

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/snapshot", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(store.Get())
	})

	srv := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
		_ = os.Remove(socketPath)
	}()
	return srv.Serve(ln)
}
