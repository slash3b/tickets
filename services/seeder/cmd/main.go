// Creates one showing, once a day.
//
// Run as a CronJob. This is what makes the system permanently have something to
// do without anybody deciding to give it work.
//
// SINCE THE SPLIT IT HAS NO DATABASE. It calls catalog to create the showing and
// inventory to open its seats — the same two hops any other client would make,
// which is the point: a job with direct table access would have been the obvious
// shortcut and would have quietly made "inventory is the only writer of seat
// status" false again.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/slash3b/tickets/pkg/env"
	"github.com/slash3b/tickets/pkg/grpcx"
	"github.com/slash3b/tickets/pkg/logger"
	"github.com/slash3b/tickets/pkg/obs"
	"github.com/slash3b/tickets/services/catalog"
	"github.com/slash3b/tickets/services/inventory"

	"go.opentelemetry.io/otel"
	"go.uber.org/zap"
)

const (
	service   = "seeder"
	version   = "0.1.0"
	venueName = "Cineplex Screen 1"
)

// A rotating title list, so a week of showings is not seven identical rows.
var titles = []string{
	"Blade Runner 2069",
	"The Grand Budapest Sequel",
	"Dune: Part Nine",
	"Everything Everywhere All At Lunch",
	"No Country for Cold Servers",
	"The Kubernetes Identity",
	"Once Upon a Time in Postgres",
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		debug         = env.Get("DEBUG", "false") == "true"
		otlp          = env.Get("OTEL_EXPORTER_OTLP_ENDPOINT", "")
		catalogAddr   = env.Get("CATALOG_ADDR", "catalog.tickets.svc.cluster.local:9090")
		inventoryAddr = env.Get("INVENTORY_ADDR", "inventory.tickets.svc.cluster.local:9090")
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	shutdownObs, logProvider, err := obs.Setup(ctx, service, version, otlp)
	if err != nil {
		return fmt.Errorf("observability: %w", err)
	}
	lg, flush := logger.MustNew(service, debug, logProvider)
	defer func() { _ = flush() }()

	// A CronJob exits immediately after its work. Without an explicit flush the
	// last batch of logs and spans dies with the process, which for a job that
	// runs for two seconds is nearly all of them.
	defer func() {
		drain, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = shutdownObs(drain)
	}()

	ctx, span := otel.Tracer(service).Start(ctx, "seed")
	defer span.End()

	catConn, err := grpcx.Dial(catalogAddr)
	if err != nil {
		return err
	}
	defer func() { _ = catConn.Close() }()

	invConn, err := grpcx.Dial(inventoryAddr)
	if err != nil {
		return err
	}
	defer func() { _ = invConn.Close() }()

	cat := catalog.NewClient(catConn)
	inv := inventory.NewClient(invConn)

	// Tomorrow evening, on sale now.
	//
	// SEED_DAYS_AHEAD exists so a showing can be created on demand without waiting
	// for 03:00 or editing the CronJob. The daily run leaves it unset and gets 1.
	days := 1
	if n, err := strconv.Atoi(env.Get("SEED_DAYS_AHEAD", "")); err == nil && n > 0 {
		days = n
	}
	startsAt := time.Now().AddDate(0, 0, days).Truncate(time.Hour)

	existing, err := cat.CountEventsStartingOn(ctx, startsAt)
	if err != nil {
		return fmt.Errorf("check for that day's showing: %w", err)
	}
	if existing > 0 {
		lg.Info("a showing already exists for that day; nothing to do",
			zap.Time("starts_at", startsAt), zap.Int("existing", existing))
		return nil
	}

	venueID, sectionID, err := venue(ctx, cat, lg)
	if err != nil {
		return err
	}

	title := titles[time.Now().YearDay()%len(titles)]
	event, err := cat.CreateEvent(ctx, venueID, title, startsAt, time.Now().Add(-time.Minute))
	if err != nil {
		return fmt.Errorf("create event: %w", err)
	}
	if err := cat.SetPrice(ctx, mustID(event.GetId()), sectionID, 1200); err != nil {
		return fmt.Errorf("set price: %w", err)
	}

	// Catalog says which seats exist; INVENTORY opens them. Even here, inventory
	// stays the only writer of seat status.
	eventID, err := uuid.Parse(event.GetId())
	if err != nil {
		return fmt.Errorf("event id: %w", err)
	}

	seatIDs, err := cat.SeatIDsForEvent(ctx, eventID)
	if err != nil {
		return fmt.Errorf("list seats: %w", err)
	}
	opened, err := inv.OpenEvent(ctx, eventID, seatIDs)
	if err != nil {
		return fmt.Errorf("open event: %w", err)
	}

	lg.Info("showing created",
		zap.String("title", title),
		zap.String("event_id", event.GetId()),
		zap.Time("starts_at", startsAt),
		zap.Int("seats_opened", opened))
	return nil
}

// venue finds the cinema, creating it the first time this ever runs.
//
// ErrNoVenue has to be distinguishable from a transport failure: one means the
// catalog was never bootstrapped and we should build it, the other means the
// catalog is down and building anything would be wrong.
func venue(ctx context.Context, cat *catalog.Client, lg *zap.Logger) (venueID, sectionID uuid.UUID, err error) {
	venueID, err = cat.FindVenueByName(ctx, venueName)
	switch {
	case errors.Is(err, catalog.ErrNoVenue):
		if venueID, err = cat.CreateVenue(ctx, venueName, "cinema"); err != nil {
			return uuid.Nil, uuid.Nil, fmt.Errorf("create venue: %w", err)
		}
		// 8 rows of 12 — a realistic small screen at 96 seats. Big enough for
		// contention to be visible, small enough to sell out in a day.
		if sectionID, err = cat.AddSection(ctx, venueID, "Stalls", 8, 12); err != nil {
			return uuid.Nil, uuid.Nil, fmt.Errorf("add section: %w", err)
		}
		lg.Info("created the venue", zap.String("venue", venueName))
		return venueID, sectionID, nil
	case err != nil:
		return uuid.Nil, uuid.Nil, fmt.Errorf("find venue: %w", err)
	}

	if sectionID, err = cat.FirstSectionID(ctx, venueID); err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("first section: %w", err)
	}
	return venueID, sectionID, nil
}

// mustID parses an id the catalog just produced. A catalog that returns an
// unparseable uuid is broken in a way retrying will not fix.
func mustID(s string) uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		panic(fmt.Sprintf("catalog returned a malformed uuid %q: %v", s, err))
	}
	return id
}
