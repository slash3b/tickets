// Package profiling serves pprof on loopback, for collecting the CPU profiles
// that feed profile-guided optimisation.
//
// LOOPBACK ONLY, AND THAT IS THE WHOLE SECURITY MODEL. pprof is not a read-only
// dashboard: /debug/pprof/heap dumps live memory, and /debug/pprof/profile pins a
// core for its duration, so it is both a disclosure and a denial-of-service
// surface. This cluster has no authentication anywhere, and the gateway's HTTP
// port is routed to the public hostname — putting pprof on the shared mux would
// publish heap dumps of a running ticket shop at app.tickets.lan/debug/pprof.
//
// Binding to 127.0.0.1 inside the pod makes it unreachable from any other pod or
// from the Gateway API, while `kubectl port-forward` still works: port-forward
// attaches to the POD'S NETWORK NAMESPACE, so loopback there is exactly what it
// connects to. That gives us profiles with no new port on a Service, no auth
// story to invent, and nothing new to remember to turn off.
//
//	kubectl -n tickets port-forward deploy/gateway 6060:6060
//	go tool pprof -http=: http://127.0.0.1:6060/debug/pprof/profile?seconds=30
//
// See infra/dashboards/../PGO notes in the repo README for how the committed
// default.pgo files are refreshed.
package profiling

import (
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

// Addr is where the profiler listens. 6060 is the Go convention.
const Addr = "127.0.0.1:6060"

// Serve starts the pprof listener and returns. It never returns an error,
// because a debugging facility must not be able to stop a service from booting —
// if the port is taken, profiling is unavailable and the service is fine.
func Serve() {
	// A DEDICATED MUX, NOT http.DefaultServeMux. Importing net/http/pprof
	// registers its handlers on the default mux as a side effect of init, so any
	// server that ever falls back to it would start serving pprof wherever it
	// happens to listen. Wiring the handlers explicitly here means that global
	// registration is never the thing being served.
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	lis, err := net.Listen("tcp", Addr)
	if err != nil {
		return
	}

	srv := &http.Server{
		Handler: mux,
		// No ReadTimeout or WriteTimeout: a CPU profile is a long, deliberately
		// slow response — ?seconds=30 means thirty seconds of streaming — and a
		// write timeout would cut every profile short at the default.
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srv.Serve(lis) }()
}
