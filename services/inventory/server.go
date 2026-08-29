// Package inventory is the INVENTORY service: what is AVAILABLE.
//
// The only writer of seat status in the system, which since the split is
// enforced by topology rather than discipline — nothing else has credentials for
// the inventory schema.
package inventory

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"

	pb "github.com/slash3b/tickets/gen/tickets/v1"
	"github.com/slash3b/tickets/pkg/events"
	"github.com/slash3b/tickets/services/inventory/store"
)

type Server struct {
	pb.UnimplementedInventoryServiceServer
	store *store.Store
	pub   *events.Publisher
	lg    *zap.Logger
}

// NewServer takes a nil publisher happily: without a broker the system still
// works, it just goes back to browsers polling for changes.
func NewServer(s *store.Store, pub *events.Publisher, lg *zap.Logger) *Server {
	return &Server{store: s, pub: pub, lg: lg}
}

func (s *Server) Hold(ctx context.Context, req *pb.HoldRequest) (*pb.HoldResponse, error) {
	eventID, err := parseUUID(req.GetEventId())
	if err != nil {
		return nil, err
	}
	seats, err := parseUUIDs(req.GetSeatIds())
	if err != nil {
		return nil, err
	}
	ttl := req.GetTtl().AsDuration()
	if len(seats) == 0 || ttl <= 0 {
		return nil, status.Error(codes.InvalidArgument, "event_id, seat_ids and ttl are required")
	}

	id, err := s.store.Hold(ctx, eventID, seats, ttl)
	switch {
	case errors.Is(err, store.ErrSeatsUnavailable):
		// ABORTED, and this code is the contract. It is the documented status for
		// a concurrency conflict, it is a NORMAL outcome on a contended seat, and
		// the gateway turns it into 409. Returning Internal here would turn every
		// lost race into a server fault and make the error rate meaningless.
		return nil, status.Error(codes.Aborted, "one or more seats are not available")
	case err != nil:
		return nil, status.Error(codes.Internal, "could not hold seats")
	}
	// PUBLISHED AFTER THE COMMIT, NEVER BEFORE. The database is the truth; Kafka
	// carries news of it. Publishing first would announce a hold that might not
	// exist, and no consumer could tell the difference.
	s.publish(ctx, events.TopicSeatHeld, events.SeatChange{
		EventID: req.GetEventId(), SeatIDs: req.GetSeatIds(),
		HoldID: id.String(), Status: "held",
	})
	return &pb.HoldResponse{HoldId: id.String()}, nil
}

// publish is fire-and-forget. A seat claim must never wait on a broker: the
// transaction has committed and the customer is owed an answer.
func (s *Server) publish(ctx context.Context, topic string, c events.SeatChange) {
	if s.pub == nil {
		return
	}
	s.pub.Publish(ctx, topic, c)
}

func (s *Server) Release(ctx context.Context, req *pb.ReleaseRequest) (*pb.ReleaseResponse, error) {
	id, err := parseUUID(req.GetHoldId())
	if err != nil {
		return nil, err
	}
	reason := req.GetReason()
	if reason == "" {
		reason = "released"
	}
	eventID, seatIDs, err := s.store.SeatIDsForHold(ctx, id)
	if err != nil {
		// Not fatal: the release itself matters more than announcing it.
		s.lg.Warn("could not read seats for hold before release", zap.Error(err))
	}
	if err := s.store.Release(ctx, id, reason); err != nil {
		return nil, status.Error(codes.Internal, "could not release hold")
	}
	s.publish(ctx, events.TopicSeatReleased, events.SeatChange{
		EventID: eventID.String(), SeatIDs: uuidsToStrings(seatIDs),
		HoldID: id.String(), Status: "available", Reason: reason,
	})
	return &pb.ReleaseResponse{}, nil
}

func (s *Server) Convert(ctx context.Context, req *pb.ConvertRequest) (*pb.ConvertResponse, error) {
	id, err := parseUUID(req.GetHoldId())
	if err != nil {
		return nil, err
	}
	if err := s.store.Convert(ctx, id); err != nil {
		return nil, status.Error(codes.Internal, "could not convert hold")
	}
	return &pb.ConvertResponse{}, nil
}

func (s *Server) Commit(ctx context.Context, req *pb.CommitRequest) (*pb.CommitResponse, error) {
	id, err := parseUUID(req.GetHoldId())
	if err != nil {
		return nil, err
	}
	// Read the seats BEFORE committing: commit consumes the hold, and afterwards
	// there is no way back from a hold id to the seats it held.
	eventID, seatIDs, sErr := s.store.SeatIDsForHold(ctx, id)
	if sErr != nil {
		s.lg.Warn("could not read seats for hold before commit", zap.Error(sErr))
	}

	err = s.store.Commit(ctx, id)
	switch {
	case errors.Is(err, store.ErrHoldReleased):
		// FAILED_PRECONDITION: the seats are gone for good. The saga must not
		// retry this — retrying can never succeed, and forward recovery would spin
		// on it forever.
		return nil, status.Error(codes.FailedPrecondition, "hold was already released; seats are gone")
	case err != nil:
		return nil, status.Error(codes.Internal, "could not commit hold")
	}
	s.publish(ctx, events.TopicSeatSold, events.SeatChange{
		EventID: eventID.String(), SeatIDs: uuidsToStrings(seatIDs),
		HoldID: id.String(), Status: "sold",
	})
	return &pb.CommitResponse{}, nil
}

func (s *Server) OpenEvent(ctx context.Context, req *pb.OpenEventRequest) (*pb.OpenEventResponse, error) {
	eventID, err := parseUUID(req.GetEventId())
	if err != nil {
		return nil, err
	}
	seats, err := parseUUIDs(req.GetSeatIds())
	if err != nil {
		return nil, err
	}
	n, err := s.store.OpenEvent(ctx, eventID, seats)
	if err != nil {
		return nil, status.Error(codes.Internal, "could not open event")
	}
	return &pb.OpenEventResponse{Opened: int32(n)}, nil
}

func (s *Server) SeatStatuses(ctx context.Context, req *pb.SeatStatusesRequest) (*pb.SeatStatusesResponse, error) {
	eventID, err := parseUUID(req.GetEventId())
	if err != nil {
		return nil, err
	}
	seats, err := parseUUIDs(req.GetSeatIds())
	if err != nil {
		return nil, err
	}
	statuses, err := s.store.SeatStatuses(ctx, eventID, seats)
	if err != nil {
		return nil, status.Error(codes.Internal, "could not read seat statuses")
	}
	out := make(map[string]string, len(statuses))
	for id, st := range statuses {
		out[id.String()] = st
	}
	return &pb.SeatStatusesResponse{Statuses: out}, nil
}

// Sweep reclaims expired holds. Driven by workers on a ticker.
//
// THE INSTRUMENTATION LIVES HERE BECAUSE THE SWEEP DOES. It was written into
// store.Sweeper at milestone 9 — a loop that ran inside this process back when
// this was a package in the workers binary. The split moved the timer out to
// workers and this RPC took over the work, and the Sweeper stopped being called
// by anything. Sweeps kept happening; the span, the counter and the
// hard-deadline warning silently stopped.
//
// That is the third time in this repo that instrumentation has been written and
// then quietly orphaned, and it is the same shape each time: the code still runs,
// so nothing fails, and only the absence of telemetry says anything is wrong.
func (s *Server) Sweep(ctx context.Context, _ *pb.SweepRequest) (*pb.SweepResponse, error) {
	ctx, span := tracer.Start(ctx, "sweep")
	defer span.End()

	expired, err := s.store.SweepExpired(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, "sweep expired")
		return nil, status.Error(codes.Internal, "sweep expired failed")
	}
	span.SetAttributes(attribute.Int("holds.expired", expired))
	if expired > 0 {
		swept.Add(ctx, int64(expired), metric.WithAttributes(attribute.String("reason", "expired")))
	}

	hard, err := s.store.SweepHardDeadline(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, "sweep hard deadline")
		return nil, status.Error(codes.Internal, "sweep hard deadline failed")
	}
	if len(hard) > 0 {
		// A hold reaching its HARD deadline was stuck in `converting`, so a payment
		// outcome was never established. That is a bug signal, not routine cleanup.
		span.SetAttributes(attribute.Int("holds.hard_deadline", len(hard)))
		swept.Add(ctx, int64(len(hard)), metric.WithAttributes(attribute.String("reason", "hard_deadline")))
		s.lg.Warn("holds hit the hard deadline", zap.Int("count", len(hard)))
	}

	return &pb.SweepResponse{Expired: int32(expired), HardDeadline: int32(len(hard))}, nil
}

var (
	tracer = otel.Tracer("inventory")
	meter  = otel.Meter("inventory")

	// Holds reclaimed by the sweeper, by why. A hold released on its HARD deadline
	// rather than its TTL means something got stuck in `converting` — a bug
	// signal, not routine cleanup — and separating them is the whole value.
	swept, _ = meter.Int64Counter("tickets.holds.swept",
		metric.WithDescription("Holds reclaimed by the sweeper"),
		metric.WithUnit("{hold}"))
)

func uuidsToStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

func parseUUID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, status.Error(codes.InvalidArgument, "malformed id")
	}
	return id, nil
}

func parseUUIDs(in []string) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, len(in))
	for i, s := range in {
		id, err := parseUUID(s)
		if err != nil {
			return nil, err
		}
		out[i] = id
	}
	return out, nil
}
