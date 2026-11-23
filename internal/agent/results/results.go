// Package results keeps the final result of a detached task on disk until
// the server acknowledges it (agent-protocol v1.21 item 5). Each file is
// <dir>/<task_id>.json, mode 0600, and holds exactly the task_result frame.
package results

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

const (
	// MaxFiles and MaxAge are the contract caps; the oldest go first.
	MaxFiles = 100
	MaxAge   = 7 * 24 * time.Hour
	// DirName is the directory under the agent's state dir.
	DirName = "results"
	// maxFileBytes bounds a stored result on read; the frame cap.
	maxFileBytes = protocol.MaxFrame
	suffix       = ".json"
)

// taskIDRE is what may become a file name. A task id outside it is not
// stored (the caller keeps it in memory instead).
var taskIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// ErrTaskID is a task id that cannot be a file name.
var ErrTaskID = errors.New("results: task id")

// Store is the results directory.
type Store struct {
	mu  sync.Mutex
	dir string
	now func() time.Time
}

// Open creates stateDir/results mode 0700 and prunes it. now is the clock
// for the age limit; nil is time.Now.
func Open(stateDir string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	dir := filepath.Join(stateDir, DirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() {
		return nil, errors.New("results: not a directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, now: now}
	s.mu.Lock()
	s.prune()
	s.mu.Unlock()
	return s, nil
}

// ValidTaskID reports whether id can be stored.
func ValidTaskID(id string) bool { return taskIDRE.MatchString(id) }

// Put writes res (without its late flag) to <task_id>.json through a temp
// file, fsync and rename, then prunes to the caps. It is called before the
// result is sent.
func (s *Store) Put(res protocol.TaskResult) error {
	if !ValidTaskID(res.TaskID) {
		return ErrTaskID
	}
	res.Late = false
	b, err := protocol.Marshal(res)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, s.path(res.TaskID)); err != nil {
		return err
	}
	ok = true
	syncDir(s.dir)
	s.prune()
	return nil
}

// Get returns the stored result for id.
func (s *Store) Get(id string) (protocol.TaskResult, bool) {
	if !ValidTaskID(id) {
		return protocol.TaskResult{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read(s.path(id))
}

// List returns the stored results, oldest first, after pruning.
func (s *Store) List() []protocol.TaskResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	var out []protocol.TaskResult
	for _, f := range s.files() {
		if res, ok := s.read(f.path); ok {
			out = append(out, res)
		}
	}
	return out
}

// Delete removes the stored result for id (task_result_ack). A missing
// file is not an error.
func (s *Store) Delete(id string) error {
	if !ValidTaskID(id) {
		return ErrTaskID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.path(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) path(id string) string { return filepath.Join(s.dir, id+suffix) }

// read decodes one file. A file that is not a task_result for its own
// name, or is too large, is removed.
func (s *Store) read(path string) (protocol.TaskResult, bool) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxFileBytes {
		if err == nil {
			_ = os.Remove(path)
		}
		return protocol.TaskResult{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return protocol.TaskResult{}, false
	}
	var res protocol.TaskResult
	id := strings.TrimSuffix(filepath.Base(path), suffix)
	if json.Unmarshal(b, &res) != nil || res.Type != "task_result" || res.TaskID != id {
		_ = os.Remove(path)
		return protocol.TaskResult{}, false
	}
	return res, true
}

type stored struct {
	path string
	mod  time.Time
}

// files is the stored results, oldest first (modification time, then name).
func (s *Store) files() []stored {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var out []stored
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, suffix) || !ValidTaskID(strings.TrimSuffix(name, suffix)) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, stored{path: filepath.Join(s.dir, name), mod: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].mod.Equal(out[j].mod) {
			return out[i].mod.Before(out[j].mod)
		}
		return out[i].path < out[j].path
	})
	return out
}

// prune removes files older than MaxAge, then the oldest past MaxFiles,
// and any temp file left by a crash. Callers hold mu.
func (s *Store) prune() {
	if ents, err := os.ReadDir(s.dir); err == nil {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".tmp-") {
				_ = os.Remove(filepath.Join(s.dir, e.Name()))
			}
		}
	}
	cutoff := s.now().Add(-MaxAge)
	var keep []stored
	for _, f := range s.files() {
		if f.mod.Before(cutoff) {
			_ = os.Remove(f.path)
			continue
		}
		keep = append(keep, f)
	}
	for len(keep) > MaxFiles {
		_ = os.Remove(keep[0].path)
		keep = keep[1:]
	}
}

func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
