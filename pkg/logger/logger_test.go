package logger

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/embedded"
)

// recorder is the smallest LoggerProvider that can answer "what got emitted".
type recorder struct {
	embedded.LoggerProvider
	lg *recLogger
}

func (r *recorder) Logger(string, ...log.LoggerOption) log.Logger { return r.lg }

type recLogger struct {
	embedded.Logger
	got []log.Severity
}

// Enabled says yes to everything, exactly like the real SDK with no
// minimum-severity processor configured. That is the condition the gate exists
// for: if MustNew ever stops gating, this logger will happily take Debug.
func (l *recLogger) Enabled(context.Context, log.EnabledParameters) bool { return true }

func (l *recLogger) Emit(_ context.Context, r log.Record) {
	l.got = append(l.got, r.Severity())
}

// TestOTLPCoreHonoursLevel: the bridge must not ship what stdout suppresses.
//
// zapcore.Tee enables an entry if ANY core wants it, and the OTel SDK says yes to
// every severity, so an ungated bridge sends Debug to the backend while the pod's
// own stdout shows nothing. The two halves of one logger disagreeing about what
// was logged is a bug you find months later, holding a log line you cannot
// reproduce.
func TestOTLPCoreHonoursLevel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		debug bool
		want  int // records that should reach OTLP
	}{
		{"production drops debug", false, 2},
		{"debug build keeps it", true, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recLogger{}
			lg, flush := MustNew("test", tc.debug, &recorder{lg: rec})

			lg.Debug("quiet")
			lg.Info("loud")
			lg.Warn("louder")
			_ = flush()

			if len(rec.got) != tc.want {
				t.Fatalf("OTLP got %d records %v, want %d", len(rec.got), rec.got, tc.want)
			}
			for _, s := range rec.got {
				if !tc.debug && s < log.SeverityInfo {
					t.Errorf("a record below Info reached OTLP: %v", s)
				}
			}
		})
	}
}

// TestNilProviderIsStdoutOnly: a laptop with no collector must still log.
func TestNilProviderIsStdoutOnly(t *testing.T) {
	lg, flush := MustNew("test", false, nil)
	if lg == nil {
		t.Fatal("nil logger")
	}
	lg.Info("works")
	_ = flush()
}
