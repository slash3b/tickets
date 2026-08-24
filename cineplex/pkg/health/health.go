// Package health provides HTTP endpoints for Kubernetes liveness (/livez) and readiness (/readyz) probes.
//
// The /livez endpoint reports whether the application is running and should only fail in cases where a container restart is required.
// The /readyz endpoint reports whether the application is ready to serve traffic, executing optional health checks for dependencies
// such as databases or other persistent services.
//
// Expose these endpoints on a typical health-check port (e.g., 8080). These handlers are intended for internal use,
// enabling Kubernetes and other systems to determine pod state and manage rollout and recovery as needed.
package health

import (
	"context"
	"net/http"
	"time"

	"go.uber.org/zap"
)

const (
	livePath  = "/livez"
	readyPath = "/readyz"
)

var ready = false

// Livez determines if the application is running without deadlocks or critical failures.
// Failure triggers container restart in Kubernetes,
// should only fail if the application needs to be restarted.
func Livez(mux *http.ServeMux, lg *zap.Logger) {
	mux.HandleFunc(livePath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// Readyz determines if the application is ready to serve requests.
// Failure removes the pod from load balancer rotation,
// can fail temporarily during startup, configuration updates, or dependency issues.
func Readyz(ctx context.Context, mux *http.ServeMux, lg *zap.Logger, timeout time.Duration, checks ...func(ctx context.Context) error) {
	go readyCheck(ctx, timeout, checks...)

	mux.HandleFunc(readyPath, func(w http.ResponseWriter, r *http.Request) {
		if ready {
			w.WriteHeader(http.StatusOK)

			return
		}

		lg.Info("readyz is not doing well")

		w.WriteHeader(http.StatusInternalServerError)
	})
}

func readyCheck(ctx context.Context, timeout time.Duration, checks ...func(ctx context.Context) error) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			for _, check := range checks {
				func() {
					ctx, cancel := context.WithTimeout(ctx, timeout)
					defer cancel()

					err := check(ctx)
					if err != nil {
						ready = false

						return
					}
				}()
			}

			ready = true

			time.Sleep(15 * time.Second)
		}
	}
}
