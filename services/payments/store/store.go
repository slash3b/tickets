// Package store is the PAYMENTS service: whether money moved.
//
// Not a deployed process — it is compiled into gateway (which charges) and into
// workers (whose reconciler establishes the truth about charges the bank never
// answered for). See services/README.md for which directories are binaries.
//
// It owns payment records and is the only thing that decides whether
// money moved, and it is written on the assumption that the bank may take money
// and then say nothing.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type State string

const (
	StatePending   State = "pending"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateUnknown   State = "unknown"
)

type Payment struct {
	ID                uuid.UUID
	OrderID           uuid.UUID
	IdempotencyKey    string
	AmountMinor       int64
	State             State
	BankChargeID      string
	DeclineCode       string
	ReconcileAttempts int
	UpdatedAt         time.Time
}

type Store struct{ db *pgxpool.Pool }

func New(db *pgxpool.Pool) *Store { return &Store{db: db} }

// IdempotencyKey derives a stable key from an order id.
//
// DETERMINISTIC ON PURPOSE. A process can crash between creating a payment and
// hearing back from the bank; on restart it must produce the SAME key, or the
// bank sees a new request and charges again. Anything random or clock-based here
// silently defeats the bank's idempotency and no amount of care elsewhere
// recovers it.
func IdempotencyKey(orderID uuid.UUID) string {
	sum := sha256.Sum256([]byte("tickets/payment/v1/" + orderID.String()))
	return "pay_" + hex.EncodeToString(sum[:16])
}

// Create records the intent to charge, before any money moves. Returns the
// existing payment if one is already recorded for this order — a duplicate
// request must never become a second charge.
func (s *Store) Create(ctx context.Context, orderID uuid.UUID, amountMinor int64) (*Payment, error) {
	p := &Payment{
		ID:             uuid.New(),
		OrderID:        orderID,
		IdempotencyKey: IdempotencyKey(orderID),
		AmountMinor:    amountMinor,
		State:          StatePending,
	}

	err := s.db.QueryRow(ctx,
		`INSERT INTO payments.payments (id, order_id, idempotency_key, amount_minor, state)
		 VALUES ($1, $2, $3, $4, 'pending')
		 ON CONFLICT (order_id) DO UPDATE SET updated_at = payments.payments.updated_at
		 RETURNING id, state, coalesce(bank_charge_id,''), coalesce(decline_code,''), reconcile_attempts`,
		p.ID, p.OrderID, p.IdempotencyKey, p.AmountMinor,
	).Scan(&p.ID, &p.State, &p.BankChargeID, &p.DeclineCode, &p.ReconcileAttempts)
	if err != nil {
		return nil, fmt.Errorf("create payment: %w", err)
	}
	return p, nil
}

// Resolve records a definite outcome.
func (s *Store) Resolve(ctx context.Context, orderID uuid.UUID, state State, chargeID, declineCode string) error {
	if state == StatePending {
		return errors.New("pending is not an outcome")
	}
	_, err := s.db.Exec(ctx,
		`UPDATE payments.payments
		    SET state = $2, bank_charge_id = nullif($3,''), decline_code = nullif($4,''),
		        updated_at = now()
		  WHERE order_id = $1`,
		orderID, state, chargeID, declineCode)
	return err
}

// Get returns the payment for an order.
func (s *Store) Get(ctx context.Context, orderID uuid.UUID) (*Payment, error) {
	var p Payment
	err := s.db.QueryRow(ctx,
		`SELECT id, order_id, idempotency_key, amount_minor, state,
		        coalesce(bank_charge_id,''), coalesce(decline_code,''),
		        reconcile_attempts, updated_at
		   FROM payments.payments WHERE order_id = $1`, orderID,
	).Scan(&p.ID, &p.OrderID, &p.IdempotencyKey, &p.AmountMinor, &p.State,
		&p.BankChargeID, &p.DeclineCode, &p.ReconcileAttempts, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &p, err
}

// Unresolved returns payments that have no definite outcome and have been left
// alone for at least minAge. The age matters: a payment created two seconds ago
// is probably still in flight, and asking the bank about it races the request
// that is already happening.
func (s *Store) Unresolved(ctx context.Context, minAge time.Duration, limit int) ([]*Payment, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, order_id, idempotency_key, amount_minor, state,
		        coalesce(bank_charge_id,''), coalesce(decline_code,''),
		        reconcile_attempts, updated_at
		   FROM payments.payments
		  WHERE state IN ('pending','unknown') AND updated_at < now() - $1::interval
		  ORDER BY updated_at
		  LIMIT $2`, minAge.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Payment
	for rows.Next() {
		var p Payment
		if err := rows.Scan(&p.ID, &p.OrderID, &p.IdempotencyKey, &p.AmountMinor, &p.State,
			&p.BankChargeID, &p.DeclineCode, &p.ReconcileAttempts, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// MarkUnknown records that the bank did not answer, and counts the attempt.
func (s *Store) MarkUnknown(ctx context.Context, orderID uuid.UUID) error {
	_, err := s.db.Exec(ctx,
		`UPDATE payments.payments
		    SET state = 'unknown',
		        reconcile_attempts = reconcile_attempts + 1,
		        updated_at = now()
		  WHERE order_id = $1 AND state IN ('pending','unknown')`, orderID)
	return err
}
