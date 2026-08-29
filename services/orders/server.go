// Package orders is the ORDERS service: the saga.
//
// The only service that calls two others. Placing an order converts a hold in
// inventory, charges in payments, and commits the seats back in inventory —
// three calls that can each fail independently.
package orders

import (
	"context"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/slash3b/tickets/gen/tickets/v1"
	"github.com/slash3b/tickets/pkg/events"
	"github.com/slash3b/tickets/services/orders/store"
)

type Server struct {
	pb.UnimplementedOrdersServiceServer
	store   *store.Store
	saga    *store.Saga
	resumer *store.Resumer
	pub     *events.Publisher
	lg      *zap.Logger
}

func NewServer(s *store.Store, saga *store.Saga, res *store.Resumer, pub *events.Publisher, lg *zap.Logger) *Server {
	return &Server{store: s, saga: saga, resumer: res, pub: pub, lg: lg}
}

// publish is fire-and-forget, after the fact. An order must never wait on a
// broker to be told what happened to it.
func (s *Server) publish(ctx context.Context, topic string, c events.OrderChange) {
	if s.pub == nil {
		return
	}
	s.pub.PublishOrder(ctx, topic, c)
}

func (s *Server) Place(ctx context.Context, req *pb.PlaceRequest) (*pb.PlaceResponse, error) {
	holdID, err := uuid.Parse(req.GetHoldId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "malformed hold_id")
	}
	eventID, err := uuid.Parse(req.GetEventId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "malformed event_id")
	}
	userID, err := uuid.Parse(req.GetUserId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "malformed user_id")
	}
	if req.GetAmountMinor() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "amount_minor must be positive")
	}

	o, err := s.store.Create(ctx, holdID, eventID, userID, req.GetAmountMinor())
	if err != nil {
		return nil, status.Error(codes.Internal, "could not create order")
	}

	base := events.OrderChange{
		OrderID: o.ID.String(), EventID: eventID.String(), HoldID: holdID.String(),
		UserID: userID.String(), AmountMinor: req.GetAmountMinor(),
	}
	created := base
	created.State = "created"
	s.publish(ctx, events.TopicOrderCreated, created)

	// THE SAGA MAY LEGITIMATELY NOT FINISH, and that is not an error to the
	// caller. An unknown payment leaves the order mid-flight for the resumer, and
	// the honest answer is the state it actually reached — not a failure, which
	// would invite retrying something already in progress.
	if err := s.saga.Run(ctx, o.ID); err != nil {
		s.lg.Warn("saga did not complete in the request",
			zap.String("order_id", o.ID.String()), zap.Error(err))
	}

	state, err := s.state(ctx, o.ID)
	if err != nil {
		state = "unknown"
	}

	// Only TERMINAL states get an event. An order that is still mid-saga will be
	// finished by the resumer, and announcing "awaiting_payment" would be news
	// about nothing — the interesting message is the one that says it ended.
	switch state {
	case "confirmed":
		done := base
		done.State = state
		s.publish(ctx, events.TopicOrderConfirmed, done)
	case "failed":
		done := base
		done.State = state
		s.publish(ctx, events.TopicOrderFailed, done)
	}

	return &pb.PlaceResponse{OrderId: o.ID.String(), State: state}, nil
}

func (s *Server) GetOrder(ctx context.Context, req *pb.GetOrderRequest) (*pb.GetOrderResponse, error) {
	id, err := uuid.Parse(req.GetOrderId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "malformed order_id")
	}
	state, err := s.state(ctx, id)
	if err != nil {
		return nil, status.Error(codes.NotFound, "no such order")
	}
	return &pb.GetOrderResponse{OrderId: id.String(), State: state}, nil
}

func (s *Server) Resume(ctx context.Context, _ *pb.ResumeRequest) (*pb.ResumeResponse, error) {
	if s.resumer == nil {
		return nil, status.Error(codes.Unavailable, "resumer not configured")
	}
	n, err := s.resumer.Once(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, "resume failed")
	}
	return &pb.ResumeResponse{Resumed: int32(n)}, nil
}

func (s *Server) state(ctx context.Context, id uuid.UUID) (string, error) {
	o, err := s.store.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if o == nil {
		return "", status.Error(codes.NotFound, "no such order")
	}
	return string(o.State), nil
}
