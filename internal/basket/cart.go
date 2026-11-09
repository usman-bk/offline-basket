package basket

import "sort"

// Delta is one relative change to a line, kept under the id of the action that
// made it.
type Delta struct {
	Qty int   `json:"qty"`
	At  Stamp `json:"at"`
}

// Line is one SKU in the cart.
//
// Last write wins is the right rule for an absolute write, and it is the wrong
// rule for a relative one. Two phones each add one of the same item while both
// are offline. Under last write wins the cart ends up holding one, because the
// later write replaces the earlier instead of counting it, and the customer is
// simply missing something they chose. Under a plain counter the opposite
// breaks: a replayed batch adds the item twice.
//
// So a line holds both. Base is the quantity left by the most recent absolute
// write, which is set_qty, remove_item or a cart clear, chosen by stamp. Deltas
// are the increments that happened after that write, held in a set keyed by the
// client's action id. Summing a set is order independent, and keying it by
// action id makes a replay a no-op rather than a double count, so the two
// properties this service has to have fall out of the same structure.
//
// A delta stamped at or before BaseAt has been overtaken by the absolute write
// and is dropped on sight, which is what keeps the set from growing forever.
type Line struct {
	SKU    string           `json:"sku"`
	Base   int              `json:"base"`
	BaseAt Stamp            `json:"base_at"`
	Deltas map[string]Delta `json:"deltas,omitempty"`
}

// Qty is the quantity the customer should see, clamped to what a line is
// allowed to hold. Clamping at read time rather than on write keeps the merge
// commutative: three increments of 50 arriving in any order all settle at the
// ceiling, where clamping each write in turn would depend on the order.
func (l Line) Qty() int {
	q := l.Base
	for _, d := range l.Deltas {
		q += d.Qty
	}
	if q < 0 {
		return 0
	}
	if q > MaxQty {
		return MaxQty
	}
	return q
}

// Tombstone reports whether this line is a record of a removal rather than a
// line the customer has. It is kept in the cart, and not deleted, so that an
// add from a device that had not yet heard about the removal cannot bring the
// item back. It stops being needed once every device has synced past it, which
// the server cannot know, so it is dropped on age instead. See Prune.
func (l Line) Tombstone() bool { return l.Qty() == 0 }

// Cart is one customer's basket, in the only form the server keeps it.
type Cart struct {
	UserID string           `json:"user_id"`
	Lines  map[string]*Line `json:"lines"`

	// Cleared is a cart wide tombstone. A clear has to beat every write that
	// came before it, including writes for SKUs the server has not seen yet,
	// so it cannot be recorded by tombstoning the lines that happen to exist.
	Cleared Stamp `json:"cleared"`

	// Version is the highest Seq the cart has absorbed. It goes back to the
	// device on every sync, and the device raises its own counter past it
	// before recording anything else. That is what makes Seq comparable
	// between two devices that have never spoken to each other.
	Version uint64 `json:"version"`
}

func NewCart(userID string) *Cart {
	return &Cart{UserID: userID, Lines: map[string]*Line{}}
}

// Apply folds one action into the cart and reports whether the cart changed.
//
// Apply is commutative and idempotent over a set of actions: the same set in
// any order produces the same cart, and applying one twice is the same as
// applying it once. Those two properties are what let the client replay a
// queue it is not sure the server received. They are asserted directly in
// merge_test.go rather than left as a claim here.
func (c *Cart) Apply(a Action) bool {
	if a.Seq > c.Version {
		c.Version = a.Seq
	}
	st := a.Stamp()

	// Anything from before the cart was cleared never happened.
	if !c.Cleared.Zero() && !st.After(c.Cleared) {
		return false
	}

	if a.Kind == KindClear {
		if !st.After(c.Cleared) {
			return false
		}
		c.Cleared = st

		// A clear is an absolute write of zero to every SKU, including the
		// ones this server has not been told about yet, which is why it is
		// recorded on the cart as well as applied to the lines.
		//
		// It is not a wipe. Emptying the map would also discard a write that
		// is newer than the clear, and whether such a write exists depends
		// only on the order the batch happened to arrive in. That made the
		// merge non commutative, and the property test in merge_test.go found
		// it: a shuffle that placed an add before the clear lost the item.
		// Each line is therefore compared against the clear on its own.
		changed := false
		for sku, l := range c.Lines {
			if !st.After(l.BaseAt) {
				continue // this line already has a newer absolute write
			}
			if l.Qty() != 0 {
				changed = true
			}
			l.Base, l.BaseAt = 0, st
			for id, d := range l.Deltas {
				if !d.At.After(st) {
					delete(l.Deltas, id)
				}
			}
			if len(l.Deltas) == 0 {
				l.Deltas = nil
			}
			// A line left at zero by the clear needs no tombstone of its own.
			// Cleared covers every SKU below this stamp, so the guard at the
			// top of Apply already refuses the older add that would have
			// brought it back.
			if l.Base == 0 && l.Deltas == nil {
				delete(c.Lines, sku)
			}
		}
		return changed
	}

	l := c.Lines[a.SKU]
	if l == nil {
		l = &Line{SKU: a.SKU}
		c.Lines[a.SKU] = l
	}
	before := l.Qty()

	switch a.Kind {
	case KindAdd:
		// A relative write. It only counts if it happened after the absolute
		// write currently standing, and it is held under its own action id so
		// that seeing it again changes nothing.
		if !l.BaseAt.Zero() && !st.After(l.BaseAt) {
			return false
		}
		if _, dup := l.Deltas[a.ID]; dup {
			return false
		}
		if l.Deltas == nil {
			l.Deltas = map[string]Delta{}
		}
		l.Deltas[a.ID] = Delta{Qty: a.Qty, At: st}

	case KindSetQty, KindRemove:
		// An absolute write. It replaces the base and everything relative that
		// came before it.
		if !st.After(l.BaseAt) {
			return false
		}
		qty := a.Qty
		if a.Kind == KindRemove {
			qty = 0
		}
		l.Base, l.BaseAt = qty, st
		for id, d := range l.Deltas {
			if !d.At.After(st) {
				delete(l.Deltas, id)
			}
		}
		if len(l.Deltas) == 0 {
			l.Deltas = nil
		}
	}

	return l.Qty() != before
}

// Item is one line as the client sees it, and it is deliberately two fields.
//
// The internal Line carries a base, a stamp and a set of deltas, and none of
// that is the client's business. Sending it would put the merge rule on the
// wire, oblige the app to sum a map to find out how many of something is in
// the basket, and make every future change to the bookkeeping a breaking
// change for two app releases that cannot be forced to update. It is also
// several times the bytes, on the request that happens when a phone has just
// come back onto a signal.
//
// So the wire type is separate from the stored type on purpose, and the
// server does the arithmetic.
type Item struct {
	SKU string `json:"sku"`
	Qty int    `json:"qty"`
}

// View is the cart as the client receives it, sorted so two calls and two
// replicas give the same bytes.
func (c *Cart) View() []Item {
	lines := c.Items()
	out := make([]Item, 0, len(lines))
	for _, l := range lines {
		out = append(out, Item{SKU: l.SKU, Qty: l.Qty()})
	}
	return out
}

// Items returns the live lines with their bookkeeping intact. It is for the
// store and the tests. Anything going to a client wants View.
func (c *Cart) Items() []Line {
	out := make([]Line, 0, len(c.Lines))
	for _, l := range c.Lines {
		if !l.Tombstone() {
			out = append(out, *l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SKU < out[j].SKU })
	return out
}

// Count is the number of distinct items, not the sum of their quantities.
func (c *Cart) Count() int { return len(c.Items()) }

// Prune drops tombstones left by a write older than keepBelow, and is how the
// cart stops growing without bound. Calling it with the stamp of the oldest
// unsynced device would be exact, and no server knows that, so it is driven
// off a retention window instead: a device offline for longer than the window
// can resurrect an item, and a device offline for longer than the window is
// also carrying prices that are no longer true. The window belongs to the
// caller for that reason, not to this package.
func (c *Cart) Prune(keepBelow Stamp) int {
	n := 0
	for sku, l := range c.Lines {
		if l.Tombstone() && !l.BaseAt.After(keepBelow) {
			delete(c.Lines, sku)
			n++
		}
	}
	return n
}

// Clone is a deep copy. The store hands one of these to a caller rather than
// the cart it holds, so a handler that mutates what it was given cannot
// corrupt state it does not own.
func (c *Cart) Clone() *Cart {
	cp := &Cart{UserID: c.UserID, Cleared: c.Cleared, Version: c.Version, Lines: make(map[string]*Line, len(c.Lines))}
	for sku, l := range c.Lines {
		nl := &Line{SKU: l.SKU, Base: l.Base, BaseAt: l.BaseAt}
		if l.Deltas != nil {
			nl.Deltas = make(map[string]Delta, len(l.Deltas))
			for id, d := range l.Deltas {
				nl.Deltas[id] = d
			}
		}
		cp.Lines[sku] = nl
	}
	return cp
}
