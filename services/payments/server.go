// Package payments is the PAYMENTS service: whether money moved.
//
// THE THREE OUTCOMES ARE NOT TWO. succeeded and failed are the easy ones.
// unknown is first class: the bank took the request and never answered, so money
// may or may not have moved. It is emphatically NOT failed, and an unknown
// payment must not release the seats — that is exactly how a paying customer
// loses them to someone else.
package payments

import (
	"context"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/slash3b/tickets/gen/tickets/v1"
	"github.com/slash3b/tickets/pkg/events"
	"github.com/slash3b/tickets/services/payments/bankclient"
	"github.com/slash3b/tickets/services/payments/store"
)

type Server struct {
	pb.UnimplementedPaymentsServiceServer
	store      *store.Store
	bank       *bankclient.Client
	reconciler *store.Reconciler
	pub        *events.Publisher
	lg         *zap.Logger
}

func NewServer(s *store.Store, bank *bankclient.Client, rec *store.Reconciler, pub *events.Publisher, lg *zap.Logger) *Server {
	return &Server{store: s, bank: bank, reconciler: rec, pub: pub, lg: lg}
}

func (s *Server) publish(ctx context.Context, topic string, c events.OrderChange) {
	if s.pub == nil {
		return
	}
	s.pub.PublishOrder(ctx, topic, c)
}

func (s *Server) Charge(ctx context.Context, req *pb.ChargeRequest) (*pb.ChargeResponse, error) {
	orderID, err := uuid.Parse(req.GetOrderId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "malformed order_id")
	}
	if req.GetAmountMinor() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "amount_minor must be positive")
	}

	pay, err := s.store.Create(ctx, orderID, req.GetAmountMinor())
	if err != nil {
		return nil, status.Error(codes.Internal, "could not record the charge")
	}

	charge, bankErr := s.bank.AuthorizeAndReconcile(ctx, pay.IdempotencyKey, req.GetAmountMinor())
	switch {
	case bankErr == nil:
		if err := s.store.Resolve(ctx, orderID, store.StateSucceeded, charge.ID, ""); err != nil {
			return nil, status.Error(codes.Internal, "could not record success")
		}
		s.publish(ctx, events.TopicPaymentSucceeded, events.OrderChange{
			OrderID: orderID.String(), AmountMinor: req.GetAmountMinor(), State: "succeeded",
		})
		return &pb.ChargeResponse{Outcome: pb.Outcome_OUTCOME_SUCCEEDED}, nil

	case charge != nil && charge.Status == "declined":
		if err := s.store.Resolve(ctx, orderID, store.StateFailed, "", charge.DeclineCode); err != nil {
			return nil, status.Error(codes.Internal, "could not record decline")
		}
		// OK with OUTCOME_FAILED, not an error status. The call worked; the answer
		// was no. Encoding a business outcome as a transport failure is how retry
		// logic ends up retrying decisions made correctly the first time.
		s.publish(ctx, events.TopicPaymentFailed, events.OrderChange{
			OrderID: orderID.String(), AmountMinor: req.GetAmountMinor(),
			State: "failed", Reason: charge.DeclineCode,
		})
		return &pb.ChargeResponse{
			Outcome: pb.Outcome_OUTCOME_FAILED, DeclineCode: charge.DeclineCode,
		}, nil

	default:
		if err := s.store.MarkUnknown(ctx, orderID); err != nil {
			return nil, status.Error(codes.Internal, "could not record unknown outcome")
		}
		return &pb.ChargeResponse{Outcome: pb.Outcome_OUTCOME_UNKNOWN}, nil
	}
}

func (s *Server) GetCharge(ctx context.Context, req *pb.GetChargeRequest) (*pb.GetChargeResponse, error) {
	id, err := uuid.Parse(req.GetOrderId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "malformed order_id")
	}
	p, err := s.store.Get(ctx, id)
	if err != nil || p == nil {
		return nil, status.Error(codes.NotFound, "no payment for that order")
	}
	return &pb.GetChargeResponse{
		OrderId: p.OrderID.String(), State: string(p.State), AmountMinor: p.AmountMinor,
	}, nil
}

func (s *Server) Reconcile(ctx context.Context, _ *pb.ReconcileRequest) (*pb.ReconcileResponse, error) {
	if s.reconciler == nil {
		return nil, status.Error(codes.Unavailable, "reconciler not configured")
	}
	n, err := s.reconciler.Once(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, "reconcile failed")
	}
	return &pb.ReconcileResponse{Resolved: int32(n)}, nil
}
