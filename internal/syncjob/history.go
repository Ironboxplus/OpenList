package syncjob

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// The history is one JSON snapshot per line, appended when a job ends, so
// jobs stay auditable across restarts without a schema or a database table.

var historyMu sync.Mutex

func appendHistory(path string, s Snapshot) error {
	s.Events = nil
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	historyMu.Lock()
	defer historyMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func readHistory(path string, limit int) ([]Snapshot, error) {
	historyMu.Lock()
	defer historyMu.Unlock()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return []Snapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var all []Snapshot
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var s Snapshot
		// A line cut short by a crash mid-write is skipped, not fatal.
		if json.Unmarshal(sc.Bytes(), &s) == nil {
			all = append(all, s)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	return all, nil
}
