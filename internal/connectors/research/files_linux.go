//go:build linux

package research

import (
	"os"
	"path/filepath"
	"syscall"
)

// Scope each administrator-selected file to its containing directory. OpenRoot
// rejects symlink escapes; O_NONBLOCK prevents a replaced FIFO from hanging a
// collector before its regular-file check. The parent directory is trusted
// administrator configuration, never selected by a model or requester.
func openLocal(path string) (*os.File, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.OpenFile(filepath.Base(path), os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
