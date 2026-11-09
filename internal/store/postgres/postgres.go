// Package postgres is the store the service actually runs on.
package postgres

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/usmanvirk/offline-basket/internal/basket"
)

//go:embed schema.sql
var Schema string

type Store struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Migrate applies the schema. It is idempotent, and it is deliberately the
// whole of the migration story in this repository: one file, applied on start.
// A service with one table group and no history does not need a migration
// tool, and pretending otherwise would be the more complicated choice.
func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, Schema)
	return err
}

type dedupe struct {
	seen map[string]bool
	add  []string
}

func (d *dedupe) Seen(id string) bool { return d.seen[id] }
func (d *dedupe) Record(id string)    { d.add = append(d.add, id) }

// Tx runs the merge with the cart row locked.
//
// The lock is SELECT FOR UPDATE on carts, taken before anything is read. Two
// devices belonging to one person sync at the same moment routinely, and a
// phone that timed out retries while the first attempt may still be in
// flight, so this is the normal case and not a rare one. Without the lock the
// two transactions read the same cart, merge separately, and the second write
// silently discards the first one's actions while still recording their ids
// as applied, which is the worst available outcome: the items are gone and the
// client has been told they landed.
//
// Serializable isolation would also be correct and would mean handling 40001
// retries in the handler for a conflict that is expected rather than rare.
// Taking the lock explicitly makes the contention visible at the point it
// happens, and the critical section is a few hundred microseconds of in memory
// merging.
func (s *Store) Tx(ctx context.Context, userID string, actionIDs []string, fn func(*basket.Cart, basket.Dedupe) error) (*basket.Cart, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Create the row if it is not there, then lock it. ON CONFLICT DO NOTHING
	// rather than checking first: two devices sending a first action at the
	// same time both find no row, and only one of them can insert it.
	if _, err := tx.Exec(ctx,
		`INSERT INTO carts (user_id) VALUES ($1) ON CONFLICT (user_id) DO NOTHING`, userID); err != nil {
		return nil, fmt.Errorf("ensure cart: %w", err)
	}

	cart := basket.NewCart(userID)
	var clearedSeq, version int64
	var clearedDev, clearedAct string
	if err := tx.QueryRow(ctx,
		`SELECT cleared_seq, cleared_dev, cleared_act, version
		   FROM carts WHERE user_id = $1 FOR UPDATE`, userID,
	).Scan(&clearedSeq, &clearedDev, &clearedAct, &version); err != nil {
		return nil, fmt.Errorf("lock cart: %w", err)
	}
	cart.Cleared = basket.Stamp{Seq: uint64(clearedSeq), DeviceID: clearedDev, ActionID: clearedAct}
	cart.Version = uint64(version)

	rows, err := tx.Query(ctx,
		`SELECT sku, base, base_seq, base_dev, base_act, deltas
		   FROM cart_lines WHERE user_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("load lines: %w", err)
	}
	for rows.Next() {
		var l basket.Line
		var seq int64
		var dev, act string
		var raw []byte
		if err := rows.Scan(&l.SKU, &l.Base, &seq, &dev, &act, &raw); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan line: %w", err)
		}
		l.BaseAt = basket.Stamp{Seq: uint64(seq), DeviceID: dev, ActionID: act}
		if len(raw) > 0 {
			var d map[string]basket.Delta
			if err := json.Unmarshal(raw, &d); err != nil {
				rows.Close()
				return nil, fmt.Errorf("deltas for %s: %w", l.SKU, err)
			}
			if len(d) > 0 {
				l.Deltas = d
			}
		}
		line := l
		cart.Lines[l.SKU] = &line
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load lines: %w", err)
	}

	d := &dedupe{seen: map[string]bool{}}
	if len(actionIDs) > 0 {
		// One statement for the whole batch. A query per action would be 200
		// round trips inside a held lock.
		seenRows, err := tx.Query(ctx,
			`SELECT action_id FROM applied_actions
			  WHERE user_id = $1 AND action_id = ANY($2)`, userID, actionIDs)
		if err != nil {
			return nil, fmt.Errorf("load applied: %w", err)
		}
		for seenRows.Next() {
			var id string
			if err := seenRows.Scan(&id); err != nil {
				seenRows.Close()
				return nil, fmt.Errorf("scan applied: %w", err)
			}
			d.seen[id] = true
		}
		seenRows.Close()
		if err := seenRows.Err(); err != nil {
			return nil, fmt.Errorf("load applied: %w", err)
		}
	}

	if err := fn(cart, d); err != nil {
		return nil, err
	}

	if err := writeCart(ctx, tx, cart, d.add); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return cart.Clone(), nil
}

// writeCart persists the merged cart and the action ids in the same
// transaction. Splitting them is the bug described on basket.Store.
func writeCart(ctx context.Context, tx pgx.Tx, c *basket.Cart, applied []string) error {
	if _, err := tx.Exec(ctx,
		`UPDATE carts
		    SET cleared_seq = $2, cleared_dev = $3, cleared_act = $4,
		        version = $5, updated_at = now()
		  WHERE user_id = $1`,
		c.UserID, int64(c.Cleared.Seq), c.Cleared.DeviceID, c.Cleared.ActionID, int64(c.Version),
	); err != nil {
		return fmt.Errorf("save cart: %w", err)
	}

	// The merge deletes a line outright when a clear covers it, so the rows
	// that are gone from the map have to go from the table too. The cart is
	// tens of lines, so replacing the set is cheaper to write and to reason
	// about than tracking which ones changed.
	keep := make([]string, 0, len(c.Lines))
	for sku := range c.Lines {
		keep = append(keep, sku)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM cart_lines WHERE user_id = $1 AND NOT (sku = ANY($2))`,
		c.UserID, keep); err != nil {
		return fmt.Errorf("prune lines: %w", err)
	}

	for _, l := range c.Lines {
		deltas := l.Deltas
		if deltas == nil {
			deltas = map[string]basket.Delta{}
		}
		raw, err := json.Marshal(deltas)
		if err != nil {
			return fmt.Errorf("encode deltas for %s: %w", l.SKU, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO cart_lines (user_id, sku, base, base_seq, base_dev, base_act, deltas)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)
			 ON CONFLICT (user_id, sku) DO UPDATE
			    SET base = EXCLUDED.base, base_seq = EXCLUDED.base_seq,
			        base_dev = EXCLUDED.base_dev, base_act = EXCLUDED.base_act,
			        deltas = EXCLUDED.deltas`,
			c.UserID, l.SKU, l.Base, int64(l.BaseAt.Seq), l.BaseAt.DeviceID, l.BaseAt.ActionID, raw,
		); err != nil {
			return fmt.Errorf("save line %s: %w", l.SKU, err)
		}
	}

	for _, id := range applied {
		// DO NOTHING covers the race this lock does not: the same action id
		// arriving for the same user on a connection that was waiting.
		if _, err := tx.Exec(ctx,
			`INSERT INTO applied_actions (user_id, action_id) VALUES ($1,$2)
			 ON CONFLICT (user_id, action_id) DO NOTHING`, c.UserID, id); err != nil {
			return fmt.Errorf("record action %s: %w", id, err)
		}
	}
	return nil
}

// SweepApplied drops idempotency records past the retention window and
// reports how many went.
//
// The window is the longest a device may be offline and still have its replay
// recognised. Past it, a resend is applied a second time, so it is not a
// tuning knob: shortening it to save space makes the service wrong for
// somebody. 30 days is the figure the service runs with, and it is set by the
// caller so that the number lives in configuration next to the other
// operational choices rather than buried here.
func (s *Store) SweepApplied(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM applied_actions WHERE applied_at < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int(olderThan.Seconds())))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
