package bench

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type journalEntry struct {
	File   string      `json:"file"`
	Backup string      `json:"backup"`
	Mode   os.FileMode `json:"mode"`
}

func benchDir(root string) string { return filepath.Join(root, ".aiguard", "bench") }

func journalPath(root string) string { return filepath.Join(benchDir(root), "journal.json") }

func loadJournal(root string) ([]journalEntry, error) {
	data, err := os.ReadFile(journalPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []journalEntry
	return entries, json.Unmarshal(data, &entries)
}

func saveJournal(root string, entries []journalEntry) error {
	if len(entries) == 0 {
		err := os.Remove(journalPath(root))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	data, _ := json.Marshal(entries)
	tmp := journalPath(root) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, journalPath(root))
}

// ensureDir creates the state directory, hidden from git by its own ignore file.
func ensureDir(root string) error {
	if err := os.MkdirAll(filepath.Join(benchDir(root), "backup"), 0o755); err != nil {
		return err
	}
	ignore := filepath.Join(root, ".aiguard", ".gitignore")
	if _, err := os.Stat(ignore); errors.Is(err, os.ErrNotExist) {
		return os.WriteFile(ignore, []byte("*\n"), 0o644)
	}
	return nil
}

// Apply edits the patch's file and returns the function that puts the
// original bytes back. The original is saved, and journalled, before the file
// is touched, so Restore can undo the edit even if this process dies.
func Apply(root string, p Patch, n int) (restore func() error, err error) {
	if err := ensureDir(root); err != nil {
		return nil, err
	}
	entries, err := loadJournal(root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.File == p.File {
			return nil, fmt.Errorf("%s is still patched from an earlier run: run `ai-guard bench restore` first", p.File)
		}
	}
	path := filepath.Join(root, filepath.FromSlash(p.File))
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	orig, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	num := strconv.Itoa(n)
	var patched string
	switch {
	case p.Find != "":
		if !strings.Contains(string(orig), p.Find) {
			return nil, fmt.Errorf("patch %s: %q not found in %s", p.Name, p.Find, p.File)
		}
		patched = strings.Replace(string(orig), p.Find, strings.ReplaceAll(p.Replace, "{n}", num), 1)
	case p.Append != "":
		patched = string(orig) + strings.ReplaceAll(p.Append, "{n}", num)
	default:
		return nil, fmt.Errorf("patch %s: needs append or find/replace", p.Name)
	}
	entry := journalEntry{File: p.File, Backup: filepath.Join("backup", strconv.Itoa(len(entries)+1)+"-"+strconv.Itoa(n)), Mode: info.Mode().Perm()}
	if err := os.WriteFile(filepath.Join(benchDir(root), entry.Backup), orig, 0o644); err != nil {
		return nil, err
	}
	if err := saveJournal(root, append(entries, entry)); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(patched), info.Mode().Perm()); err != nil {
		return nil, err
	}
	return func() error { return restoreEntry(root, entry) }, nil
}

func restoreEntry(root string, e journalEntry) error {
	backup := filepath.Join(benchDir(root), e.Backup)
	orig, err := os.ReadFile(backup)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(e.File)), orig, e.Mode); err != nil {
		return err
	}
	entries, err := loadJournal(root)
	if err != nil {
		return err
	}
	kept := entries[:0]
	for _, x := range entries {
		if x.File != e.File {
			kept = append(kept, x)
		}
	}
	if err := saveJournal(root, kept); err != nil {
		return err
	}
	return os.Remove(backup)
}

// Restore puts back every file a previous run left patched and reports how many.
func Restore(root string) (int, error) {
	entries, err := loadJournal(root)
	if err != nil {
		return 0, err
	}
	for i, e := range entries {
		if err := restoreEntry(root, e); err != nil {
			return i, err
		}
	}
	return len(entries), nil
}
