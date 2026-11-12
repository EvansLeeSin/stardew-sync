//go:build !unix

package agent

import (
	"os"
	"path/filepath"
	"sync"
)

var procLock sync.Mutex

// lockFile is an in-process lock on non-unix platforms (tests only).
func lockFile(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	procLock.Lock()
	return procLock.Unlock, nil
}
