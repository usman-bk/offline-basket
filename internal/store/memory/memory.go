// Package memory keeps carts in process.
//
// It exists for the tests and for running the service locally without a
// database. It is not a cache in front of Postgres and must not become one:
// two instances of it hold two different carts, and the service is meant to
// run with more than one replica.
package memory

import (
	"context"
	"sync"

	"github.com/usmanvirk/offline-basket/internal/basket"
)

type seen struct {
	ids map[string]bool
	new []string
}

func (s *seen) Seen(id string) bool { return s.ids[id] }
func (s *seen) Record(id string)    { s.new = append(s.new, id) }

// Store is a concurrency safe in memory basket.Store.
type Store struct {
	mu    sync.Mutex
	carts map[string]*basket.Cart
	acted map[string]map[string]bool // user -> action id
}

func New() *Store {
	return &Store{carts: map[string]*basket.Cart{}, acted: map[string]map[string]bool{}}
}

// Tx holds one lock across every user rather than one per user. That is the
// wrong shape for a real workload and the right shape for a fake: the Postgres
// store locks a single cart row, and a fake that quietly allowed more
// concurrency than the real thing would hide exactly the races the real one
// has to survive.
func (s *Store) Tx(ctx context.Context, userID string, _ []string, fn func(*basket.Cart, basket.Dedupe) error) (*basket.Cart, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	c := s.carts[userID]
	if c == nil {
		c = basket.NewCart(userID)
	} else {
		c = c.Clone()
	}
	if s.acted[userID] == nil {
		s.acted[userID] = map[string]bool{}
	}
	d := &seen{ids: s.acted[userID]}

	if err := fn(c, d); err != nil {
		return nil, err // nothing committed, which is the point of the clone
	}

	s.carts[userID] = c
	for _, id := range d.new {
		s.acted[userID][id] = true
	}
	return c.Clone(), nil
}
