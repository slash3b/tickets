// Package memory sets GOMEMLIMIT from the container's own cgroup limit.
//
// GOMEMLIMIT is a second trigger point for the garbage collector. The GC runs at whichever trigger comes first.
// Without it, there's one rule — GOGC=100 says next collection when the heap reaches 2× live data. With it, there are two, and the lower one wins.
package memory
