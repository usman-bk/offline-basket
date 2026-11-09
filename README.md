# offline-basket

_Built with AI assistance._

A cart service for a native app that keeps taking taps after the signal has
gone, and for the two devices one person uses, which come back disagreeing
about what is in the basket.

The server settles it. A device sends the queue it recorded while it was
offline, up to 200 actions, and gets back the whole reconciled cart in one
response. Sending the same queue again changes nothing. Sending it in a
different order changes nothing.

Go 1.23, Postgres, no other runtime dependency.

```
internal/basket/stamp.go      the order two writes are compared in
internal/basket/cart.go       the merge
internal/basket/sync.go       the batch, the dedupe, the outcome per action
internal/store/postgres/      the lock, the schema, the jsonb column
internal/store/memory/        the same contract, in process
internal/store/storetest/     the contract, run against both
internal/httpapi/             one endpoint
cmd/basketd/                  the binary
```

## The problem, stated once

A phone on a train keeps working. The customer adds two things, removes one,
and the app has no way to tell the server about any of it. When the signal
comes back the app has a queue, and three things are true at once:

- It does not know whether the last batch it sent arrived. The response may
  have been lost rather than the request.
- Another device belonging to the same person has been changing the same cart.
- The order the server sees any of this in is decided by the network.

So the merge has to be idempotent and commutative, and the dedupe has to
outlive the request. Those are not nice properties to have, they are the
specification.

## Quantities are the hard part, and last write wins gets them wrong

The obvious rule is last write wins per line item. It is correct for setting a
quantity and for removing something. It is wrong for adding.

Two phones each add one of the same item while both are offline. Under last
write wins the cart ends up holding one, because the later write replaces the
earlier instead of counting it. The customer chose two things and has one. The
opposite rule, a plain counter, breaks the other way: a replayed batch adds the
item twice.

A line therefore holds both kinds of write.

| | |
|---|---|
| `Base`, `BaseAt` | the quantity left by the most recent absolute write, chosen by stamp |
| `Deltas` | the increments that happened after it, in a set keyed by the client's action id |

`Qty` is the base plus the sum of the set. Summing a set is order independent,
and keying it by action id makes a replay a no-op rather than a double count,
so both properties the service needs fall out of one structure. An absolute
write prunes every delta below it, which is what stops the set growing.

## Ordering, and why not timestamps

Ordering by the time the phone recorded the action does not work. A device
that has been in a tunnel comes back with a drifted clock, a user can set the
clock by hand, and two phones in one pocket disagree by seconds. The losing
write would be chosen by whichever device had the worse clock, which is not a
rule anybody can explain to a customer who lost an item.

`Seq` is a Lamport clock instead. Each device increments a counter per action
and raises it past the `version` the server returns on every sync. Two devices
that have both synced produce comparable stamps, and a device that has never
synced only ever loses to one that has.

The comparison is `Seq`, then `DeviceID`, then `ActionID`:

    Seq      the Lamport clock
    DeviceID arbitrary, deterministic, same answer on every replica
    ActionID the only field unique by construction

The third one is not decoration. `Seq` and `DeviceID` are unique only if every
device increments its counter for every action, which is a promise made by
client code the server does not control. A device that ships a bug and reuses
a sequence number produces two writes where neither is after the other, and the
merge quietly becomes order dependent: the same batch settles one way on the
first attempt and another way on the retry. `ActionID` is the idempotency key
the protocol already carries, so including it makes the order total.

The randomised test found that. It also found that an earlier version of the
clear was not commutative, because it emptied the map rather than comparing
each line against the clear stamp, so a shuffle that put an add in front of a
clear lost the item. Both bugs are in the history.

## Removals leave a tombstone

A removed line stays in the cart at quantity zero, holding the stamp of the
removal. Without it, an add from a device that had not yet heard about the
removal brings the item back.

A tombstone is not needed once every device has synced past it, and no server
knows when that is, so they are dropped on age instead. The window is the
caller's to set, because a device offline for longer than the window is also
carrying prices that are no longer true, and that is a product decision rather
than a storage one.

A clear is the same idea at cart scope. It has to beat writes for SKUs the
server has not seen yet, so it is recorded on the cart rather than by
tombstoning the lines that happen to exist.

## What the client gets back

The settled cart, entire, as `{"sku": ..., "qty": ...}` and nothing else.

A patch would be smaller and it would oblige the client to hold a correct copy
to apply it to, which is the one thing a client that has been offline for a
week cannot be assumed to have. The internal line carries a base, a stamp and a
set of deltas, and none of that goes on the wire: it would put the merge rule
in the app, make every change to the bookkeeping a breaking change for two app
releases nobody can force to update, and cost several times the bytes on the
request that happens the moment a phone comes back onto a signal.

Every action also comes back with an outcome, so the client knows exactly what
to drop from its queue.

| Outcome | Meaning | Client |
|---|---|---|
| `applied` | new, and the cart changed | drop it |
| `duplicate` | this id had already landed | drop it. This is the normal answer to a retry |
| `superseded` | new, and lost to a later write | drop it |
| `rejected` | could not be read, which means a client bug | keep it and report it |

`duplicate` is a success and not a conflict. Answering 409 would be true and
useless: the client cannot tell from it whether the items are in the cart.

A malformed action is reported rather than failing the batch. The queue was
recorded over hours on a device that may have shipped a bug, and refusing the
whole batch would strand every good action behind the bad one with no way for
the client to make progress.

## Two devices syncing at once

`Tx` takes `SELECT FOR UPDATE` on the cart row before reading anything, and
commits the cart and the action ids together.

Without the lock, two transactions read the same cart, merge separately, and
the second write discards the first one's actions while still recording their
ids as applied. That is the worst outcome available: the items are gone and
the client has been told they landed. Writing the cart and the ids in separate
transactions fails the same way from the other end.

Serializable isolation would also be correct, and would mean handling 40001
retries for a conflict that is routine rather than rare. The explicit lock puts
the contention where it happens, and the critical section is a few hundred
microseconds of in memory merging.

## Running the service

    createdb basket
    DATABASE_URL="postgres://localhost:5432/basket?sslmode=disable" \
      go run ./cmd/basketd -insecure-header-auth

The schema is applied on start. It is one file and it is idempotent; a service
with one table group and no history does not need a migration tool.

There is no token verification in this repository, so the only authentication
it has is the kind that must never face the internet. The server refuses to
start unless that is asked for by name.

Two adds:

    curl -s -X POST localhost:8080/v1/cart/sync -H 'X-User-Id: demo' -d '{"actions":[
      {"id":"a1","device_id":"phone","seq":1,"kind":"add_item","sku":"IPHONE-12-B","qty":1},
      {"id":"a2","device_id":"phone","seq":2,"kind":"add_item","sku":"PIXEL-7-A","qty":2}]}'

    {"cart":[{"sku":"IPHONE-12-B","qty":1},{"sku":"PIXEL-7-A","qty":2}],
     "version":2,"results":[{"id":"a1","outcome":"applied"},{"id":"a2","outcome":"applied"}]}

The same request again:

    {"cart":[{"sku":"IPHONE-12-B","qty":1},{"sku":"PIXEL-7-A","qty":2}],
     "version":2,"results":[{"id":"a1","outcome":"duplicate"},{"id":"a2","outcome":"duplicate"}]}

A second device adding the same item, with no knowledge of the first:

    curl -s -X POST localhost:8080/v1/cart/sync -H 'X-User-Id: demo' -d '{"actions":[
      {"id":"b1","device_id":"tablet","seq":2,"kind":"add_item","sku":"IPHONE-12-B","qty":1}]}'

    {"cart":[{"sku":"IPHONE-12-B","qty":2},...

A stale add arriving behind a removal:

    curl -s -X POST localhost:8080/v1/cart/sync -H 'X-User-Id: demo' -d '{"actions":[
      {"id":"c1","device_id":"phone","seq":9,"kind":"remove_item","sku":"PIXEL-7-A"},
      {"id":"c2","device_id":"old","seq":1,"kind":"add_item","sku":"PIXEL-7-A","qty":5}]}'

    {"cart":[{"sku":"IPHONE-12-B","qty":2}],"version":9,
     "results":[{"id":"c1","outcome":"applied"},{"id":"c2","outcome":"superseded"}]}

An empty batch returns the cart, so a cold start and a reconnection are the
same request. A separate GET would be a second code path with the same answer.

## Testing

    go test ./...                      56 cases, no database needed
    DATABASE_URL=... go test ./...     73, adding the Postgres suite

The randomised search is fixed at 4,000 batches so CI takes a second and a
failure reproduces. To search harder:

    BASKET_FUZZ_SEED=12345 BASKET_FUZZ_TRIALS=40000 go test ./internal/basket/

That was run across six seeds, 480,000 batches, each shuffled six ways.

`internal/store/storetest` is the contract both stores are held to. The memory
store is what the handler tests run against and Postgres is what production
runs on, so a difference between them would otherwise appear for the first
time in production. The Postgres suite needs a real server: the things worth
testing in it are the lock, the rollback and the jsonb round trip, and a mock
of Postgres would be a mock of the behaviour under test.

## What this does not do

No pricing, no stock check, no checkout. A cart that is merged is not a cart
that can be paid for, and the price of a graded one of a kind item at the
moment of checkout is a different problem with a different owner.

No per item concurrency beyond the line. Two devices editing the same line
settle by the rules above and the customer is not told it happened.

The retention window is the honest limit. A device offline for longer than it
can replay a batch that is applied a second time.
