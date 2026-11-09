package basket

// Stamp orders two writes against each other.
//
// The obvious thing is to order by the time the phone recorded the action, and
// it does not work. A device that has been in a tunnel comes back with a clock
// that drifted, a user can set the clock by hand, and two phones in the same
// pocket disagree by seconds. Ordering a cart by that means the losing write is
// chosen by whichever device had the worse clock, which is not a rule anybody
// can explain to a customer who lost an item.
//
// So Seq is a Lamport clock. Each device keeps a counter, increments it for
// every action it records, and on every sync raises it past the Version the
// server returned. Two devices that have both synced therefore produce stamps
// that compare meaningfully, and a device that has never synced only ever
// loses to one that has, which is the behaviour you want: the cart the server
// already knows about is the one with more claim to be real.
//
// DeviceID breaks the tie. Two devices genuinely concurrent on the same line
// item can hold the same Seq, and something has to decide. Comparing the ids
// is arbitrary but it is deterministic and it is the same answer on every
// replica, which is the only property that matters here. Choosing by arrival
// order instead would mean the same batch replayed twice could settle two
// different ways, and the replay would stop being safe.
// ActionID is the last tie break, and it is not decoration. Seq and DeviceID
// together are only unique if every device increments its counter for every
// action it records, and that is a promise made by client code the server does
// not control. A device that ships a bug and reuses a sequence number produces
// two different writes with identical stamps, neither of which is After the
// other, and the merge quietly becomes order dependent: the same batch settles
// one way on a retry and another way on the first attempt. The randomised test
// in fuzz_test.go found that within a few thousand batches. ActionID is unique
// by construction, because it is the idempotency key the whole protocol
// already depends on, so adding it makes the order total and the bug
// unreachable rather than merely unlikely.
type Stamp struct {
	Seq      uint64
	DeviceID string
	ActionID string
}

// After reports whether s is the later write. The order is total: no two
// distinct actions compare equal.
func (s Stamp) After(o Stamp) bool {
	if s.Seq != o.Seq {
		return s.Seq > o.Seq
	}
	if s.DeviceID != o.DeviceID {
		return s.DeviceID > o.DeviceID
	}
	return s.ActionID > o.ActionID
}

// Zero reports whether the stamp has never been set.
func (s Stamp) Zero() bool { return s.Seq == 0 && s.DeviceID == "" && s.ActionID == "" }
