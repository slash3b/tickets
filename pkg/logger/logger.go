// Package logger builds the one logger every service uses.
//
// Logs go to TWO places at once: stdout, so `kubectl logs` still works and the
// pod is debuggable when the telemetry pipeline is down, and OTLP, so they land
// beside traces and metrics in the same store and can be correlated.
package logger

import (
	"context"

	"go.opentelemetry.io/contrib/bridges/otelzap"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// MustNew returns the service logger and its flush function. Call the flush on
// shutdown or buffered records are lost.
//
// provider may be nil — on a laptop with no collector — in which case logs go to
// stdout only and nothing else changes.
func MustNew(service string, debug bool, provider otellog.LoggerProvider) (*zap.Logger, func() error) {
	cfg := zap.NewProductionConfig()
	if debug {
		cfg = zap.NewDevelopmentConfig()
	}

	stdout, err := cfg.Build()
	if err != nil {
		panic(err)
	}

	lg := stdout
	if provider != nil {
		// THE OTLP CORE MUST BE GATED AT THE SAME LEVEL AS STDOUT.
		//
		// otelzap.Core.Enabled asks the OTel log SDK, and the SDK with no
		// minimum-severity processor says yes to everything — including Debug.
		// zapcore.Tee enables an entry if ANY core wants it, so an ungated bridge
		// quietly ships every Debug line to the backend while stdout, correctly at
		// Info, shows none of them. The two halves of one logger then disagree
		// about what was logged, which is the worst way to find out.
		//
		// It was measurable: workers put 2,133 lines into SigNoz over six hours
		// and 10 into its own stdout, because nearly everything it says per tick
		// is Debug.
		otel, err := zapcore.NewIncreaseLevelCore(
			otelzap.NewCore(service, otelzap.WithLoggerProvider(provider)),
			cfg.Level,
		)
		if err != nil {
			panic(err)
		}

		// Tee, not replace. If the collector is down, stdout still has everything —
		// losing the ability to debug a pod exactly when telemetry is broken would
		// be the worst possible trade.
		lg = zap.New(zapcore.NewTee(stdout.Core(), otel))
	}

	return lg.With(zap.String("service.name", service)), lg.Sync
}

// Ctx returns a logger that carries ctx's trace context.
//
// DESIGN.md requires a trace id on every line. This attaches it two ways, on
// purpose:
//
//   - trace_id / span_id as plain strings, so a human reading `kubectl logs` can
//     see and grep them.
//   - the context itself, which the otelzap bridge consumes to set the OTLP
//     record's real TraceId field. That is the field the backend indexes, and
//     the reason correlation is structural here rather than string-matching a
//     body. Parsing ids back out of a log body works until one service formats
//     them differently, and then fails silently.
//
// THE SkipType TRICK: the bridge picks the context out of field.Interface
// regardless of the field's Type, while zap's encoders ignore SkipType fields
// entirely. So one field feeds the OTLP core and stays invisible in stdout,
// instead of dumping a serialised context into every line.
func Ctx(ctx context.Context, lg *zap.Logger) *zap.Logger {
	fields := []zap.Field{{Key: "ctx", Type: zapcore.SkipType, Interface: ctx}}

	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		fields = append(fields,
			zap.String("trace_id", sc.TraceID().String()),
			zap.String("span_id", sc.SpanID().String()),
		)
	}

	return lg.With(fields...)
}
