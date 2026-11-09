// Package storetest is the contract every basket.Store has to satisfy.
//
// It exists because the memory store is what the handler tests run against
// and the Postgres store is what production runs against, and a difference
// between the two would only ever show up in production. Both are handed the
// same suite. A store that passes here is substitutable; one that does not is
// not, whatever its own tests say.
package storetest

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/usmanvirk/offline-basket/internal/basket"
)

// Factory returns a store with no state in it, and a name for the subtests.
type Factory func(t *testing.T) basket.Store

func add(id, dev string, seq uint64, sku string, qty int) basket.Action {
	return basket.Action{ID: id, DeviceID: dev, Seq: seq, Kind: basket.KindAdd, SKU: sku, Qty: qty}
}

func qtyOf(res basket.SyncResult, sku string) int {
	for _, l := range res.Cart {
		if l.SKU == sku {
			return l.Qty
		}
	}
	return 0
}

// Run executes the whole contract.
func Run(t *testing.T, newStore Factory) {
	t.Helper()
	tests := []struct {
		name string
		fn   func(*testing.T, basket.Store)
	}{
		{"empty cart", emptyCart},
		{"round trip", roundTrip},
		{"dedupe survives a reload", dedupeSurvivesReload},
		{"absolute write persists", absoluteWritePersists},
		{"clear persists", clearPersists},
		{"tombstone persists", tombstonePersists},
		{"deltas persist", deltasPersist},
		{"users are isolated", usersIsolated},
		{"action ids are per user", actionIDsPerUser},
		{"version persists", versionPersists},
		{"error rolls back", errorRollsBack},
		{"concurrent syncs lose nothing", concurrentSyncs},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, newStore(t)) })
	}
}

func settle(t *testing.T, s basket.Store, user string, as ...basket.Action) basket.SyncResult {
	t.Helper()
	res, err := basket.Sync(context.Background(), s, user, as)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return res
}

func emptyCart(t *testing.T, s basket.Store) {
	res := settle(t, s, "u1")
	if len(res.Cart) != 0 || res.Version != 0 {
		t.Fatalf("a cart that was never written is not empty: %+v", res)
	}
}

func roundTrip(t *testing.T, s basket.Store) {
	settle(t, s, "u1", add("a1", "d1", 1, "S1", 2))
	res := settle(t, s, "u1")
	if qtyOf(res, "S1") != 2 {
		t.Fatalf("S1 came back as %d, want 2", qtyOf(res, "S1"))
	}
}

// dedupeSurvivesReload is the one that separates a store from a cache. The
// idempotency record has to outlive the request that made it, or a retry
// minutes later double counts.
func dedupeSurvivesReload(t *testing.T, s basket.Store) {
	a := add("a1", "d1", 1, "S1", 2)
	settle(t, s, "u1", a)
	res := settle(t, s, "u1", a)
	if qtyOf(res, "S1") != 2 {
		t.Fatalf("replay in a later transaction double counted: S1=%d", qtyOf(res, "S1"))
	}
	if c := res.Counts(); c[basket.Duplicate] != 1 {
		t.Fatalf("outcomes %v, want one duplicate", c)
	}
}

func absoluteWritePersists(t *testing.T, s basket.Store) {
	settle(t, s, "u1", add("a1", "d1", 1, "S1", 2))
	settle(t, s, "u1", basket.Action{ID: "a2", DeviceID: "d1", Seq: 2, Kind: basket.KindSetQty, SKU: "S1", Qty: 9})
	if got := qtyOf(settle(t, s, "u1"), "S1"); got != 9 {
		t.Fatalf("S1=%d, want 9", got)
	}
}

func clearPersists(t *testing.T, s basket.Store) {
	settle(t, s, "u1", add("a1", "d1", 1, "S1", 2), add("a2", "d1", 2, "S2", 3))
	settle(t, s, "u1", basket.Action{ID: "a3", DeviceID: "d1", Seq: 5, Kind: basket.KindClear})

	// A fresh transaction has to reload the cart wide tombstone, or this older
	// action from a device that was offline comes back to life.
	res := settle(t, s, "u1", add("a4", "phone", 4, "S9", 1))
	if len(res.Cart) != 0 {
		t.Fatalf("an action older than a persisted clear was applied: %v", res.Cart)
	}
}

func tombstonePersists(t *testing.T, s basket.Store) {
	settle(t, s, "u1",
		add("a1", "d1", 8, "S1", 1),
		basket.Action{ID: "a2", DeviceID: "d1", Seq: 9, Kind: basket.KindRemove, SKU: "S1"})
	res := settle(t, s, "u1", add("a3", "phone", 3, "S1", 1))
	if qtyOf(res, "S1") != 0 {
		t.Fatalf("a removed item was resurrected across transactions: S1=%d", qtyOf(res, "S1"))
	}
}

func deltasPersist(t *testing.T, s basket.Store) {
	settle(t, s, "u1", add("a1", "d1", 1, "S1", 2))
	settle(t, s, "u1", add("a2", "d2", 1, "S1", 3))
	if got := qtyOf(settle(t, s, "u1"), "S1"); got != 5 {
		t.Fatalf("S1=%d, want 5: a delta was lost between transactions", got)
	}
}

func usersIsolated(t *testing.T, s basket.Store) {
	settle(t, s, "u1", add("a1", "d1", 1, "S1", 2))
	if len(settle(t, s, "u2").Cart) != 0 {
		t.Fatal("u2 can see u1's cart")
	}
}

func actionIDsPerUser(t *testing.T, s basket.Store) {
	settle(t, s, "u1", add("same", "d1", 1, "S1", 2))
	res := settle(t, s, "u2", add("same", "d2", 1, "S9", 7))
	if qtyOf(res, "S9") != 7 {
		t.Fatalf("u2's action was swallowed as u1's duplicate: %v", res.Cart)
	}
}

func versionPersists(t *testing.T, s basket.Store) {
	settle(t, s, "u1", add("a1", "d1", 42, "S1", 1))
	if v := settle(t, s, "u1").Version; v != 42 {
		t.Fatalf("version came back as %d, want 42", v)
	}
}

// errorRollsBack checks that a failure inside the merge leaves nothing behind.
// A cart written without its action ids double counts on the next retry, and a
// half written cart is worse than no write at all.
func errorRollsBack(t *testing.T, s basket.Store) {
	settle(t, s, "u1", add("a1", "d1", 1, "S1", 2))

	boom := fmt.Errorf("deliberate")
	_, err := s.Tx(context.Background(), "u1", []string{"a2"}, func(c *basket.Cart, d basket.Dedupe) error {
		c.Apply(add("a2", "d1", 2, "S2", 5))
		d.Record("a2")
		return boom
	})
	if err == nil {
		t.Fatal("Tx swallowed the error from fn")
	}

	res := settle(t, s, "u1")
	if len(res.Cart) != 1 || qtyOf(res, "S2") != 0 {
		t.Fatalf("a failed transaction left state behind: %v", res.Cart)
	}
	// And the action id must not have been recorded, or the retry is lost.
	again := settle(t, s, "u1", add("a2", "d1", 2, "S2", 5))
	if qtyOf(again, "S2") != 5 {
		t.Fatalf("the rolled back action id was recorded, so the retry was dropped: S2=%d", qtyOf(again, "S2"))
	}
}

// concurrentSyncs is the two device case, run hard enough to find a missing
// lock. Every action has a distinct id, so every one of them must count.
func concurrentSyncs(t *testing.T, s basket.Store) {
	const devices, each = 6, 12
	var wg sync.WaitGroup
	errs := make(chan error, devices*each)

	for d := 0; d < devices; d++ {
		wg.Add(1)
		go func(d int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				_, err := basket.Sync(context.Background(), s, "u1", []basket.Action{
					add(fmt.Sprintf("d%d-%d", d, i), fmt.Sprintf("dev%d", d), uint64(i+1), "S1", 1),
				})
				if err != nil {
					errs <- err
					return
				}
			}
		}(d)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent sync failed: %v", err)
	}

	// 72 adds of one, deliberately under the 99 ceiling: clamping would hide
	// a lost write behind a number that still looks full. The count has to be
	// exact.
	if want, got := devices*each, qtyOf(settle(t, s, "u1"), "S1"); got != want {
		t.Fatalf("%d concurrent adds settled at %d: %d writes were lost", want, got, want-got)
	}
}
