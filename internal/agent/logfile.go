package agent

import (
	"os"
	"sync"
)

// RotatingFile is an append-only log file that rotates to <path>.1 when it
// exceeds MaxSize. Rotation is checked on every write, so long-running
// processes (the daemon) never grow the log unbounded.
type RotatingFile struct {
	Path    string
	MaxSize int64

	mu   sync.Mutex
	f    *os.File
	size int64
}

func OpenRotating(path string, maxSize int64) (*RotatingFile, error) {
	r := &RotatingFile{Path: path, MaxSize: maxSize}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *RotatingFile) open() error {
	f, err := os.OpenFile(r.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	// Another process (the CLI) may have rotated the file: re-stat cheaply via our size.
	if r.MaxSize > 0 && r.size+int64(len(p)) > r.MaxSize {
		r.f.Close()
		os.Rename(r.Path, r.Path+".1")
		if err := r.open(); err != nil {
			r.f = nil
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}
