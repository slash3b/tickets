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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/slash3b/tickets/gen/tickets/v1"
	"github.com/slash3b/tickets/pkg/events"
	"github.com/slash3b/tickets/pkg/logger"
	"github.com/slash3b/tickets/services/payments/bankclient"
	"github.com/slash3b/tickets/services/payments/store"
)

// THE COUNTER THAT MAKES `unknown` VISIBLE.
//
// Every other layer of this service is careful that unknown is not failed, and
// until now that distinction existed only in the database and the code. From
// outside, a bank that has stopped answering and a bank that is declining
// everything looked identical: orders stop confirming. They need completely
// different responses — one is "wait for the reconciler", the other is "stop
// selling" — so the rate of each has to be a number you can graph and alert on.
var (
	meter = otel.Meter("payments")

	charges, _ = meter.Int64Counter("tickets.payments",
		metric.WithDescription("Charge attempts by outcome"),
		metric.WithUnit("{charge}"))
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

	// Recorded on the span too, so a single trace answers "what did the bank say"
	// without opening the payments database.
	outcome := "unknown"
	switch {
	case bankErr == nil:
		outcome = "succeeded"
	case charge != nil && charge.Status == "declined":
		outcome = "declined"
	}
	charges.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("payment.outcome", outcome))

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
		// WARN, not error: the request succeeded and the design handles this. But
		// it is the one outcome that may have moved money without anyone knowing,
		// so it must never be silent.
		logger.Ctx(ctx, s.lg).Warn("bank did not answer, payment is unknown",
			zap.String("order_id", orderID.String()),
			zap.Int64("amount_minor", req.GetAmountMinor()),
			zap.Error(bankErr))
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
