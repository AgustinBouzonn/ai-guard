package tools

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// pathDirs creates directories holding the given files and returns them as a PATH value.
func pathDirs(t *testing.T, dirs ...[]string) string {
	t.Helper()
	var list string
	for _, files := range dirs {
		dir := t.TempDir()
		for _, f := range files {
			if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if list != "" {
			list += string(os.PathListSeparator)
		}
		list += dir
	}
	return list
}

// counting stands in for exec.LookPath and records what it was asked.
type counting struct{ calls map[string]int }

func (c *counting) resolve(name string) (string, error) {
	c.calls[name]++
	if name == "node" || name == "tool" || name == "./local" {
		return "/resolved/" + name, nil
	}
	return "", errors.New("not found")
}

func TestLookPathAnswersMissesWithoutProbingAndRemembersHits(t *testing.T) {
	// On Windows a miss costs every PATH entry times every PATHEXT extension:
	// ~280 ms per absent tool on the machine this was measured on, three times per run.
	c := &counting{calls: map[string]int{}}
	look := newLookPath("linux", pathDirs(t, []string{"git"}, []string{"node", "npm"}), "", os.ReadDir, c.resolve)
	for i := 0; i < 3; i++ {
		if _, err := look("biome"); err == nil {
			t.Fatal("biome found although no PATH directory has it")
		}
		if p, err := look("node"); err != nil || p != "/resolved/node" {
			t.Fatalf("node = %q, %v", p, err)
		}
	}
	if c.calls["biome"] != 0 {
		t.Errorf("the real lookup ran %d times for a name no PATH directory contains", c.calls["biome"])
	}
	if c.calls["node"] != 1 {
		t.Errorf("the real lookup ran %d times for node, want once", c.calls["node"])
	}
	if p, err := look("./local"); err != nil || p != "/resolved/./local" || c.calls["./local"] != 1 {
		t.Errorf("a path is not a PATH lookup: got %q, %v after %d calls", p, err, c.calls["./local"])
	}
}

func TestLookPathOnWindowsMatchesNamesLikeTheSystemDoes(t *testing.T) {
	c := &counting{calls: map[string]int{}}
	look := newLookPath("windows", pathDirs(t, []string{"Tool.EXE", "script.cmd", "readme.txt"}), ".COM;.EXE;.BAT;.CMD", os.ReadDir, c.resolve)
	if p, err := look("tool"); err != nil || p != "/resolved/tool" {
		t.Errorf("tool = %q, %v; Tool.EXE is a candidate, the real lookup decides", p, err)
	}
	look("script")
	if c.calls["script"] != 1 {
		t.Errorf("script.cmd is a candidate for \"script\": the real lookup ran %d times", c.calls["script"])
	}
	look("readme")
	look("absent")
	if c.calls["readme"] != 0 || c.calls["absent"] != 0 {
		t.Errorf("real lookups for names with no executable candidate: readme %d, absent %d", c.calls["readme"], c.calls["absent"])
	}
}

func TestLookPathTrustsTheRealLookupWhenADirectoryCannotBeListed(t *testing.T) {
	// A directory can allow running its programs without allowing to list them.
	c := &counting{calls: map[string]int{}}
	locked := t.TempDir()
	path := pathDirs(t, []string{"git"}) + string(os.PathListSeparator) + locked + string(os.PathListSeparator) + filepath.Join(t.TempDir(), "gone")
	list := func(dir string) ([]os.DirEntry, error) {
		if dir == locked {
			return nil, os.ErrPermission
		}
		return os.ReadDir(dir)
	}
	look := newLookPath("linux", path, "", list, c.resolve)
	look("biome")
	if c.calls["biome"] != 1 {
		t.Errorf("an entry that cannot be listed may hold anything: the real lookup ran %d times, want 1", c.calls["biome"])
	}

	// A PATH entry that does not exist holds nothing: it does not spoil the shortcut.
	c = &counting{calls: map[string]int{}}
	look = newLookPath("linux", pathDirs(t, []string{"git"})+string(os.PathListSeparator)+filepath.Join(t.TempDir(), "gone"), "", os.ReadDir, c.resolve)
	look("biome")
	if c.calls["biome"] != 0 {
		t.Errorf("a missing PATH entry forced %d real lookups", c.calls["biome"])
	}
}
