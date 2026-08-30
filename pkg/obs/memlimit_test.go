package obs

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCgroupLimit covers the readings that matter, including the two that must be
// REJECTED. Obeying an "unlimited" sentinel would hand Go a limit of eight
// exabytes, which is harmless, but obeying a misparsed small number would make
// the GC thrash forever on a process that was perfectly healthy.
func TestCgroupLimit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		files   map[string]string
		want    int64
		wantOK  bool
		wantSrc string
	}{
		{
			name:    "cgroup v2 with a real limit",
			files:   map[string]string{"memory.max": "402653184\n"}, // 384Mi
			want:    402653184,
			wantOK:  true,
			wantSrc: "cgroup v2 memory.max",
		},
		{
			name:   "cgroup v2 unlimited says max",
			files:  map[string]string{"memory.max": "max\n"},
			wantOK: false,
		},
		{
			name:    "falls back to cgroup v1",
			files:   map[string]string{"memory/memory.limit_in_bytes": "201326592\n"}, // 192Mi
			want:    201326592,
			wantOK:  true,
			wantSrc: "cgroup v1 memory.limit_in_bytes",
		},
		{
			// cgroup v1 has no "max" sentinel — unlimited is this absurd number.
			// Treating it as a limit is the classic bug.
			name:   "cgroup v1 unlimited is an absurd number",
			files:  map[string]string{"memory/memory.limit_in_bytes": "9223372036854771712\n"},
			wantOK: false,
		},
		{
			name:   "nothing to read",
			files:  map[string]string{},
			wantOK: false,
		},
		{
			name:   "garbage is not a limit",
			files:  map[string]string{"memory.max": "not-a-number\n"},
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for name, body := range tc.files {
				p := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			got, src, ok := cgroupLimit(root)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got %d from %q)", ok, tc.wantOK, got, src)
			}
			if !tc.wantOK {
				return
			}
			if got != tc.want {
				t.Errorf("limit = %d, want %d", got, tc.want)
			}
			if src != tc.wantSrc {
				t.Errorf("source = %q, want %q", src, tc.wantSrc)
			}
		})
	}
}

// TestHeadroomLeavesRoom: Go must not be handed the whole cgroup limit. It only
// accounts for its own heap and stacks, while the cgroup also charges the binary,
// cgo allocations and mmap'd files — give it everything and it uses everything,
// leaving nothing for the parts it does not count.
func TestHeadroomLeavesRoom(t *testing.T) {
	var limit int64 = 402653184 // 384Mi
	target := int64(float64(limit) * headroom)

	if target >= limit {
		t.Fatalf("target %d is not below the cgroup limit %d", target, limit)
	}
	if spare := limit - target; spare < 16<<20 {
		t.Errorf("only %d bytes of headroom under a 384Mi limit; too tight for "+
			"the binary and anything allocated outside the Go heap", spare)
	}
	if target < floor {
		t.Errorf("target %d is below the sanity floor", target)
	}
}
