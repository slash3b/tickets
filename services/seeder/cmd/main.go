// Creates one showing, once a day.
//
// Run as a CronJob. This is what makes the system permanently have something to
// do without anybody deciding to give it work — the standing rule for this
// project is one showing per day, quietly, and an on-sale burst is something a
// human triggers.
//
// IDEMPOTENT. A CronJob that fires twice, a retried pod, or a manual run must not
// produce two showings for the same day.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/slash3b/tickets/pkg/env"
	"github.com/slash3b/tickets/pkg/logger"
	"github.com/slash3b/tickets/pkg/migrate"
	catalogstore "github.com/slash3b/tickets/services/catalog/store"
	inventorystore "github.com/slash3b/tickets/services/inventory/store"
	ordersstore "github.com/slash3b/tickets/services/orders/store"
	paystore "github.com/slash3b/tickets/services/payments/store"

	"go.uber.org/zap"
)

const venueName = "Cineplex Screen 1"

// A rotating title list, so a week of showings is not seven identical rows.
var titles = []string{
	"Dune: Part Three", "Blade Runner 2069", "The Grand Budapest Sequel",
	"Arrival II", "Heat 2", "Children of Men Redux", "Solaris",
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := env.Get("DATABASE_URL", "")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	lg, flush := logger.MustNew("seeder", env.Get("DEBUG", "false") == "true", nil)
	defer func() { _ = flush() }()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()

	if err := migrate.Apply(ctx, pool,
		catalogstore.SchemaSQL, inventorystore.SchemaSQL,
		ordersstore.SchemaSQL, paystore.SchemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}

	cat := catalogstore.New(pool)
	inv := inventorystore.New(pool)

	// Tomorrow evening, on sale now.
	//
	// SEED_DAYS_AHEAD exists so a showing can be created on demand without waiting
	// for 03:00 or editing the CronJob. The daily run leaves it unset and gets 1.
	// It earns its place because the day's seats DO sell out — the simulator
	// drained all 96 on 2026-08-27 — and once they have, there is nothing left to
	// hold, buy, or trace until the next cron fires.
	days := 1
	if n, err := strconv.Atoi(env.Get("SEED_DAYS_AHEAD", "")); err == nil && n > 0 {
		days = n
	}
	startsAt := time.Now().AddDate(0, 0, days).Truncate(time.Hour)

	existing, err := cat.CountEventsStartingOn(ctx, startsAt)
	if err != nil {
		return fmt.Errorf("check for today's showing: %w", err)
	}
	if existing > 0 {
		lg.Info("a showing already exists for that day; nothing to do",
			zap.Time("starts_at", startsAt), zap.Int("existing", existing))
		return nil
	}

	venueID, err := cat.FindVenueByName(ctx, venueName)
	if err != nil {
		return err
	}
	sectionID := venueID
	if venueID == uuid.Nil {
		v, err := cat.CreateVenue(ctx, venueName, "cinema")
		if err != nil {
			return fmt.Errorf("create venue: %w", err)
		}
		venueID = v.ID
		// 8 rows of 12 — a realistic small screen at 96 seats. Big enough for
		// contention to be visible, small enough to sell out in a day.
		if sectionID, err = cat.AddSection(ctx, venueID, "Stalls", 8, 12); err != nil {
			return fmt.Errorf("add section: %w", err)
		}
		lg.Info("created the venue", zap.String("venue", venueName))
	} else if sectionID, err = cat.FirstSectionID(ctx, venueID); err != nil {
		return err
	}

	title := titles[time.Now().YearDay()%len(titles)]
	event, err := cat.CreateEvent(ctx, venueID, title, startsAt, time.Now().Add(-time.Minute))
	if err != nil {
		return fmt.Errorf("create event: %w", err)
	}
	if err := cat.SetPrice(ctx, event.ID, sectionID, 1200); err != nil {
		return fmt.Errorf("set price: %w", err)
	}

	// Catalog says which seats exist; INVENTORY opens them. Even here, inventory
	// stays the only writer of seat status.
	seatIDs, err := cat.SeatIDsForEvent(ctx, event.ID)
	if err != nil {
		return fmt.Errorf("list seats: %w", err)
	}
	opened, err := inv.OpenEvent(ctx, event.ID, seatIDs)
	if err != nil {
		return fmt.Errorf("open event: %w", err)
	}

	lg.Info("showing created",
		zap.String("title", title),
		zap.String("event_id", event.ID.String()),
		zap.Time("starts_at", startsAt),
		zap.Int("seats_opened", opened))
	return nil
}
