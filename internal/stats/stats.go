// Package stats remembers what each step cost on this machine, so that the
// scheduler reserves what a step usually takes instead of its worst case.
package stats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// keep is how many runs of a step are remembered.
const keep = 8

type entry struct {
	PeakMB []int   `json:"peak_mb"`
	Ms     []int64 `json:"ms"`
}

// Book is the per-repository record. A nil *Book is valid and stays empty.
type Book struct {
	mu    sync.Mutex
	path  string
	steps map[string]*entry
	dirty bool
}

// Load reads the book at path; a missing or unreadable file is an empty book.
func Load(path string) *Book {
	b := &Book{path: path, steps: map[string]*entry{}}
	if data, err := os.ReadFile(path); err == nil {
		var f struct {
			Steps map[string]*entry `json:"steps"`
		}
		if json.Unmarshal(data, &f) == nil {
			for id, e := range f.Steps {
				if e != nil {
					b.steps[id] = e
				}
			}
		}
	}
	return b
}

// key drops the "#chunk" suffix: chunks of one step are the same tool on a
// different slice of the files.
func key(id string) string {
	if i := strings.LastIndexByte(id, '#'); i >= 0 {
		return id[:i]
	}
	return id
}

// Record adds one finished run. A run without a memory reading is ignored.
func (b *Book) Record(id string, peakMB int, ms int64) {
	if b == nil || peakMB <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.steps[key(id)]
	if e == nil {
		e = &entry{}
		b.steps[key(id)] = e
	}
	e.PeakMB, e.Ms = append(e.PeakMB, peakMB), append(e.Ms, ms)
	if n := len(e.PeakMB) - keep; n > 0 {
		e.PeakMB = e.PeakMB[n:]
	}
	if n := len(e.Ms) - keep; n > 0 {
		e.Ms = e.Ms[n:]
	}
	b.dirty = true
}

// Typical is the median peak of the remembered runs (the upper one of an
// even count): one cold run among warm ones does not set the expectation.
func (b *Book) Typical(id string) (peakMB int, ok bool) {
	if b == nil {
		return 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.steps[key(id)]
	if e == nil || len(e.PeakMB) == 0 {
		return 0, false
	}
	sorted := append([]int{}, e.PeakMB...)
	sort.Ints(sorted)
	return sorted[len(sorted)/2], true
}

// Save writes the book if it changed, atomically: concurrent runs in one
// repository must never leave half a file behind.
func (b *Book) Save() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.dirty {
		return nil
	}
	data, err := json.Marshal(struct {
		Steps map[string]*entry `json:"steps"`
	}{b.steps})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(b.path), ".stats-*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(data, '\n'))
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), b.path)
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return werr
	}
	b.dirty = false
	return nil
}
