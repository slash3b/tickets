// Package store owns venues, sections, seats, events and prices.
//
// This is the easy service on purpose. It is read-heavy, almost never written,
// and everything it serves is cacheable — so it is where the caching lessons live
// without correctness being at stake. Nothing here decides whether a seat is
// available; that is inventory's job and inventory's alone.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Venue struct {
	ID   uuid.UUID
	Name string
	Kind string
}

type Section struct {
	ID    uuid.UUID
	Name  string
	Seats int
}

type Event struct {
	ID        uuid.UUID
	VenueID   uuid.UUID
	Title     string
	VenueName string
	StartsAt  time.Time
	OnSaleAt  time.Time
}

type Seat struct {
	ID        uuid.UUID
	RowLabel  string
	Number    int
	X, Y      float64
}

type Store struct{ db *pgxpool.Pool }

func New(db *pgxpool.Pool) *Store { return &Store{db: db} }

func (s *Store) CreateVenue(ctx context.Context, name, kind string) (*Venue, error) {
	v := &Venue{ID: uuid.New(), Name: name, Kind: kind}
	_, err := s.db.Exec(ctx,
		`INSERT INTO catalog.venues (id,name,kind) VALUES ($1,$2,$3)`, v.ID, name, kind)
	return v, err
}

// AddSection creates a section and generates rows x seatsPerRow seats in it.
func (s *Store) AddSection(ctx context.Context, venueID uuid.UUID, name string, rows, seatsPerRow int) (uuid.UUID, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	sectionID := uuid.New()
	if _, err := tx.Exec(ctx,
		`INSERT INTO catalog.sections (id,venue_id,name) VALUES ($1,$2,$3)`,
		sectionID, venueID, name); err != nil {
		return uuid.Nil, fmt.Errorf("create section: %w", err)
	}

	// One statement rather than rows*seats round trips — an arena section is
	// thousands of seats and this is the difference between instant and a minute.
	if _, err := tx.Exec(ctx, `
		INSERT INTO catalog.seats (id, section_id, row_label, seat_number, x, y)
		SELECT gen_random_uuid(), $1, chr(64 + r), c, c * 30.0, r * 30.0
		  FROM generate_series(1,$2) AS r, generate_series(1,$3) AS c`,
		sectionID, rows, seatsPerRow); err != nil {
		return uuid.Nil, fmt.Errorf("generate seats: %w", err)
	}

	return sectionID, tx.Commit(ctx)
}

func (s *Store) CreateEvent(ctx context.Context, venueID uuid.UUID, title string, startsAt, onSaleAt time.Time) (*Event, error) {
	e := &Event{ID: uuid.New(), VenueID: venueID, Title: title, StartsAt: startsAt, OnSaleAt: onSaleAt}
	_, err := s.db.Exec(ctx,
		`INSERT INTO catalog.events (id,venue_id,title,starts_at,on_sale_at)
		 VALUES ($1,$2,$3,$4,$5)`, e.ID, venueID, title, startsAt, onSaleAt)
	return e, err
}

func (s *Store) SetPrice(ctx context.Context, eventID, sectionID uuid.UUID, priceMinor int64) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO catalog.event_prices (event_id,section_id,price_minor) VALUES ($1,$2,$3)
		 ON CONFLICT (event_id,section_id) DO UPDATE SET price_minor = excluded.price_minor`,
		eventID, sectionID, priceMinor)
	return err
}

// ListOnSale returns events currently purchasable, soonest first.
func (s *Store) ListOnSale(ctx context.Context, limit int) ([]*Event, error) {
	rows, err := s.db.Query(ctx,
		`SELECT e.id, e.venue_id, e.title, v.name, e.starts_at, e.on_sale_at
		   FROM catalog.events e
		   JOIN catalog.venues v ON v.id = e.venue_id
		  WHERE e.on_sale_at <= now() AND e.starts_at > now()
		  ORDER BY e.starts_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.VenueID, &e.Title, &e.VenueName, &e.StartsAt, &e.OnSaleAt); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// FindVenueByName returns a venue id, or uuid.Nil if there is none.
func (s *Store) FindVenueByName(ctx context.Context, name string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.db.QueryRow(ctx, `SELECT id FROM catalog.venues WHERE name = $1 LIMIT 1`, name).Scan(&id)
	if err != nil {
		return uuid.Nil, nil
	}
	return id, nil
}

// FirstSectionID returns a venue's first section, or uuid.Nil.
func (s *Store) FirstSectionID(ctx context.Context, venueID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.db.QueryRow(ctx,
		`SELECT id FROM catalog.sections WHERE venue_id = $1 ORDER BY display_order, name LIMIT 1`,
		venueID).Scan(&id)
	if err != nil {
		return uuid.Nil, nil
	}
	return id, nil
}

// CountEventsStartingOn reports how many events begin on a given day.
//
// This is how the daily seeder stays idempotent: a CronJob that runs twice, or a
// pod that is retried, must not produce two showings.
func (s *Store) CountEventsStartingOn(ctx context.Context, day time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(ctx,
		`SELECT count(*) FROM catalog.events WHERE starts_at::date = $1::date`, day).Scan(&n)
	return n, err
}

func (s *Store) GetEvent(ctx context.Context, id uuid.UUID) (*Event, error) {
	var e Event
	err := s.db.QueryRow(ctx,
		`SELECT e.id, e.venue_id, e.title, v.name, e.starts_at, e.on_sale_at
		   FROM catalog.events e JOIN catalog.venues v ON v.id = e.venue_id
		  WHERE e.id = $1`, id).
		Scan(&e.ID, &e.VenueID, &e.Title, &e.VenueName, &e.StartsAt, &e.OnSaleAt)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// Sections lists an event's sections with seat counts — enough to render a
// chooser without fetching a single seat.
func (s *Store) Sections(ctx context.Context, eventID uuid.UUID) ([]*Section, error) {
	rows, err := s.db.Query(ctx,
		`SELECT sec.id, sec.name, count(st.id)
		   FROM catalog.events e
		   JOIN catalog.sections sec ON sec.venue_id = e.venue_id
		   LEFT JOIN catalog.seats st ON st.section_id = sec.id
		  WHERE e.id = $1
		  GROUP BY sec.id, sec.name, sec.display_order
		  ORDER BY sec.display_order, sec.name`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Section
	for rows.Next() {
		var sec Section
		if err := rows.Scan(&sec.ID, &sec.Name, &sec.Seats); err != nil {
			return nil, err
		}
		out = append(out, &sec)
	}
	return out, rows.Err()
}

// SectionSeats returns the seats in one section. There is deliberately NO
// whole-event equivalent: at 20,000 seats that endpoint is a denial of service
// against your own database, and not having it means nobody can call it.
func (s *Store) SectionSeats(ctx context.Context, sectionID uuid.UUID) ([]*Seat, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, row_label, seat_number, x, y FROM catalog.seats
		  WHERE section_id = $1 ORDER BY row_label, seat_number`, sectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Seat
	for rows.Next() {
		var st Seat
		if err := rows.Scan(&st.ID, &st.RowLabel, &st.Number, &st.X, &st.Y); err != nil {
			return nil, err
		}
		out = append(out, &st)
	}
	return out, rows.Err()
}

// SeatIDsForEvent returns every seat id in the event's venue.
//
// This is how inventory learns what to open for sale. Catalog does NOT write
// inventory.event_seats — inventory is the only writer of seat status anywhere in
// the system, and that stays true even for the initial load.
func (s *Store) SeatIDsForEvent(ctx context.Context, eventID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.db.Query(ctx,
		`SELECT st.id FROM catalog.events e
		   JOIN catalog.sections sec ON sec.venue_id = e.venue_id
		   JOIN catalog.seats st ON st.section_id = sec.id
		  WHERE e.id = $1`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
