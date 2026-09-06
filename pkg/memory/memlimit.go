package memory

import (
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
)

const (
	// headroom is the fraction of the cgroup limit handed to Go.
	//
	// NOT 100%, because GOMEMLIMIT only governs what the GO RUNTIME accounts for —
	// heap, goroutine stacks, runtime metadata. The cgroup also charges the binary
	// itself, anything allocated through cgo or plain malloc, and mmap'd files. Give
	// Go the whole limit and it will happily use all of it, leaving nothing for the
	// parts it is not counting.
	headroom = 0.9

	// floor guards against a too low reading.
	floor = 32 << 20
)

// ApplyLimit teaches the GC the ceiling the container is under. Call it once, as
// early in main as possible — it costs nothing when there is no cgroup limit to
// read.
//
// It is deliberately silent about what it did. There is no accessor for the
// applied value because nothing needs to ask Go for it: when a limit exists the
// runtime instrumentation exports it as go.memory.limit, which is the copy
// anyone actually looks at.
func ApplyLimit() {
	// AN EXPLICIT GOMEMLIMIT WINS. Go reads that environment variable itself
	// before main runs, so a value here is somebody overriding on purpose and
	// must not be second-guessed. math.MaxInt64 is the runtime's "unset".
	if debug.SetMemoryLimit(-1) != math.MaxInt64 {
		return
	}

	// No reading at all is the ordinary answer on a laptop, or in a container
	// with no memory limit set. Nothing to do.
	limit, ok := cgroupLimit("/sys/fs/cgroup")
	if !ok {
		return
	}

	// Below the floor the reading is far likelier to be wrong than real, and
	// applying it would strangle a healthy process. Leave the runtime alone.
	target := int64(float64(limit) * headroom)
	if target < floor {
		return
	}

	debug.SetMemoryLimit(target)
}

// plausible rejects the "unlimited" sentinels. cgroup v1 reports something near
// 2^63 when there is no limit; anything at or above a terabyte is not a limit
// anyone set on a homelab node and is better ignored than obeyed.
func plausible(n int64) bool { return n > 0 && n < 1<<40 }

// cgroupLimit reads the memory ceiling, trying cgroup v2 then v1.
//
// root is a parameter so this is testable without a container.
//
// see: https://github.com/KimMachineGun/automemlimit
func cgroupLimit(root string) (int64, bool) {
	// cgroup v2. "max" means no limit, which is the normal answer on a laptop.
	if b, err := os.ReadFile(filepath.Join(root, "memory.max")); err == nil {
		s := strings.TrimSpace(string(b))
		if s == "max" {
			return 0, false
		}

		if n, err := strconv.ParseInt(s, 10, 64); err == nil && plausible(n) {
			return n, true
		}
	}

	// cgroup v1 has no "max" sentinel: unlimited is a number so large it is
	// obviously not a real limit, which is what plausible() rejects.
	if b, err := os.ReadFile(filepath.Join(root, "memory", "memory.limit_in_bytes")); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && plausible(n) {
			return n, true
		}
	}

	return 0, false
}
