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

	// THE DAILY CRON STILL MAKES EXACTLY ONE MOVIE. The concert path is a
	// different job entirely, run by hand, because an arena on-sale is something
	// you stage and watch — not something that should appear at 03:00 while
	// nobody is looking.
	if env.Get("SEED_CONCERT", "") != "" {
		return concert(ctx, lg, cat)
	}

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

// concert creates an arena and a 20,000-seat show that goes on sale LATER.
//
// This is milestone 8 in one function, and the interesting part is the last
// argument to CreateEvent: on_sale_at in the FUTURE, for the first time in this
// system. Until that moment passes, the workers on-sale loop leaves the event
// alone, inventory has no rows for it, and every attempt to hold a seat fails
// on its own without anything checking a clock.
//
// Seats are NOT opened here. That is deliberate: opening them is what starts the
// sale, and it belongs to the loop that watches the clock.
func concert(ctx context.Context, lg *zap.Logger, cat *catalog.Client) error {
	const (
		arenaName = "Homelab Arena"
		sections  = 10
		rows      = 50
		perRow    = 40 // 10 * 50 * 40 = 20,000
	)

	onSaleIn, err := time.ParseDuration(env.Get("CONCERT_ON_SALE_IN", "10m"))
	if err != nil {
		return fmt.Errorf("CONCERT_ON_SALE_IN: %w", err)
	}
	title := env.Get("CONCERT_TITLE", "Lady Gaga — The Chromatica Ball")

	venueID, err := cat.FindVenueByName(ctx, arenaName)
	switch {
	case errors.Is(err, catalog.ErrNoVenue):
		if venueID, err = cat.CreateVenue(ctx, arenaName, "arena"); err != nil {
			return fmt.Errorf("create arena: %w", err)
		}
		// Ten sections of 2,000. Generated one statement per section rather than
		// one for all 20,000, so a failure halfway leaves a coherent venue and so
		// the seat chart has the shape a real arena has.
		for i := 1; i <= sections; i++ {
			name := fmt.Sprintf("Block %d", i)
			if _, err := cat.AddSection(ctx, venueID, name, rows, perRow); err != nil {
				return fmt.Errorf("add %s: %w", name, err)
			}
			lg.Info("arena section built", zap.String("section", name),
				zap.Int("seats", rows*perRow))
		}
		lg.Info("created the arena", zap.String("venue", arenaName),
			zap.Int("seats", sections*rows*perRow))
	case err != nil:
		return fmt.Errorf("find arena: %w", err)
	}

	startsAt := time.Now().AddDate(0, 0, 30).Truncate(time.Hour)
	onSaleAt := time.Now().Add(onSaleIn).Truncate(time.Second)

	event, err := cat.CreateEvent(ctx, venueID, title, startsAt, onSaleAt)
	if err != nil {
		return fmt.Errorf("create concert: %w", err)
	}

	// Price every section. An arena would really tier these by section; that is
	// an open question in DESIGN.md and it does not change what milestone 9
	// measures, so every block costs the same for now.
	secs, err := cat.Sections(ctx, mustID(event.GetId()))
	if err != nil {
		return fmt.Errorf("list sections: %w", err)
	}
	// Tiered by position — see catalog.TierFor. Every block at one price made the
	// rush spread evenly, which is the one thing a real on-sale does not do.
	for i, sec := range secs {
		tier := catalog.TierFor(i, len(secs), false)
		if err := cat.SetPrice(ctx, mustID(event.GetId()), mustID(sec.GetId()), tier.PriceMinor); err != nil {
			return fmt.Errorf("set price: %w", err)
		}
	}

	lg.Info("concert created — NOT yet on sale",
		zap.String("title", title),
		zap.String("event_id", event.GetId()),
		zap.Time("starts_at", startsAt),
		zap.Time("on_sale_at", onSaleAt),
		zap.Duration("on_sale_in", onSaleIn),
		zap.Int("sections", len(secs)))
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
