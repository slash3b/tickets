//go:build oversell

// MANUALLY TRIGGERED ONLY — same rule as the store-level oversell test.
//
//	make oversell
//
// THIS ONE GOES THROUGH gRPC. The original fires a thousand goroutines directly
// at the store, which is where the guarantee actually lives and is the right
// place to test it. But since the split, no user reaches the store that way: a
// hold crosses the gateway, a gRPC call, protobuf encoding, an interceptor chain
// and a status-code round trip before it touches a row.
//
// Every one of those layers is somewhere the answer could be mistranslated —
// and the split already produced exactly that bug once, when a lost race came
// back as 500 instead of 409 because two sentinel errors were different values.
// A guarantee that holds in the store and is misreported one layer up is not a
// guarantee anybody benefits from.
package oversell

import (
	"context"
	"errors"
	"github.com/slash3b/tickets/pkg/pgtest"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/slash3b/tickets/gen/tickets/v1"
	"github.com/slash3b/tickets/pkg/grpcx"
	"github.com/slash3b/tickets/pkg/obs"
	"github.com/slash3b/tickets/services/inventory"
	"github.com/slash3b/tickets/services/inventory/store"
)

func TestNoOversellThroughGRPC(t *testing.T) {
	const (
		seatCount = 10
		attempts  = 1000
	)

	dsn := pgtest.DSN(t, "oversell")
	ctx := context.Background()

	pool, err := obs.Pool(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, store.SchemaSQL); err != nil {
		t.Fatalf("schema: %v", err)
	}
	for _, q := range []string{`TRUNCATE inventory.holds CASCADE`, `TRUNCATE inventory.event_seats`} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("truncate: %v", err)
		}
	}

	inv := store.New(pool)
	eventID := uuid.New()
	seats := make([]uuid.UUID, seatCount)
	for i := range seats {
		seats[i] = uuid.New()
	}
	if _, err := inv.OpenEvent(ctx, eventID, seats); err != nil {
		t.Fatalf("open: %v", err)
	}

	// The REAL server, with the real interceptors — not a hand-rolled stub. A
	// harness that skipped grpcx.NewServer would not be testing the path that
	// runs in the cluster.
	lis := bufconn.Listen(1 << 20)
	srv := grpcx.NewServer(zap.NewNop())
	pb.RegisterInventoryServiceServer(srv, inventory.NewServer(inv, nil, zap.NewNop()))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := inventory.NewClient(conn)

	var won, lost, failed atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup

	for i := range attempts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them together, so they genuinely contend
			seat := seats[i%seatCount]
			_, err := client.Hold(ctx, eventID, []uuid.UUID{seat}, time.Minute)
			switch {
			case err == nil:
				won.Add(1)
			case errors.Is(err, store.ErrSeatsUnavailable):
				// THE POINT OF THIS TEST. codes.Aborted crossed the wire and the
				// client turned it back into the store's own sentinel, so callers
				// can still switch on it exactly as they did in-process.
				lost.Add(1)
			default:
				failed.Add(1)
				t.Errorf("unexpected error, neither a win nor a clean loss: %v", err)
			}
		}(i)
	}

	began := time.Now()
	close(start)
	wg.Wait()
	elapsed := time.Since(began)

	t.Logf("%d attempts over gRPC in %s (%.0f/sec): won=%d lost=%d failed=%d",
		attempts, elapsed.Round(time.Millisecond),
		float64(attempts)/elapsed.Seconds(), won.Load(), lost.Load(), failed.Load())

	if won.Load() != seatCount {
		t.Fatalf("OVERSELL: %d holds succeeded for %d seats", won.Load(), seatCount)
	}
	if failed.Load() != 0 {
		t.Fatalf("%d attempts failed in a way that was neither a win nor a lost race", failed.Load())
	}

	// And the database agrees, which is the only opinion that counts.
	statuses, err := inv.SeatStatuses(ctx, eventID, seats)
	if err != nil {
		t.Fatal(err)
	}
	held := 0
	for _, st := range statuses {
		if st == "held" {
			held++
		}
	}
	if held != seatCount {
		t.Fatalf("database says %d seats held, want %d", held, seatCount)
	}
}
