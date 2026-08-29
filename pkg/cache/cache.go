// Package cache is the seat-map projection in Redis.
//
// WHAT IT IS FOR, measured rather than assumed: opening a seat map cost ~586ms
// on average and 2.9 SECONDS at p95, on a page whose whole job is to feel live.
// The expensive half was inventory — 494ms to read 2,000 rows from
// inventory.event_seats, WHICH IS THE SAME TABLE THE SEAT CLAIMS ARE FIGHTING
// OVER. Browse traffic and the contended writer were on the same rows, each
// making the other slower.
//
// Kafka fixed the change traffic. It did nothing for the initial load, and the
// initial load was the expensive part.
//
// THE RULE FROM DESIGN.md, WHICH THIS OBEYS EXACTLY: Redis is NEVER the truth for
// inventory, and specifically never holds hold TTLs — Redis key expiry is not a
// reliable event source, and a hold that exists in Redis but not Postgres is a
// seat sold twice. The test of the design is that FLUSHING REDIS IN PRODUCTION
// MUST COST LATENCY AND NOTHING ELSE.
//
// That test is what shapes the API below. Every read can miss, every miss falls
// through to the owning service, and a partial answer is treated as a miss —
// because a seat map missing three seats is not a seat map, it is a wrong one.
package cache

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

type Cache struct {
	c  *redis.Client
	lg *zap.Logger
}

// New returns nil when addr is empty, and every method on a nil *Cache is a
// no-op miss. The system must run without Redis; that is the whole point of it
// being a cache.
func New(addr string, lg *zap.Logger) *Cache {
	if addr == "" {
		return nil
	}
	return &Cache{
		lg: lg,
		c: redis.NewClient(&redis.Options{
			Addr: addr,
			// Short timeouts, deliberately. A slow cache must degrade into a cache
			// miss, not into a slow request — the fallback path is already known
			// to work and is only a few hundred milliseconds.
			DialTimeout:  500 * time.Millisecond,
			ReadTimeout:  300 * time.Millisecond,
			WriteTimeout: 300 * time.Millisecond,
			PoolSize:     20,
		}),
	}
}

func (c *Cache) Close() error {
	if c == nil {
		return nil
	}
	return c.c.Close()
}

// statusKey holds every seat status for one event, as a hash of seat id.
//
// KEYED BY EVENT, NOT BY SECTION, because that is what the Kafka messages carry:
// a seat change knows its event and its seats, and which section a seat belongs
// to is catalog's business. Keying by section would mean the consumer had to ask
// catalog on every message.
func statusKey(eventID string) string { return "seatstatus:" + eventID }

// Statuses returns the cached status of exactly these seats.
//
// A PARTIAL HIT IS A MISS. If any requested seat is absent the whole thing is
// reported as a miss and the caller goes to inventory, because a seat map with
// three seats missing is not a stale seat map, it is a wrong one. This is also
// what makes a flushed Redis harmless: everything is absent, so everything misses.
func (c *Cache) Statuses(ctx context.Context, eventID string, seatIDs []string) (map[string]string, bool) {
	if c == nil || len(seatIDs) == 0 {
		return nil, false
	}
	vals, err := c.c.HMGet(ctx, statusKey(eventID), seatIDs...).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			c.lg.Debug("cache read failed", zap.Error(err))
		}
		return nil, false
	}

	out := make(map[string]string, len(seatIDs))
	for i, v := range vals {
		s, ok := v.(string)
		if !ok || s == "" {
			return nil, false // absent: treat the whole answer as a miss
		}
		out[seatIDs[i]] = s
	}
	return out, true
}

// PutStatuses fills the projection for seats just read from inventory.
//
// TTL is on the whole event hash rather than per seat: an event that nobody has
// looked at for a day is one nobody is buying, and letting it fall out entirely
// is simpler than tracking per-seat freshness.
func (c *Cache) PutStatuses(ctx context.Context, eventID string, statuses map[string]string) {
	if c == nil || len(statuses) == 0 {
		return
	}
	pairs := make([]any, 0, len(statuses)*2)
	for id, st := range statuses {
		pairs = append(pairs, id, st)
	}
	pipe := c.c.TxPipeline()
	pipe.HSet(ctx, statusKey(eventID), pairs...)
	pipe.Expire(ctx, statusKey(eventID), 24*time.Hour)
	if _, err := pipe.Exec(ctx); err != nil {
		c.lg.Debug("cache write failed", zap.Error(err))
	}
}

// Apply records a change that arrived on the Kafka stream.
//
// IT ONLY UPDATES FIELDS THAT ALREADY EXIST — HSET on a key that is absent would
// create a hash holding three seats out of two thousand, and the next read would
// see a partial hash. Since a partial hash is a miss, that is safe rather than
// wrong, but it wastes a round trip on every read until something repopulates it.
// Checking for the key first keeps the projection either complete or absent.
func (c *Cache) Apply(ctx context.Context, eventID string, seatIDs []string, status string) {
	if c == nil || len(seatIDs) == 0 || eventID == "" {
		return
	}
	key := statusKey(eventID)
	exists, err := c.c.Exists(ctx, key).Result()
	if err != nil || exists == 0 {
		return // nothing cached for this event; nothing to keep up to date
	}
	pairs := make([]any, 0, len(seatIDs)*2)
	for _, id := range seatIDs {
		pairs = append(pairs, id, status)
	}
	if err := c.c.HSet(ctx, key, pairs...).Err(); err != nil {
		c.lg.Debug("cache apply failed", zap.Error(err))
	}
}

// Ping reports whether Redis is reachable, for the log line at start-up. It is
// deliberately NOT wired into readiness: a service must not leave the rotation
// because its cache is down.
func (c *Cache) Ping(ctx context.Context) error {
	if c == nil {
		return errors.New("no cache configured")
	}
	return c.c.Ping(ctx).Err()
}
