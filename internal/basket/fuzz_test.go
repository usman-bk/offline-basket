package basket

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"
)

// The committed run is fixed so a failure is reproducible and CI takes a
// second. A longer search is a different job: set BASKET_FUZZ_TRIALS and
// BASKET_FUZZ_SEED to widen it. The clear bug and the duplicate stamp bug were
// both found at a seed other than the default one.
func trials(def int) int {
	if v := os.Getenv("BASKET_FUZZ_TRIALS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func seed(def int64) int64 {
	if v := os.Getenv("BASKET_FUZZ_SEED"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

// randomBatch builds a batch with the shapes that break merges: repeated SKUs,
// several devices, stamps that collide, and clears scattered through it.
func randomBatch(rng *rand.Rand, n int) []Action {
	devices := []string{"phone", "tablet", "laptop", "kiosk"}
	skus := []string{"S1", "S2", "S3"}
	kinds := []Kind{KindAdd, KindAdd, KindAdd, KindSetQty, KindRemove, KindClear}

	as := make([]Action, 0, n)
	for i := 0; i < n; i++ {
		k := kinds[rng.Intn(len(kinds))]
		a := Action{
			ID:       fmt.Sprintf("r%03d", i),
			DeviceID: devices[rng.Intn(len(devices))],
			// A small stamp range on purpose. Ties between devices are the
			// interesting case and a wide range would almost never produce one.
			Seq:  uint64(1 + rng.Intn(12)),
			Kind: k,
		}
		if k != KindClear {
			a.SKU = skus[rng.Intn(len(skus))]
			switch k {
			case KindAdd:
				a.Qty = 1 + rng.Intn(3)
			case KindSetQty:
				a.Qty = rng.Intn(6)
			}
		}
		as = append(as, a)
	}
	return as
}

func settle(as []Action) string {
	c := NewCart("u")
	for _, a := range as {
		c.Apply(a)
	}
	s := ""
	for _, l := range c.Items() {
		s += fmt.Sprintf("%s=%d;", l.SKU, l.Qty())
	}
	return s
}

// TestRandomBatchesAreOrderIndependent searches for an ordering that settles
// differently. The clear bug that the fixed fixture caught was found this way
// first: a shuffle put an add in front of a clear and the item vanished.
func TestRandomBatchesAreOrderIndependent(t *testing.T) {
	rng := rand.New(rand.NewSource(seed(20261022)))
	for trial := 0; trial < trials(4000); trial++ {
		as := randomBatch(rng, 1+rng.Intn(25))
		want := settle(as)
		for s := 0; s < 6; s++ {
			sh := append([]Action(nil), as...)
			rng.Shuffle(len(sh), func(i, j int) { sh[i], sh[j] = sh[j], sh[i] })
			if got := settle(sh); got != want {
				t.Fatalf("trial %d shuffle %d settled differently\n got %q\nwant %q\nbatch %#v", trial, s, got, want, as)
			}
		}
	}
}

// TestRandomBatchesAreIdempotent replays a random slice of the batch on top of
// itself, which is what a retry after a timeout actually looks like.
func TestRandomBatchesAreIdempotent(t *testing.T) {
	rng := rand.New(rand.NewSource(seed(99)))
	for trial := 0; trial < trials(4000); trial++ {
		as := randomBatch(rng, 1+rng.Intn(25))
		want := settle(as)
		dup := append(append([]Action(nil), as...), as[:rng.Intn(len(as)+1)]...)
		rng.Shuffle(len(dup), func(i, j int) { dup[i], dup[j] = dup[j], dup[i] })
		if got := settle(dup); got != want {
			t.Fatalf("trial %d: replay changed the cart\n got %q\nwant %q\nbatch %#v", trial, got, want, as)
		}
	}
}

// FuzzApplyNeverPanics is the cheap safety net over the parser and the merge.
// Nothing a client can send should take the service down.
func FuzzApplyNeverPanics(f *testing.F) {
	f.Add("a1", "d1", uint64(1), "add_item", "S1", 2)
	f.Add("", "", uint64(0), "clear_cart", "", -5)
	f.Fuzz(func(t *testing.T, id, dev string, seq uint64, kind, sku string, qty int) {
		a := Action{ID: id, DeviceID: dev, Seq: seq, Kind: Kind(kind), SKU: sku, Qty: qty}
		c := NewCart("u")
		if a.Validate() == nil {
			c.Apply(a)
			c.Items()
			c.Clone()
		}
	})
}
