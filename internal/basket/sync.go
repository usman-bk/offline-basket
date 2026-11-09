package basket

import (
	"context"
	"fmt"
)

// Dedupe is the record of which action ids this user's cart has already
// absorbed. It is per user rather than global: action ids are generated on
// devices and the server does not get to assume they are unique across the
// whole estate.
type Dedupe interface {
	Seen(id string) bool
	Record(id string)
}

// Store owns the cart and the dedupe record together.
//
// Tx must hold the cart against concurrent writers for the duration of fn, and
// must commit the cart and the action ids in one atomic step. Those two things
// going in separately is the bug the whole design is trying to avoid: a cart
// written without its action ids is a cart that will double count on the next
// retry, and action ids written without their cart is a cart that will silently
// drop the actions it was asked to apply.
//
// Two devices belonging to the same person sync at the same moment often
// enough to matter, and a phone that got no response retries while the first
// attempt may still be running, so this is a routine case rather than a rare
// one.
// actionIDs is every id in the batch, passed in so an implementation backed by
// a database can fetch the dedupe set in one statement instead of a round trip
// per action. A batch of 200 on a link that has just come back is the case
// this is for.
type Store interface {
	Tx(ctx context.Context, userID string, actionIDs []string, fn func(*Cart, Dedupe) error) (*Cart, error)
}

// Outcome is what happened to one action in a batch.
type Outcome string

const (
	// Applied means the action was new and the cart absorbed it.
	Applied Outcome = "applied"
	// Duplicate means this exact action id had already been absorbed. The
	// client should drop it from its queue. It is a success, not an error:
	// it is the normal answer to a retry of a batch that did arrive.
	Duplicate Outcome = "duplicate"
	// Superseded means the action was new, and lost to a later write. The
	// client should also drop it. The distinction from Applied exists because
	// it is the single most useful thing to have in a log when a customer
	// says an item disappeared.
	Superseded Outcome = "superseded"
	// Rejected means the action could not be read. It is the only outcome the
	// client should not simply forget, and in practice it means a client bug.
	Rejected Outcome = "rejected"
)

// ActionResult pairs an action id with what became of it.
type ActionResult struct {
	ID      string  `json:"id"`
	Outcome Outcome `json:"outcome"`
	Error   string  `json:"error,omitempty"`
}

// SyncResult is the whole of what the server sends back.
//
// The cart comes back entire, not as a patch. A patch is smaller and it
// obliges the client to hold a correct copy to apply it to, which is the one
// thing a client that has been offline for a week cannot be assumed to have.
// Sending the settled cart means the app never computes the answer and never
// has to guess which of its own actions survived. For a cart, which is tens of
// lines rather than thousands, that trade is not close.
type SyncResult struct {
	Cart    []Item         `json:"cart"`
	Version uint64         `json:"version"`
	Results []ActionResult `json:"results"`
}

// Counts summarises the outcomes, for logging and for the tests.
func (r SyncResult) Counts() map[Outcome]int {
	m := map[Outcome]int{}
	for _, x := range r.Results {
		m[x.Outcome]++
	}
	return m
}

// ErrBatchTooBig is returned whole: nothing in an oversized batch is applied,
// so the client can resend it split without reasoning about a partial result.
type ErrTooBig struct{ N int }

func (e ErrTooBig) Error() string {
	return fmt.Sprintf("batch of %d actions, limit is %d", e.N, MaxBatch)
}

// Sync settles a replay queue against the stored cart and returns the result.
//
// A malformed action does not fail the batch. The queue was recorded over
// hours on a device that may have shipped a bug, and refusing the whole batch
// would strand every good action behind the bad one with no way for the client
// to make progress. Each action is reported on by id instead.
func Sync(ctx context.Context, s Store, userID string, actions []Action) (SyncResult, error) {
	if len(actions) > MaxBatch {
		return SyncResult{}, ErrTooBig{N: len(actions)}
	}
	if userID == "" {
		return SyncResult{}, fmt.Errorf("no user id")
	}

	res := SyncResult{Results: make([]ActionResult, 0, len(actions))}

	ids := make([]string, 0, len(actions))
	for _, a := range actions {
		if a.ID != "" {
			ids = append(ids, a.ID)
		}
	}

	cart, err := s.Tx(ctx, userID, ids, func(c *Cart, d Dedupe) error {
		for _, a := range actions {
			switch verr := a.Validate(); {
			case verr != nil:
				res.Results = append(res.Results, ActionResult{
					ID: a.ID, Outcome: Rejected, Error: verr.Error()})
			case d.Seen(a.ID):
				// Seen before. Not re-applied, and not an error: this is a
				// retry of a batch that landed.
				res.Results = append(res.Results, ActionResult{ID: a.ID, Outcome: Duplicate})
			default:
				changed := c.Apply(a)
				d.Record(a.ID)
				out := Superseded
				if changed {
					out = Applied
				}
				res.Results = append(res.Results, ActionResult{ID: a.ID, Outcome: out})
			}
		}
		return nil
	})
	if err != nil {
		return SyncResult{}, err
	}

	res.Cart, res.Version = cart.View(), cart.Version
	return res, nil
}
