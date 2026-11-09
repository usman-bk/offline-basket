-- The cart row exists so there is something to take a lock on. Every sync for
-- a user serialises on it, which is what makes the read, the merge and the
-- write one step rather than three with a gap in the middle.
CREATE TABLE IF NOT EXISTS carts (
    user_id     text        PRIMARY KEY,
    -- The cart wide tombstone from a clear, stored as its three parts rather
    -- than as a composite type so the ordering can be read in SQL.
    cleared_seq bigint      NOT NULL DEFAULT 0,
    cleared_dev text        NOT NULL DEFAULT '',
    cleared_act text        NOT NULL DEFAULT '',
    version     bigint      NOT NULL DEFAULT 0,
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS cart_lines (
    user_id  text   NOT NULL REFERENCES carts(user_id) ON DELETE CASCADE,
    sku      text   NOT NULL,
    base     int    NOT NULL,
    base_seq bigint NOT NULL,
    base_dev text   NOT NULL,
    base_act text   NOT NULL,
    -- The surviving relative writes, keyed by the action id that made each
    -- one. jsonb and not a child table: they are only ever read and written
    -- as a whole alongside their line, never queried across lines, and a
    -- child table would add a join and a second lock for nothing. The set is
    -- bounded because an absolute write prunes everything below it.
    deltas   jsonb  NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (user_id, sku)
);

-- The idempotency record. The primary key is the whole mechanism: the second
-- arrival of an action id is a conflict, not a second row.
--
-- (user_id, action_id) and not action_id alone. Action ids are generated on
-- devices and nothing makes them unique across the estate, so a global key
-- would let one customer's action be swallowed as another customer's
-- duplicate.
CREATE TABLE IF NOT EXISTS applied_actions (
    user_id    text        NOT NULL,
    action_id  text        NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, action_id)
);

-- Supports the retention sweep, which deletes by age. Without it the sweep is
-- a sequential scan of the largest table in the schema.
CREATE INDEX IF NOT EXISTS applied_actions_applied_at_idx
    ON applied_actions (applied_at);
