package postgres_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/usmanvirk/offline-basket/internal/basket"
	"github.com/usmanvirk/offline-basket/internal/store/postgres"
	"github.com/usmanvirk/offline-basket/internal/store/storetest"
)

// These tests need a real server. There is no embedded Postgres here and no
// mock of one: the things worth testing in this file are the lock, the
// rollback and the jsonb round trip, and a mock of Postgres would be a mock of
// exactly the behaviour under test. Without DATABASE_URL the file skips, and
// CI sets it against a service container.
func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set, skipping the Postgres tests")
	}
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Ping(ctx); err != nil {
		t.Fatalf("pinging %s: %v", redact(dsn), err)
	}
	t.Cleanup(p.Close)
	return p
}

func redact(dsn string) string {
	if i := strings.Index(dsn, "@"); i > 0 {
		return "..." + dsn[i:]
	}
	return dsn
}

var schemaOnce atomic.Bool

// fresh gives each subtest its own store with no rows in it. The tables are
// truncated rather than dropped so the schema is created once.
func fresh(t *testing.T) basket.Store {
	t.Helper()
	p := pool(t)
	s := postgres.New(p)
	ctx := context.Background()
	if !schemaOnce.Load() {
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		schemaOnce.Store(true)
	}
	if _, err := p.Exec(ctx, `TRUNCATE carts, cart_lines, applied_actions`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

// TestConformance runs the same suite the memory store runs. A difference
// between the two stores would otherwise only appear in production.
func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) basket.Store { return fresh(t) })
}

// TestMigrateIsIdempotent covers the fact that it runs on every start, on
// every replica, at the same time.
func TestMigrateIsIdempotent(t *testing.T) {
	p := pool(t)
	s := postgres.New(p)
	for i := 0; i < 3; i++ {
		if err := s.Migrate(context.Background()); err != nil {
			t.Fatalf("migrate run %d: %v", i+1, err)
		}
	}
}

// TestDeltasRoundTripThroughJSONB is specific to this store. The merge keeps
// the surviving relative writes in a map, the column is jsonb, and a mistake
// in either direction shows up as a quantity that is quietly wrong rather
// than as an error.
func TestDeltasRoundTripThroughJSONB(t *testing.T) {
	s := fresh(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := basket.Sync(ctx, s, "u1", []basket.Action{{
			ID: fmt.Sprintf("a%d", i), DeviceID: fmt.Sprintf("dev%d", i), Seq: 1,
			Kind: basket.KindAdd, SKU: "S1", Qty: 2,
		}}); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}

	res, err := basket.Sync(ctx, s, "u1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Cart) != 1 || res.Cart[0].Qty != 10 {
		t.Fatalf("five concurrent adds of two came back as %v, want 10", res.Cart)
	}

	// Counted in the column rather than through the API, because the column
	// is what this test is about. The wire type carries a quantity and not
	// the bookkeeping behind it.
	p := pool(t)
	var n int
	if err := p.QueryRow(ctx,
		`SELECT jsonb_object_keys_count FROM (
		   SELECT count(*) AS jsonb_object_keys_count
		     FROM cart_lines, jsonb_object_keys(deltas)
		    WHERE user_id = 'u1' AND sku = 'S1') q`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("%d deltas survived the round trip, want 5", n)
	}

	// An absolute write has to clear them out of the column, or the jsonb
	// grows without bound.
	if _, err := basket.Sync(ctx, s, "u1", []basket.Action{{
		ID: "abs", DeviceID: "dev9", Seq: 9, Kind: basket.KindSetQty, SKU: "S1", Qty: 3,
	}}); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := p.QueryRow(ctx,
		`SELECT count(*) FROM cart_lines, jsonb_object_keys(deltas)
		  WHERE user_id = 'u1' AND sku = 'S1'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("an absolute write left %d deltas behind in the column", left)
	}
	res, _ = basket.Sync(ctx, s, "u1", nil)
	if res.Cart[0].Qty != 3 {
		t.Fatalf("after the absolute write the cart reads %d, want 3", res.Cart[0].Qty)
	}
}

// TestClearDeletesRowsNotJustValues checks the DELETE in writeCart. The merge
// drops a cleared line from the map, and if the table keeps the row the cart
// reloads with a line the merge thinks it deleted.
func TestClearDeletesRowsNotJustValues(t *testing.T) {
	s := fresh(t)
	ctx := context.Background()
	p := pool(t)

	if _, err := basket.Sync(ctx, s, "u1", []basket.Action{
		{ID: "a1", DeviceID: "d1", Seq: 1, Kind: basket.KindAdd, SKU: "S1", Qty: 2},
		{ID: "a2", DeviceID: "d1", Seq: 2, Kind: basket.KindAdd, SKU: "S2", Qty: 2},
		{ID: "a3", DeviceID: "d1", Seq: 3, Kind: basket.KindClear},
	}); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM cart_lines WHERE user_id = 'u1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d line rows survived a clear", n)
	}
}

// TestSweepApplied covers the retention sweep, including the part that
// matters: it must not touch records inside the window.
func TestSweepApplied(t *testing.T) {
	s := fresh(t)
	ctx := context.Background()
	p := pool(t)
	ps := s.(*postgres.Store)

	if _, err := basket.Sync(ctx, s, "u1", []basket.Action{
		{ID: "recent", DeviceID: "d1", Seq: 1, Kind: basket.KindAdd, SKU: "S1", Qty: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx,
		`INSERT INTO applied_actions (user_id, action_id, applied_at)
		 VALUES ('u1', 'ancient', now() - interval '60 days')`); err != nil {
		t.Fatal(err)
	}

	n, err := ps.SweepApplied(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("sweep removed %d rows, want 1", n)
	}

	var left string
	if err := p.QueryRow(ctx,
		`SELECT action_id FROM applied_actions WHERE user_id = 'u1'`).Scan(&left); err != nil {
		t.Fatalf("the sweep took the record inside the window: %v", err)
	}
	if left != "recent" {
		t.Fatalf("the wrong record survived: %s", left)
	}
}

// TestLockSerialisesTwoTransactions proves the FOR UPDATE is doing something.
// The second transaction must not be able to read the cart until the first
// has committed, so it has to observe the first one's write.
func TestLockSerialisesTwoTransactions(t *testing.T) {
	s := fresh(t)
	ctx := context.Background()

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		_, err := s.Tx(ctx, "u1", []string{"first"}, func(c *basket.Cart, d basket.Dedupe) error {
			c.Apply(basket.Action{ID: "first", DeviceID: "d1", Seq: 1, Kind: basket.KindAdd, SKU: "S1", Qty: 4})
			d.Record("first")
			close(entered)
			<-release // hold the lock
			return nil
		})
		done <- err
	}()

	<-entered
	second := make(chan int, 1)
	go func() {
		var saw int
		_, err := s.Tx(ctx, "u1", nil, func(c *basket.Cart, _ basket.Dedupe) error {
			if l := c.Lines["S1"]; l != nil {
				saw = l.Qty()
			}
			return nil
		})
		if err != nil {
			t.Errorf("second transaction: %v", err)
		}
		second <- saw
	}()

	select {
	case saw := <-second:
		t.Fatalf("the second transaction read the cart while the first held the lock, saw S1=%d", saw)
	case <-time.After(250 * time.Millisecond):
		// Blocked, which is the point.
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first transaction: %v", err)
	}
	if saw := <-second; saw != 4 {
		t.Fatalf("after the lock was released the second transaction saw S1=%d, want 4", saw)
	}
}
