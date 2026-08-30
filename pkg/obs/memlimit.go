package obs

import (
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
)

// GOMEMLIMIT, derived from the container's own cgroup limit.
//
// THE GO GC HAS NO IDEA THE CONTAINER HAS A CEILING. GOGC=100 means "let the heap
// grow to twice the live data, then collect" — a RATIO, with no absolute number
// anywhere in it. So a service whose live heap reaches 200MiB inside a 384Mi
// container is aiming for 400MiB, and the kernel kills it before it gets there.
// That is not hypothetical here: the gateway and the simulator have both been
// OOMKilled, when limits sized for a 96-seat cinema met a 20,000-seat arena.
//
// GOMEMLIMIT gives the runtime that missing number. As total runtime memory
// approaches it the collector runs more often, spending CPU to stay under the
// limit instead of dying. IT IS A SOFT LIMIT: Go will not refuse an allocation,
// so if live data genuinely exceeds the limit the process still dies. What this
// buys is that a TRANSIENT peak becomes slow instead of fatal.
//
// READ FROM THE CGROUP RATHER THAN SET PER DEPLOYMENT, because there are nine
// services with nine different limits and a hardcoded GOMEMLIMIT=345MiB in a
// manifest goes stale the moment somebody edits the memory limit next to it —
// silently, and in the direction that kills the pod.
//
// The applied value is visible in SigNoz as go.memory.limit: the runtime
// instrumentation only reports that metric when a limit actually exists, which is
// why the Memory Limit panel of the Go Runtime dashboard was empty until now.

// headroom is the fraction of the cgroup limit handed to Go.
//
// NOT 100%, because GOMEMLIMIT only governs what the GO RUNTIME accounts for —
// heap, goroutine stacks, runtime metadata. The cgroup also charges the binary
// itself, anything allocated through cgo or plain malloc, and mmap'd files. Give
// Go the whole limit and it will happily use all of it, leaving nothing for the
// parts it is not counting.
const headroom = 0.9

// floor guards against a nonsense reading. Below this the number is far more
// likely to be a parse error or an unexpected cgroup layout than a real limit,
// and applying it would strangle the process on purpose.
const floor = 32 << 20

var (
	appliedLimit  int64
	appliedReason string
)

// MemoryLimit reports what was applied at startup and why. Zero means no limit
// was set, and the reason says which of the several ordinary causes it was.
func MemoryLimit() (int64, string) { return appliedLimit, appliedReason }

func applyMemoryLimit() {
	// AN EXPLICIT GOMEMLIMIT WINS. Go reads that environment variable itself
	// before main runs, so a value here is somebody overriding on purpose and
	// must not be second-guessed. math.MaxInt64 is the runtime's "unset".
	if cur := debug.SetMemoryLimit(-1); cur != math.MaxInt64 {
		appliedLimit, appliedReason = cur, "GOMEMLIMIT set explicitly"
		return
	}

	limit, source, ok := cgroupLimit("/sys/fs/cgroup")
	if !ok {
		appliedReason = "no cgroup memory limit (not containerised, or unlimited)"
		return
	}

	target := int64(float64(limit) * headroom)
	if target < floor {
		appliedReason = "cgroup limit too small to be plausible; left alone"
		return
	}

	debug.SetMemoryLimit(target)
	appliedLimit, appliedReason = target, source
}

// cgroupLimit reads the memory ceiling, trying cgroup v2 then v1.
//
// root is a parameter so this is testable without a container.
func cgroupLimit(root string) (int64, string, bool) {
	// cgroup v2. "max" means no limit, which is the normal answer on a laptop.
	if b, err := os.ReadFile(filepath.Join(root, "memory.max")); err == nil {
		s := strings.TrimSpace(string(b))
		if s == "max" {
			return 0, "", false
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && plausible(n) {
			return n, "cgroup v2 memory.max", true
		}
	}

	// cgroup v1 has no "max" sentinel: unlimited is a number so large it is
	// obviously not a real limit, which is what plausible() rejects.
	if b, err := os.ReadFile(filepath.Join(root, "memory", "memory.limit_in_bytes")); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && plausible(n) {
			return n, "cgroup v1 memory.limit_in_bytes", true
		}
	}

	return 0, "", false
}

// plausible rejects the "unlimited" sentinels. cgroup v1 reports something near
// 2^63 when there is no limit; anything at or above a terabyte is not a limit
// anyone set on a homelab node and is better ignored than obeyed.
func plausible(n int64) bool { return n > 0 && n < 1<<40 }
