package sender

import (
	"context"

	"cineplex/internal/services/fetcher"
	"go.uber.org/zap"
)

type MovieFetcher interface {
	GetMovies(ctx context.Context) (fetcher.CineplexMoviesResponse, error)
}

type S struct {
	s MovieFetcher
	l *zap.Logger
}

func New(mv MovieFetcher, l *zap.Logger) *S {
	return &S{
		s: mv,
		l: l,
	}
}

func (s *S) Broadcast(ctx context.Context) error {
	mvs, err := s.s.GetMovies(ctx)
	if err != nil {
		s.l.Error("failed to fetch movies from cineplex.md", zap.Error(err))
	}

	_ = mvs.Movies

	// for _, mv := range mvs.Movies {
	// 	s.l.Info(mv.Title)
	// }

	s.l.Info("fetched movies", zap.Int("count", len(mvs.Movies)))

	return nil
}
