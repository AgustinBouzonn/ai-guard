package runner

import (
	"context"
	"os/exec"
	"testing"
)

func TestParseCgroupMountsFindsUnifiedAndV1Memory(t *testing.T) {
	const hybrid = `36 23 0:28 / /sys/fs/cgroup rw,relatime - tmpfs tmpfs rw,mode=755
40 36 0:32 / /sys/fs/cgroup/memory rw,relatime - cgroup cgroup rw,memory
44 36 0:36 / /sys/fs/cgroup/pids rw,relatime - cgroup cgroup rw,pids
46 36 0:38 / /sys/fs/cgroup/unified rw,relatime - cgroup2 cgroup2 rw
`
	v2, v1 := parseCgroupMounts([]byte(hybrid))
	if v2 == nil || v2.point != "/sys/fs/cgroup/unified" {
		t.Errorf("cgroup2 mount = %+v, want /sys/fs/cgroup/unified", v2)
	}
	if v1 == nil || v1.point != "/sys/fs/cgroup/memory" {
		t.Errorf("v1 memory mount = %+v, want /sys/fs/cgroup/memory", v1)
	}

	const unified = `35 24 0:30 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime shared:9 - cgroup2 cgroup2 rw,nsdelegate,memory_recursiveprot
`
	v2, v1 = parseCgroupMounts([]byte(unified))
	if v2 == nil || v2.point != "/sys/fs/cgroup" || v1 != nil {
		t.Errorf("unified host: v2 = %+v, v1 = %+v; want /sys/fs/cgroup and no v1", v2, v1)
	}
}

func TestOwnCgroupPathsReadsBothHierarchies(t *testing.T) {
	const hybrid = "9:name=systemd:/\n4:memory:/process_api/abc/shell\n2:cpu,cpuacct:/x\n0::/\n"
	v2, v1 := ownCgroupPaths([]byte(hybrid))
	if v2 != "/" || v1 != "/process_api/abc/shell" {
		t.Errorf("hybrid: v2 = %q, v1 = %q; want / and /process_api/abc/shell", v2, v1)
	}
	const desktop = "0::/user.slice/user-1000.slice/user@1000.service/app.slice/vte-spawn-1.scope\n"
	v2, v1 = ownCgroupPaths([]byte(desktop))
	if v2 != "/user.slice/user-1000.slice/user@1000.service/app.slice/vte-spawn-1.scope" || v1 != "" {
		t.Errorf("desktop: v2 = %q, v1 = %q", v2, v1)
	}
}

func TestCgroupMountDirHonoursBindMountedSubtree(t *testing.T) {
	// Docker without a cgroup namespace mounts only the container's subtree.
	m := &cgMount{root: "/docker/abc", point: "/sys/fs/cgroup/memory"}
	if got := m.dirFor("/docker/abc/job"); got != "/sys/fs/cgroup/memory/job" {
		t.Errorf("dirFor = %q, want /sys/fs/cgroup/memory/job", got)
	}
	whole := &cgMount{root: "/", point: "/sys/fs/cgroup"}
	if got := whole.dirFor("/user.slice/x.scope"); got != "/sys/fs/cgroup/user.slice/x.scope" {
		t.Errorf("dirFor = %q, want /sys/fs/cgroup/user.slice/x.scope", got)
	}
}

func TestRunRlimitASMakesOversizedAllocationFail(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	alloc := []string{py, "-c", "b = bytearray(600 << 20); print('allocated')"}
	free := Run(context.Background(), Spec{Argv: alloc})
	if free.ExitCode != 0 {
		t.Fatalf("control run without a limit failed (exit %d): %s", free.ExitCode, free.Stderr)
	}
	capped := Run(context.Background(), Spec{Argv: alloc, RlimitASMB: 256})
	if capped.Err != nil {
		t.Fatalf("Err = %v", capped.Err)
	}
	if capped.ExitCode == 0 {
		t.Errorf("a 600 MiB allocation succeeded under RLIMIT_AS=256 MiB (stdout %q)", capped.Stdout)
	}
}

func TestRunGatedCommandKeepsArgumentsAndExitCode(t *testing.T) {
	// RlimitASMB routes the command through the gate shim; argv must survive it untouched.
	s := Spec{Argv: []string{"sh", "-c", `printf '%s|' "$@"; exit 7`, "x", "a b", "c'd", `e"f`, "$HOME"}, RlimitASMB: 4096}
	res := Run(context.Background(), s)
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
	if got := string(res.Stdout); got != `a b|c'd|e"f|$HOME|` {
		t.Errorf("Stdout = %q, want %q", got, `a b|c'd|e"f|$HOME|`)
	}
}
