//go:build linux

package cgroup

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// debounceWindow coalesces a burst of cgroup changes (a pod starting creates
// several directories at once) into a single refresh.
const debounceWindow = 500 * time.Millisecond

// Watcher watches the cgroup tree and signals when pods come and go, so the
// resolver can refresh immediately instead of waiting for the next periodic
// rescan (design §7.4 "ongoing updates"). The periodic rescan remains the
// safety net for any events inotify drops.
type Watcher struct {
	fsw  *fsnotify.Watcher
	root string
}

// NewWatcher starts watching root and every directory beneath it.
func NewWatcher(root string) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watcher{fsw: fsw, root: root}
	w.addRecursive(root)
	return w, nil
}

// addRecursive adds a watch to dir and every subdirectory under it. Errors on
// individual directories are ignored — they race with pod deletion.
func (w *Watcher) addRecursive(dir string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = w.fsw.Add(path)
		}
		return nil
	})
}

// Run coalesces filesystem events and calls onChange once the tree has been
// quiet for the debounce window. Newly created directories are watched as they
// appear. Blocks until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context, onChange func()) {
	defer w.fsw.Close()

	var timer *time.Timer
	var fire <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			return

		case e, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			// A new pod/container directory must itself be watched so we catch
			// its children appearing.
			if e.Op&fsnotify.Create != 0 {
				if fi, err := os.Stat(e.Name); err == nil && fi.IsDir() {
					w.addRecursive(e.Name)
				}
			}
			if timer == nil {
				timer = time.NewTimer(debounceWindow)
				fire = timer.C
			} else {
				timer.Reset(debounceWindow)
			}

		case <-fire:
			timer, fire = nil, nil
			onChange()

		case <-w.fsw.Errors:
			// Ignore — the periodic rescan is the safety net.
		}
	}
}
