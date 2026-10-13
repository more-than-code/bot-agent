// Package spool stores unacked metrics batches on disk.
package spool

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	maxAgeDefault  = 24 * time.Hour
	maxByteDefault = 50 << 20
)

// Item is one stored batch, in send order.
type Item struct {
	ID    string
	Seq   int64
	Frame json.RawMessage
	At    time.Time
}

// Spool is the $STATE_DIR/spool directory.
type Spool struct {
	mu      sync.Mutex
	dir     string
	maxAge  time.Duration
	maxByte int64
	seq     int64
}

// Open creates dir/spool mode 0700 and uses the contract caps.
func Open(stateDir string) (*Spool, error) {
	return OpenLimited(stateDir, maxAgeDefault, maxByteDefault)
}

// OpenLimited is Open with explicit caps. Tests use it; the daemon does not.
func OpenLimited(stateDir string, maxAge time.Duration, maxBytes int64) (*Spool, error) {
	if maxAge <= 0 || maxBytes <= 0 {
		return nil, errors.New("spool limits")
	}
	dir := filepath.Join(stateDir, "spool")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Spool{dir: dir, maxAge: maxAge, maxByte: maxBytes}
	items, err := s.read()
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		if it.Seq > s.seq {
			s.seq = it.Seq
		}
	}
	return s, nil
}

type record struct {
	QueuedAt time.Time       `json:"queued_at"`
	Seq      int64           `json:"seq"`
	Frame    json.RawMessage `json:"frame"`
}

// Put stores frame under batch id. The file mode is 0600.
// Oldest batches are dropped until the directory fits the cap.
func (s *Spool) Put(id string, frame []byte, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := uuid.Parse(id); err != nil {
		return errors.New("batch id")
	}
	s.seq++
	rec := record{QueuedAt: at.UTC(), Seq: s.seq, Frame: append(json.RawMessage(nil), frame...)}
	body, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if int64(len(body)) > s.maxByte {
		return errors.New("batch exceeds spool cap")
	}
	if err := s.dropExpired(time.Now()); err != nil {
		return err
	}
	if err := s.makeRoom(int64(len(body))); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.dir, id+".json"), body, 0o600)
}

// Delete removes an acked batch. A missing file is not an error.
func (s *Spool) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := uuid.Parse(id); err != nil {
		return errors.New("batch id")
	}
	err := os.Remove(filepath.Join(s.dir, id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// List returns unacked batches in seq order after dropping expired ones.
func (s *Spool) List() ([]Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.dropExpired(time.Now()); err != nil {
		return nil, err
	}
	return s.read()
}

func (s *Spool) read() ([]Item, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var items []Item
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if _, err := uuid.Parse(id); err != nil {
			_ = os.Remove(filepath.Join(s.dir, name))
			continue
		}
		body, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			return nil, err
		}
		var rec record
		if err := json.Unmarshal(body, &rec); err != nil || len(rec.Frame) == 0 {
			_ = os.Remove(filepath.Join(s.dir, name))
			continue
		}
		items = append(items, Item{ID: id, Seq: rec.Seq, Frame: rec.Frame, At: rec.QueuedAt})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Seq == items[j].Seq {
			return items[i].ID < items[j].ID
		}
		return items[i].Seq < items[j].Seq
	})
	return items, nil
}

func (s *Spool) dropExpired(now time.Time) error {
	items, err := s.read()
	if err != nil {
		return err
	}
	for _, it := range items {
		if now.Sub(it.At) > s.maxAge {
			if err := os.Remove(filepath.Join(s.dir, it.ID+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func (s *Spool) makeRoom(incoming int64) error {
	for {
		items, err := s.read()
		if err != nil {
			return err
		}
		used, err := dirBytes(s.dir)
		if err != nil {
			return err
		}
		if used+incoming <= s.maxByte || len(items) == 0 {
			return nil
		}
		oldest := items[0]
		if err := os.Remove(filepath.Join(s.dir, oldest.ID+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
}

func dirBytes(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return 0, err
		}
		n += fi.Size()
	}
	return n, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".spool-*")
	if err != nil {
		return err
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
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	cleanup = false
	return os.Chmod(path, mode)
}

// FrameBytes is a small helper for tests that want a stable body.
func FrameBytes(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return bytes.TrimSpace(b), nil
}
