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
	seatIDs, err := s.store.SeatIDsForHold(ctx, id)
	if err != nil {
		// Not fatal: the release itself matters more than announcing it.
		s.lg.Warn("could not read seats for hold before release", zap.Error(err))
	}
	if err := s.store.Release(ctx, id, reason); err != nil {
		return nil, status.Error(codes.Internal, "could not release hold")
	}
	s.publish(ctx, events.TopicSeatReleased, events.SeatChange{
		SeatIDs: uuidsToStrings(seatIDs), HoldID: id.String(),
		Status: "available", Reason: reason,
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
	seatIDs, sErr := s.store.SeatIDsForHold(ctx, id)
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
		SeatIDs: uuidsToStrings(seatIDs), HoldID: id.String(), Status: "sold",
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

func (s *Server) Sweep(ctx context.Context, _ *pb.SweepRequest) (*pb.SweepResponse, error) {
	expired, err := s.store.SweepExpired(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, "sweep expired failed")
	}
	hard, err := s.store.SweepHardDeadline(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, "sweep hard deadline failed")
	}
	return &pb.SweepResponse{Expired: int32(expired), HardDeadline: int32(len(hard))}, nil
}

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
