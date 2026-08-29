// Package store is the ORDERS service: it owns orders and the saga log.
//
// Not a deployed process — it is compiled into gateway (which places orders) and
// into workers (whose resumer finishes the ones whose request died). See
// services/README.md for which directories are binaries and which are not.
//
// It drives an order through created -> awaiting payment -> paid -> confirmed,
// writing the saga log BEFORE each attempt so a crash leaves evidence of what was
// in flight. Recovery is FORWARD, never backward: an order that was paid but
// whose confirmation was lost gets finished, not refunded.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type State string

const (
	StateCreated         State = "created"
	StateAwaitingPayment State = "awaiting_payment"
	StatePaid            State = "paid"
	StateConfirmed       State = "confirmed"
	StateFailed          State = "failed"
	StateReconciling     State = "reconciling"
	StateRefunded        State = "refunded"
)

type Order struct {
	ID            uuid.UUID
	HoldID        uuid.UUID
	EventID       uuid.UUID
	UserID        uuid.UUID
	AmountMinor   int64
	State         State
	FailureReason string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type Store struct{ db *pgxpool.Pool }

func New(db *pgxpool.Pool) *Store { return &Store{db: db} }

func (s *Store) Create(ctx context.Context, holdID, eventID, userID uuid.UUID, amountMinor int64) (*Order, error) {
	o := &Order{
		ID: uuid.New(), HoldID: holdID, EventID: eventID, UserID: userID,
		AmountMinor: amountMinor, State: StateCreated,
	}
	err := s.db.QueryRow(ctx,
		`INSERT INTO orders.orders (id, hold_id, event_id, user_id, amount_minor, state)
		 VALUES ($1,$2,$3,$4,$5,'created')
		 ON CONFLICT (hold_id) DO UPDATE SET updated_at = orders.orders.updated_at
		 RETURNING id, state`, o.ID, holdID, eventID, userID, amountMinor).
		Scan(&o.ID, &o.State)
	if err != nil {
		return nil, fmt.Errorf("create order: %w", err)
	}
	return o, nil
}

func (s *Store) Get(ctx context.Context, id uuid.UUID) (*Order, error) {
	var o Order
	err := s.db.QueryRow(ctx,
		`SELECT id, hold_id, event_id, user_id, amount_minor, state,
		        coalesce(failure_reason,''), created_at, updated_at
		   FROM orders.orders WHERE id = $1`, id).
		Scan(&o.ID, &o.HoldID, &o.EventID, &o.UserID, &o.AmountMinor, &o.State,
			&o.FailureReason, &o.CreatedAt, &o.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &o, err
}

// Advance moves an order to a new state and logs the transition atomically.
//
// State and log move together or not at all — a log that disagrees with the row
// is worse than no log, because it is trusted.
func (s *Store) Advance(ctx context.Context, id uuid.UUID, to State, step, detail string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`UPDATE orders.orders
		    SET state = $2, failure_reason = nullif($3,''), updated_at = now()
		  WHERE id = $1`, id, to, detail); err != nil {
		return fmt.Errorf("advance order: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO orders.saga_log (order_id, step, outcome, detail)
		 VALUES ($1,$2,'ok',nullif($3,''))`, id, step, detail); err != nil {
		return fmt.Errorf("log step: %w", err)
	}
	return tx.Commit(ctx)
}

// LogAttempt records that a step is ABOUT to be attempted.
//
// Written before the action, never after. If the process dies mid-step, this row
// is the only evidence the step may have had an effect — and "may have" is enough
// to change what recovery must do.
func (s *Store) LogAttempt(ctx context.Context, id uuid.UUID, step string) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO orders.saga_log (order_id, step, outcome) VALUES ($1,$2,'attempting')`,
		id, step)
	return err
}

// InFlight returns orders stuck in a non-terminal state for at least minAge.
func (s *Store) InFlight(ctx context.Context, minAge time.Duration, limit int) ([]*Order, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, hold_id, event_id, user_id, amount_minor, state,
		        coalesce(failure_reason,''), created_at, updated_at
		   FROM orders.orders
		  WHERE state IN ('created','awaiting_payment','paid','reconciling')
		    AND updated_at < now() - $1::interval
		  ORDER BY updated_at LIMIT $2`, minAge.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Order
	for rows.Next() {
		var o Order
		if err := rows.Scan(&o.ID, &o.HoldID, &o.EventID, &o.UserID, &o.AmountMinor,
			&o.State, &o.FailureReason, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &o)
	}
	return out, rows.Err()
}

// Steps returns the saga log for an order, oldest first.
func (s *Store) Steps(ctx context.Context, id uuid.UUID) ([]string, error) {
	rows, err := s.db.Query(ctx,
		`SELECT step || ':' || outcome FROM orders.saga_log WHERE order_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
