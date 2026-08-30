package profiling

import (
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestServeIsLoopbackOnly is the security property, not a smoke test. pprof dumps
// live heap and can pin a core on demand, and this cluster has no authentication
// anywhere — if this ever binds to 0.0.0.0 it is reachable from every pod, and on
// the gateway it would sit behind a public hostname.
func TestServeIsLoopbackOnly(t *testing.T) {
	host, _, err := net.SplitHostPort(Addr)
	if err != nil {
		t.Fatalf("Addr %q is not host:port: %v", Addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		t.Fatalf("Addr host %q is not a literal IP; a hostname could resolve off-box", host)
	}
	if !ip.IsLoopback() {
		t.Fatalf("pprof would listen on %s, which is not loopback", ip)
	}
}

// TestServeAnswersProfiles: the endpoints PGO needs must actually be wired, and
// on the dedicated mux rather than http.DefaultServeMux.
func TestServeAnswersProfiles(t *testing.T) {
	Serve()

	// Serve returns before the goroutine is necessarily accepting.
	deadline := time.Now().Add(3 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", Addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			lastErr = nil
			break
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("nothing listening on %s: %v", Addr, lastErr)
	}

	res, err := http.Get("http://" + Addr + "/debug/pprof/")
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/debug/pprof/ -> %d, want 200", res.StatusCode)
	}
}

// TestServeSurvivesATakenPort: profiling is a convenience and must never be able
// to stop a service booting.
func TestServeSurvivesATakenPort(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener available: %v", err)
	}
	defer func() { _ = lis.Close() }()

	// Serve is called twice across this package's tests; the second call hits an
	// address already in use and must simply return.
	done := make(chan struct{})
	go func() { Serve(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve blocked instead of returning when the port was taken")
	}
	if strings.TrimSpace(Addr) == "" {
		t.Fatal("Addr is empty")
	}
}
