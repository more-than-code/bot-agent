package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// atomicWrite replaces path via a temp file in the same directory.
// The file mode is set before the bytes are written. dirMode is applied
// only when this call creates the parent directory.
func atomicWrite(path string, data []byte, mode, dirMode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return errors.New("env directory cannot be created")
	}
	f, err := os.CreateTemp(dir, ".bot-agent-*")
	if err != nil {
		return errors.New("env file cannot be written")
	}
	tmp := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return errors.New("env file cannot be written")
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return errors.New("env file cannot be written")
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return errors.New("env file cannot be written")
	}
	if err := f.Close(); err != nil {
		return errors.New("env file cannot be written")
	}
	if err := os.Rename(tmp, path); err != nil {
		return errors.New("env file cannot be written")
	}
	cleanup = false
	// Best-effort directory fsync so the rename survives a crash.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("env file mode: %w", err)
	}
	return nil
}
