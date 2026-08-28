// Package grpcx is the shared gRPC wiring: one way to build a server, one way to
// dial a peer, and the interceptors that keep traces and logs joined across the
// hop.
//
// WHY gRPC AND NOT THE JSON-OVER-HTTP THIS BRIEFLY WAS. The services exchange
// typed domain objects with a fixed shape, and the .proto files are now the only
// definition of those shapes — one artefact both sides are generated from, rather
// than two hand-written structs that agree until somebody renames a field. It
// also gives status codes as a first-class part of the contract, which is what
// carries "the seats were taken" across the wire without inventing a convention.
package grpcx

import (
	"context"
	"fmt"
	"net"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/slash3b/tickets/pkg/logger"
)

// NewServer builds the server every service uses.
func NewServer(lg *zap.Logger) *grpc.Server {
	return grpc.NewServer(
		// otelgrpc's StatsHandler, not its deprecated interceptors: it produces
		// the server span AND extracts the incoming trace context, which is what
		// keeps one purchase a single trace across four processes.
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(accessLog(lg)),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			// Long-lived connections between services are the norm here. This only
			// reaps ones that have gone quiet in a way TCP has not noticed.
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
	)
}

// Dial connects to a peer service.
//
// grpc.NewClient does NOT block: the connection comes up lazily and reconnects on
// its own. That is what we want at start-up — a service must not refuse to boot
// because a peer is briefly down, or a cluster restart becomes ordering-dependent
// and deadlocks.
func Dial(target string) (*grpc.ClientConn, error) {
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", target, err)
	}
	return conn, nil
}

// Listen binds the gRPC port.
func Listen(port string) (net.Listener, error) {
	l, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", port, err)
	}
	return l, nil
}

// accessLog logs one line per call, correlated to the trace.
//
// LEVEL IS BY WHO IS AT FAULT, exactly as on the HTTP side. Aborted means another
// caller won the race for those seats — the most common non-OK in this system and
// a sign it is working. Logging that at error level would bury the faults that
// actually need someone.
func accessLog(lg *zap.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {

		start := time.Now()
		resp, err := handler(ctx, req)
		code := status.Code(err)

		log := logger.Ctx(ctx, lg).Info
		if isFault(code) {
			log = logger.Ctx(ctx, lg).Error
		}
		log("rpc",
			zap.String("method", info.FullMethod),
			zap.String("code", code.String()),
			zap.Duration("took", time.Since(start)),
		)
		return resp, err
	}
}

// isFault separates "we are broken" from "the answer was no".
func isFault(c codes.Code) bool {
	switch c {
	case codes.OK,
		codes.Aborted,            // someone else took the seats
		codes.FailedPrecondition, // the hold was already gone
		codes.NotFound,
		codes.InvalidArgument,
		codes.AlreadyExists,
		codes.Canceled:
		return false
	default:
		return true
	}
}
