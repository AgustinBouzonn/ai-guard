package tools

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// NewLookPath returns exec.LookPath with a memory. On Windows a miss is
// expensive: every PATH entry times every PATHEXT extension is a file-system
// probe, about 280 ms per absent tool on a developer machine, and a plan
// asks for several tools the machine may not have. So results are kept for
// the life of the process, and a name that no PATH directory contains is
// answered from a single listing of those directories.
func NewLookPath() func(string) (string, error) {
	var (
		once sync.Once
		look func(string) (string, error)
	)
	return func(name string) (string, error) {
		once.Do(func() { // PATH as it is at the first lookup, not at program start
			look = newLookPath(runtime.GOOS, os.Getenv("PATH"), os.Getenv("PATHEXT"), os.ReadDir, exec.LookPath)
		})
		return look(name)
	}
}

func newLookPath(goos, pathEnv, pathExt string, list func(string) ([]os.DirEntry, error), resolve func(string) (string, error)) func(string) (string, error) {
	type result struct {
		path string
		err  error
	}
	var (
		mu       sync.Mutex
		memo     = map[string]result{}
		names    map[string]struct{} // every file name in the PATH directories
		complete bool                // false: some directory could not be listed
	)
	windows := goos == "windows"
	exts := []string{""}
	if windows {
		if pathExt == "" {
			pathExt = ".com;.exe;.bat;.cmd"
		}
		for _, e := range strings.Split(strings.ToLower(pathExt), ";") {
			if e != "" {
				exts = append(exts, e)
			}
		}
	}
	scan := func() {
		names, complete = map[string]struct{}{}, true
		for _, dir := range filepath.SplitList(pathEnv) {
			if dir == "" {
				if windows {
					continue
				}
				dir = "." // POSIX: an empty entry is the current directory
			}
			entries, err := list(dir)
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					complete = false // it may hold anything
				}
				continue
			}
			for _, e := range entries {
				n := e.Name()
				if windows {
					n = strings.ToLower(n)
				}
				names[n] = struct{}{}
			}
		}
	}
	// absent reports whether no PATH directory has a file the name could resolve to.
	absent := func(name string) bool {
		if names == nil {
			scan()
		}
		if !complete {
			return false
		}
		if windows {
			name = strings.ToLower(name)
		}
		for _, e := range exts {
			if _, ok := names[name+e]; ok {
				return false
			}
		}
		return true
	}
	return func(name string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if r, ok := memo[name]; ok {
			return r.path, r.err
		}
		var r result
		if !strings.ContainsAny(name, `/\`) && absent(name) {
			r.err = &exec.Error{Name: name, Err: exec.ErrNotFound}
		} else {
			r.path, r.err = resolve(name)
		}
		memo[name] = r
		return r.path, r.err
	}
}
