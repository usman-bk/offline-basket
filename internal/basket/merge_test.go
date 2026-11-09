package basket

import (
	"fmt"
	"math/rand"
	"testing"
)

// act is a terse constructor so a test reads as a sequence of events rather
// than as a wall of struct literals.
func act(id, dev string, seq uint64, k Kind, sku string, qty int) Action {
	return Action{ID: id, DeviceID: dev, Seq: seq, Kind: k, SKU: sku, Qty: qty}
}

func cartOf(t *testing.T, as ...Action) *Cart {
	t.Helper()
	c := NewCart("u1")
	for _, a := range as {
		if err := a.Validate(); err != nil {
			t.Fatalf("test action %s is invalid: %v", a.ID, err)
		}
		c.Apply(a)
	}
	return c
}

func qty(c *Cart, sku string) int {
	if l := c.Lines[sku]; l != nil {
		return l.Qty()
	}
	return 0
}

// snapshot renders the settled cart as a comparable string. Comparing this
// rather than the struct means the assertion is about what the customer sees,
// and not about the bookkeeping underneath, which is allowed to differ.
func snapshot(c *Cart) string {
	s := ""
	for _, l := range c.Items() {
		s += fmt.Sprintf("%s=%d;", l.SKU, l.Qty())
	}
	return s
}

func TestApplyBasics(t *testing.T) {
	tests := []struct {
		name string
		acts []Action
		sku  string
		want int
	}{
		{"single add", []Action{act("a1", "d1", 1, KindAdd, "S1", 2)}, "S1", 2},
		{"add then add accumulates", []Action{
			act("a1", "d1", 1, KindAdd, "S1", 2),
			act("a2", "d1", 2, KindAdd, "S1", 3),
		}, "S1", 5},
		{"set_qty is absolute", []Action{
			act("a1", "d1", 1, KindAdd, "S1", 2),
			act("a2", "d1", 2, KindSetQty, "S1", 7),
		}, "S1", 7},
		{"set_qty discards the adds it overtook", []Action{
			act("a1", "d1", 1, KindAdd, "S1", 2),
			act("a2", "d1", 2, KindAdd, "S1", 3),
			act("a3", "d1", 3, KindSetQty, "S1", 1),
		}, "S1", 1},
		{"add after set_qty counts on top of it", []Action{
			act("a1", "d1", 1, KindSetQty, "S1", 4),
			act("a2", "d1", 2, KindAdd, "S1", 2),
		}, "S1", 6},
		{"remove zeroes the line", []Action{
			act("a1", "d1", 1, KindAdd, "S1", 5),
			act("a2", "d1", 2, KindRemove, "S1", 0),
		}, "S1", 0},
		{"set_qty zero is a removal", []Action{
			act("a1", "d1", 1, KindAdd, "S1", 5),
			act("a2", "d1", 2, KindSetQty, "S1", 0),
		}, "S1", 0},
		{"quantity is clamped at the ceiling", []Action{
			act("a1", "d1", 1, KindAdd, "S1", 60),
			act("a2", "d1", 2, KindAdd, "S1", 60),
		}, "S1", MaxQty},
		{"other lines are untouched", []Action{
			act("a1", "d1", 1, KindAdd, "S1", 2),
			act("a2", "d1", 2, KindAdd, "S2", 9),
			act("a3", "d1", 3, KindRemove, "S1", 0),
		}, "S2", 9},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := qty(cartOf(t, tc.acts...), tc.sku); got != tc.want {
				t.Fatalf("%s = %d, want %d", tc.sku, got, tc.want)
			}
		})
	}
}

// TestConcurrentAddsBothCount is the case last write wins gets wrong, and the
// reason a line holds a set of deltas instead of a single quantity. Two phones
// each add one of the same item with no knowledge of each other. The customer
// chose two things and the cart has to end up holding two.
func TestConcurrentAddsBothCount(t *testing.T) {
	c := cartOf(t,
		act("a1", "phone", 4, KindAdd, "S1", 1),
		act("a2", "tablet", 4, KindAdd, "S1", 1),
	)
	if got := qty(c, "S1"); got != 2 {
		t.Fatalf("two concurrent adds settled at %d, want 2: last write wins has eaten one", got)
	}
}

// TestLateAddCannotResurrect covers the tombstone. The tablet removes the item
// at seq 9. The phone, which has been offline, adds the same item at seq 3. The
// phone's action is genuinely older and the removal has to hold.
func TestLateAddCannotResurrect(t *testing.T) {
	c := cartOf(t,
		act("a1", "tablet", 8, KindAdd, "S1", 1),
		act("a2", "tablet", 9, KindRemove, "S1", 0),
		act("a3", "phone", 3, KindAdd, "S1", 1),
	)
	if got := qty(c, "S1"); got != 0 {
		t.Fatalf("removed item came back at qty %d", got)
	}
	if c.Count() != 0 {
		t.Fatalf("tombstone is being shown to the customer: %v", c.Items())
	}
	if _, kept := c.Lines["S1"]; !kept {
		t.Fatal("tombstone was deleted, so a later replay of the old add would resurrect the item")
	}
}

// TestAddAfterRemoveIsHonoured is the other half of the same rule. An add the
// customer made after the removal is not an echo and has to stand.
func TestAddAfterRemoveIsHonoured(t *testing.T) {
	c := cartOf(t,
		act("a1", "tablet", 8, KindAdd, "S1", 1),
		act("a2", "tablet", 9, KindRemove, "S1", 0),
		act("a3", "phone", 12, KindAdd, "S1", 3),
	)
	if got := qty(c, "S1"); got != 3 {
		t.Fatalf("add after a removal settled at %d, want 3", got)
	}
}

func TestClearBeatsEverythingBeforeIt(t *testing.T) {
	c := cartOf(t,
		act("a1", "d1", 1, KindAdd, "S1", 2),
		act("a2", "d1", 2, KindAdd, "S2", 2),
		act("a3", "d1", 5, KindClear, "", 0),
		act("a4", "phone", 4, KindAdd, "S3", 9), // older than the clear
	)
	if c.Count() != 0 {
		t.Fatalf("cart is not empty after a clear: %v", c.Items())
	}
	if got := qty(c, "S3"); got != 0 {
		t.Fatalf("an action older than the clear was applied, S3=%d", got)
	}
}

func TestClearDoesNotBlockLaterWrites(t *testing.T) {
	c := cartOf(t,
		act("a1", "d1", 5, KindClear, "", 0),
		act("a2", "d1", 6, KindAdd, "S1", 2),
	)
	if got := qty(c, "S1"); got != 2 {
		t.Fatalf("add after a clear settled at %d, want 2", got)
	}
}

// TestTieBreakIsDeterministic pins the rule that decides between two writes
// that are genuinely concurrent. The value chosen matters less than it being
// the same value every time and on every replica.
func TestTieBreakIsDeterministic(t *testing.T) {
	a := cartOf(t,
		act("x", "aaa", 7, KindSetQty, "S1", 1),
		act("y", "zzz", 7, KindSetQty, "S1", 2),
	)
	b := cartOf(t,
		act("y", "zzz", 7, KindSetQty, "S1", 2),
		act("x", "aaa", 7, KindSetQty, "S1", 1),
	)
	if qty(a, "S1") != qty(b, "S1") {
		t.Fatalf("tie broke two ways: %d and %d", qty(a, "S1"), qty(b, "S1"))
	}
	if got := qty(a, "S1"); got != 2 {
		t.Fatalf("higher device id should win the tie, got %d", got)
	}
}

// universe is a batch with every interesting shape in it: two devices, both
// kinds of write, a clear in the middle, a removal, and overlapping SKUs.
func universe() []Action {
	return []Action{
		act("a01", "phone", 1, KindAdd, "S1", 1),
		act("a02", "phone", 2, KindAdd, "S2", 2),
		act("a03", "tablet", 2, KindAdd, "S1", 1),
		act("a04", "tablet", 3, KindSetQty, "S2", 5),
		act("a05", "phone", 4, KindAdd, "S3", 1),
		act("a06", "tablet", 5, KindRemove, "S1", 0),
		act("a07", "phone", 6, KindAdd, "S1", 2),
		act("a08", "tablet", 7, KindAdd, "S2", 1),
		act("a09", "phone", 8, KindClear, "", 0),
		act("a10", "phone", 9, KindAdd, "S4", 3),
		act("a11", "tablet", 9, KindAdd, "S4", 1),
		act("a12", "tablet", 10, KindSetQty, "S5", 2),
		act("a13", "phone", 11, KindRemove, "S4", 0),
		act("a14", "tablet", 12, KindAdd, "S4", 7),
	}
}

// TestMergeIsCommutative is the property the replay depends on. The network
// decides what order a server sees these in, and the customer's cart may not.
func TestMergeIsCommutative(t *testing.T) {
	base := snapshot(cartOf(t, universe()...))
	if base == "" {
		t.Fatal("the fixture settles to an empty cart, so this proves nothing")
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 300; i++ {
		as := universe()
		rng.Shuffle(len(as), func(x, y int) { as[x], as[y] = as[y], as[x] })
		if got := snapshot(cartOf(t, as...)); got != base {
			t.Fatalf("order %d settled differently\n got %s\nwant %s", i, got, base)
		}
	}
}

// TestMergeIsIdempotent is the other property. A phone that got no response
// resends, and the duplicate must change nothing. Note that this holds inside
// Apply itself, without the dedupe table: that table stops the work being
// repeated, it is not what makes repeating it safe.
func TestMergeIsIdempotent(t *testing.T) {
	once := snapshot(cartOf(t, universe()...))
	twice := snapshot(cartOf(t, append(universe(), universe()...)...))
	if once != twice {
		t.Fatalf("replaying the batch changed the cart\n once %s\ntwice %s", once, twice)
	}
}

// TestIdempotentUnderShuffle is the two properties together, which is the
// situation a real retry creates: a partial batch landed, the rest is resent,
// and nothing guarantees the order.
func TestIdempotentUnderShuffle(t *testing.T) {
	base := snapshot(cartOf(t, universe()...))
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 200; i++ {
		as := append(universe(), universe()[:rng.Intn(14)]...)
		rng.Shuffle(len(as), func(x, y int) { as[x], as[y] = as[y], as[x] })
		if got := snapshot(cartOf(t, as...)); got != base {
			t.Fatalf("partial replay %d changed the cart\n got %s\nwant %s", i, got, base)
		}
	}
}

func TestVersionIsTheHighestSeqSeen(t *testing.T) {
	c := cartOf(t,
		act("a1", "d1", 3, KindAdd, "S1", 1),
		act("a2", "d2", 11, KindAdd, "S2", 1),
		act("a3", "d1", 4, KindAdd, "S3", 1),
	)
	if c.Version != 11 {
		t.Fatalf("version is %d, want 11", c.Version)
	}
}

// TestVersionAdvancesOnASupersededAction matters because the client raises its
// Lamport counter from this number. An action that lost still proves the
// server has seen that height, and failing to record it would let a device sit
// behind forever and lose every write it ever made.
func TestVersionAdvancesOnASupersededAction(t *testing.T) {
	c := cartOf(t, act("a1", "d1", 9, KindRemove, "S1", 0))
	before := c.Version
	c.Apply(act("a2", "d1", 20, KindAdd, "S1", 1))
	c.Apply(act("a3", "old", 2, KindAdd, "S1", 1)) // loses
	if c.Version <= before || c.Version != 20 {
		t.Fatalf("version is %d, want 20", c.Version)
	}
}

func TestPruneDropsOldTombstonesOnly(t *testing.T) {
	c := cartOf(t,
		act("a1", "d1", 1, KindAdd, "S1", 1),
		act("a2", "d1", 2, KindRemove, "S1", 0), // tombstone at seq 2
		act("a3", "d1", 9, KindAdd, "S2", 1),    // live line
		act("a4", "d1", 20, KindAdd, "S3", 1),
		act("a5", "d1", 21, KindRemove, "S3", 0), // tombstone at seq 21
	)
	if n := c.Prune(Stamp{Seq: 10, DeviceID: "zzzz"}); n != 1 {
		t.Fatalf("pruned %d, want 1", n)
	}
	if _, gone := c.Lines["S1"]; gone {
		t.Fatal("old tombstone survived the prune")
	}
	if _, kept := c.Lines["S3"]; !kept {
		t.Fatal("a tombstone newer than the window was pruned, so an old add can resurrect it")
	}
	if _, kept := c.Lines["S2"]; !kept {
		t.Fatal("prune deleted a live line")
	}
}

func TestCloneDoesNotShareState(t *testing.T) {
	c := cartOf(t, act("a1", "d1", 1, KindAdd, "S1", 1))
	cp := c.Clone()
	cp.Apply(act("a2", "d1", 2, KindAdd, "S1", 5))
	if qty(c, "S1") != 1 {
		t.Fatalf("writing to the clone changed the original, S1=%d", qty(c, "S1"))
	}
	if qty(cp, "S1") != 6 {
		t.Fatalf("clone did not take the write, S1=%d", qty(cp, "S1"))
	}
}

func TestItemsIsSortedAndHidesTombstones(t *testing.T) {
	c := cartOf(t,
		act("a1", "d1", 1, KindAdd, "S9", 1),
		act("a2", "d1", 2, KindAdd, "S2", 1),
		act("a3", "d1", 3, KindAdd, "S5", 1),
		act("a4", "d1", 4, KindRemove, "S5", 0),
	)
	got := c.Items()
	if len(got) != 2 || got[0].SKU != "S2" || got[1].SKU != "S9" {
		t.Fatalf("items = %v, want S2 then S9", got)
	}
}
