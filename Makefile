.PHONY: run test fuzz

DATABASE_URL ?= postgres://localhost:5432/basket?sslmode=disable
export DATABASE_URL

run:
	createdb basket || true
	go run ./cmd/basketd -insecure-header-auth

test:
	go test -race ./...

# The committed run is 4,000 batches so CI stays under a second. This is the
# one to reach for after touching the merge, and it wants a seed you can quote
# back when it finds something.
fuzz:
	BASKET_FUZZ_TRIALS=40000 go test -run RandomBatches ./internal/basket/
