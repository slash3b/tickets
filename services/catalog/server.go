// Package catalog is the CATALOG service: what EXISTS.
//
// Read-mostly and almost never written — the seeder adds one showing a day and
// that is the whole write load. It does NOT know what is available; that is
// inventory's, and the split is the point.
package catalog

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.uber.org/zap"

	pb "github.com/slash3b/tickets/gen/tickets/v1"
	"github.com/slash3b/tickets/pkg/logger"
	"github.com/slash3b/tickets/services/catalog/store"
)

type Server struct {
	pb.UnimplementedCatalogServiceServer
	store *store.Store
	lg    *zap.Logger
}

func NewServer(s *store.Store, lg *zap.Logger) *Server {
	if lg == nil {
		lg = zap.NewNop()
	}
	return &Server{store: s, lg: lg}
}

// internal turns a store failure into a status AND says what went wrong.
//
// The access-log line for a failed RPC carries the method and the code and
// nothing else, which is right — but it meant a foreign-key violation surfaced
// as "Internal" with the cause discarded at exactly the point it was known.
func (s *Server) internal(ctx context.Context, op string, err error) error {
	logger.Ctx(ctx, s.lg).Error("catalog failed", zap.String("op", op), zap.Error(err))
	return status.Error(codes.Internal, "could not "+op)
}

func (s *Server) ListOnSale(ctx context.Context, req *pb.ListOnSaleRequest) (*pb.ListOnSaleResponse, error) {
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.store.ListOnSale(ctx, limit)
	if err != nil {
		return nil, s.internal(ctx, "list events", err)
	}
	out := make([]*pb.Event, len(rows))
	for i, e := range rows {
		out[i] = wireEvent(e)
	}
	return &pb.ListOnSaleResponse{Events: out}, nil
}

func (s *Server) ListUpcoming(ctx context.Context, req *pb.ListUpcomingRequest) (*pb.ListUpcomingResponse, error) {
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.store.ListUpcoming(ctx, limit)
	if err != nil {
		return nil, s.internal(ctx, "list upcoming events", err)
	}
	out := make([]*pb.Event, len(rows))
	for i, e := range rows {
		out[i] = wireEvent(e)
	}
	return &pb.ListUpcomingResponse{Events: out}, nil
}

func (s *Server) ListDueForOnSale(ctx context.Context, req *pb.ListDueForOnSaleRequest) (*pb.ListDueForOnSaleResponse, error) {
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.store.ListDueForOnSale(ctx, limit)
	if err != nil {
		return nil, s.internal(ctx, "list due events", err)
	}
	out := make([]*pb.Event, len(rows))
	for i, e := range rows {
		out[i] = wireEvent(e)
	}
	return &pb.ListDueForOnSaleResponse{Events: out}, nil
}

func (s *Server) MarkSeatsOpened(ctx context.Context, req *pb.MarkSeatsOpenedRequest) (*pb.MarkSeatsOpenedResponse, error) {
	id, err := parseUUID(req.GetEventId())
	if err != nil {
		return nil, err
	}
	if err := s.store.MarkSeatsOpened(ctx, id); err != nil {
		return nil, s.internal(ctx, "mark seats opened", err)
	}
	return &pb.MarkSeatsOpenedResponse{}, nil
}

func (s *Server) GetEvent(ctx context.Context, req *pb.GetEventRequest) (*pb.GetEventResponse, error) {
	id, err := parseUUID(req.GetEventId())
	if err != nil {
		return nil, err
	}
	e, err := s.store.GetEvent(ctx, id)
	if err != nil {
		return nil, status.Error(codes.NotFound, "no such event")
	}
	return &pb.GetEventResponse{Event: wireEvent(e)}, nil
}

func (s *Server) ListSections(ctx context.Context, req *pb.ListSectionsRequest) (*pb.ListSectionsResponse, error) {
	id, err := parseUUID(req.GetEventId())
	if err != nil {
		return nil, err
	}
	rows, err := s.store.Sections(ctx, id)
	if err != nil {
		return nil, s.internal(ctx, "list sections", err)
	}
	out := make([]*pb.Section, len(rows))
	for i, sec := range rows {
		out[i] = &pb.Section{Id: sec.ID.String(), Name: sec.Name, Seats: int32(sec.Seats)}
	}
	return &pb.ListSectionsResponse{Sections: out}, nil
}

func (s *Server) ListSectionSeats(ctx context.Context, req *pb.ListSectionSeatsRequest) (*pb.ListSectionSeatsResponse, error) {
	id, err := parseUUID(req.GetSectionId())
	if err != nil {
		return nil, err
	}
	rows, err := s.store.SectionSeats(ctx, id)
	if err != nil {
		return nil, s.internal(ctx, "list seats", err)
	}
	out := make([]*pb.Seat, len(rows))
	for i, st := range rows {
		out[i] = &pb.Seat{Id: st.ID.String(), Row: st.RowLabel, Number: int32(st.Number), X: st.X, Y: st.Y}
	}
	return &pb.ListSectionSeatsResponse{Seats: out}, nil
}

func (s *Server) ListEventSeatIds(ctx context.Context, req *pb.ListEventSeatIdsRequest) (*pb.ListEventSeatIdsResponse, error) {
	id, err := parseUUID(req.GetEventId())
	if err != nil {
		return nil, err
	}
	ids, err := s.store.SeatIDsForEvent(ctx, id)
	if err != nil {
		return nil, s.internal(ctx, "list seat ids", err)
	}
	return &pb.ListEventSeatIdsResponse{SeatIds: uuidStrings(ids)}, nil
}

func (s *Server) CreateEvent(ctx context.Context, req *pb.CreateEventRequest) (*pb.CreateEventResponse, error) {
	venueID, err := parseUUID(req.GetVenueId())
	if err != nil {
		return nil, err
	}
	e, err := s.store.CreateEvent(ctx, venueID, req.GetTitle(),
		req.GetStartsAt().AsTime(), req.GetOnSaleAt().AsTime())
	if err != nil {
		return nil, s.internal(ctx, "create event", err)
	}
	return &pb.CreateEventResponse{Event: wireEvent(e)}, nil
}

func (s *Server) SetPrice(ctx context.Context, req *pb.SetPriceRequest) (*pb.SetPriceResponse, error) {
	eventID, err := parseUUID(req.GetEventId())
	if err != nil {
		return nil, err
	}
	sectionID, err := parseUUID(req.GetSectionId())
	if err != nil {
		return nil, err
	}
	if err := s.store.SetPrice(ctx, eventID, sectionID, req.GetPriceMinor()); err != nil {
		return nil, s.internal(ctx, "set price", err)
	}
	return &pb.SetPriceResponse{}, nil
}

func (s *Server) CountEventsStartingOn(ctx context.Context, req *pb.CountEventsStartingOnRequest) (*pb.CountEventsStartingOnResponse, error) {
	n, err := s.store.CountEventsStartingOn(ctx, req.GetDay().AsTime())
	if err != nil {
		return nil, s.internal(ctx, "count events", err)
	}
	return &pb.CountEventsStartingOnResponse{Count: int32(n)}, nil
}

func (s *Server) FindVenueByName(ctx context.Context, req *pb.FindVenueByNameRequest) (*pb.FindVenueByNameResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	id, err := s.store.FindVenueByName(ctx, req.GetName())
	switch {
	case errors.Is(err, store.ErrNotFound):
		// NOT_FOUND, specifically. The seeder has to tell "nobody bootstrapped the
		// cinema" (build it) from "the catalog is down" (do nothing, try later),
		// and collapsing the two is how a database blip creates a second arena.
		return nil, status.Error(codes.NotFound, "no such venue")
	case err != nil:
		return nil, s.internal(ctx, "find venue", err)
	}
	return &pb.FindVenueByNameResponse{VenueId: id.String()}, nil
}

func (s *Server) FirstSection(ctx context.Context, req *pb.FirstSectionRequest) (*pb.FirstSectionResponse, error) {
	id, err := parseUUID(req.GetVenueId())
	if err != nil {
		return nil, err
	}
	sec, err := s.store.FirstSectionID(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, status.Error(codes.NotFound, "venue has no sections")
	case err != nil:
		return nil, s.internal(ctx, "find first section", err)
	}
	return &pb.FirstSectionResponse{SectionId: sec.String()}, nil
}

func (s *Server) CreateVenue(ctx context.Context, req *pb.CreateVenueRequest) (*pb.CreateVenueResponse, error) {
	v, err := s.store.CreateVenue(ctx, req.GetName(), req.GetKind())
	if err != nil {
		return nil, s.internal(ctx, "create venue", err)
	}
	return &pb.CreateVenueResponse{VenueId: v.ID.String()}, nil
}

func (s *Server) AddSection(ctx context.Context, req *pb.AddSectionRequest) (*pb.AddSectionResponse, error) {
	id, err := parseUUID(req.GetVenueId())
	if err != nil {
		return nil, err
	}
	sec, err := s.store.AddSection(ctx, id, req.GetName(), int(req.GetRows()), int(req.GetSeatsPerRow()))
	if err != nil {
		return nil, s.internal(ctx, "add section", err)
	}
	return &pb.AddSectionResponse{SectionId: sec.String()}, nil
}

func wireEvent(e *store.Event) *pb.Event {
	return &pb.Event{
		Id:       e.ID.String(),
		VenueId:  e.VenueID.String(),
		Title:    e.Title,
		Venue:    e.VenueName,
		StartsAt: timestamppb.New(e.StartsAt),
		OnSaleAt: timestamppb.New(e.OnSaleAt),
	}
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

var errBadUUID = errors.New("malformed id")

func parseUUID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, status.Error(codes.InvalidArgument, errBadUUID.Error())
	}
	return id, nil
}
