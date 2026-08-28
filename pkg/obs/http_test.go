package obs_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/slash3b/tickets/pkg/obs"
)

// TestAccessLogIsCorrelated guards the half of the observability story that was
// missing for longer than the spans were.
//
// pkg/logger builds a careful correlation mechanism — trace_id and span_id as
// readable strings, plus the context itself for the OTLP bridge — and for weeks
// the only caller in the entire repo was the hello canary (since deleted). The gateway served
// every request in the system and logged nothing but "listening". A log pipeline
// that receives no logs looks exactly like a broken log pipeline.
func TestAccessLogIsCorrelated(t *testing.T) {
	otel.SetTracerProvider(sdktrace.NewTracerProvider())

	core, logs := observer.New(zapcore.InfoLevel)
	mux := http.NewServeMux()
	obs.Route(mux, zap.New(core), "GET /api/things/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/things/abc", nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)

	entries := logs.FilterMessage("request").All()
	if len(entries) != 1 {
		t.Fatalf("got %d request log lines, want 1", len(entries))
	}
	fields := entries[0].ContextMap()

	// The route PATTERN, never the URL. "/api/things/abc" in a log field is one
	// distinct value per id, which is how a log store stops being searchable.
	if fields["route"] != "GET /api/things/{id}" {
		t.Errorf("route = %v, want the pattern not the URL", fields["route"])
	}
	if fields["status"] != int64(http.StatusTeapot) {
		t.Errorf("status = %v, want 418 — the recorder is not seeing WriteHeader", fields["status"])
	}

	// THE ASSERTION THAT MATTERS. Without a trace id on the line, a log and the
	// trace that produced it can only be joined by squinting at timestamps.
	id, ok := fields["trace_id"].(string)
	if !ok || id == "" {
		t.Fatalf("no trace_id on the access log; correlation is broken (fields: %v)", fields)
	}
	if _, ok := fields["span_id"].(string); !ok {
		t.Errorf("no span_id on the access log")
	}
	t.Logf("correlated: route=%v status=%v trace_id=%s", fields["route"], fields["status"], id)
}

// TestAccessLogLevelsByFault: a lost race is not an error.
//
// 409 is the single most common non-200 in this system and it means the design is
// working. Logging it at error level would bury the 5xx that actually need
// someone, which is the fastest way to make people stop reading the logs.
func TestAccessLogLevelsByFault(t *testing.T) {
	otel.SetTracerProvider(sdktrace.NewTracerProvider())

	for _, tc := range []struct {
		name   string
		status int
		want   zapcore.Level
	}{
		{"ok", http.StatusOK, zapcore.InfoLevel},
		{"lost the race", http.StatusConflict, zapcore.InfoLevel},
		{"bad request", http.StatusBadRequest, zapcore.InfoLevel},
		{"our fault", http.StatusInternalServerError, zapcore.ErrorLevel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.InfoLevel)
			mux := http.NewServeMux()
			obs.Route(mux, zap.New(core), "GET /x", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			})
			mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))

			all := logs.All()
			if len(all) != 1 {
				t.Fatalf("got %d lines, want 1", len(all))
			}
			if all[0].Level != tc.want {
				t.Errorf("status %d logged at %v, want %v", tc.status, all[0].Level, tc.want)
			}
		})
	}
}
