package grpcx

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

// recorder stands in for otelgrpc so the test can see what it was told.
type recorder struct{ lastEndErr error }

func (r *recorder) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context { return ctx }
func (r *recorder) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}
func (r *recorder) HandleConn(context.Context, stats.ConnStats) {}
func (r *recorder) HandleRPC(_ context.Context, rs stats.RPCStats) {
	if end, ok := rs.(*stats.End); ok {
		r.lastEndErr = end.Error
	}
}

// TestBusinessOutcomesAreNotErrors is the guard on a thing that would quietly
// ruin the error rate.
//
// During an on-sale roughly 90% of holds lose the race and return Aborted.
// otelgrpc marks every non-OK code as an error span, so unwrapped, a healthy
// on-sale reads as a 90% failure rate — and an error rate that is always red
// tells you nothing on the day something is genuinely broken.
func TestBusinessOutcomesAreNotErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      codes.Code
		reachOtel bool // should otelgrpc be told there was an error?
	}{
		{"lost the race", codes.Aborted, false},
		{"hold already gone", codes.FailedPrecondition, false},
		{"no such order", codes.NotFound, false},
		{"malformed request", codes.InvalidArgument, false},
		{"caller went away", codes.Canceled, false},
		{"our fault", codes.Internal, true},
		{"peer unreachable", codes.Unavailable, true},
		{"timed out", codes.DeadlineExceeded, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &recorder{}
			businessAware{r}.HandleRPC(context.Background(),
				&stats.End{Error: status.Error(tc.code, "x")})

			got := r.lastEndErr != nil
			if got != tc.reachOtel {
				if tc.reachOtel {
					t.Errorf("%s was hidden from otelgrpc; a real fault must count as one", tc.code)
				} else {
					t.Errorf("%s reached otelgrpc as an error; it is an outcome, not a fault", tc.code)
				}
			}
		})
	}
}

// A successful call must pass through untouched.
func TestSuccessIsUnchanged(t *testing.T) {
	r := &recorder{}
	businessAware{r}.HandleRPC(context.Background(), &stats.End{})
	if r.lastEndErr != nil {
		t.Errorf("a successful RPC arrived with an error attached: %v", r.lastEndErr)
	}
}
