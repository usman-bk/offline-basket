package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/usmanvirk/offline-basket/internal/basket"
	"github.com/usmanvirk/offline-basket/internal/store/memory"
)

func newServer(t *testing.T) (*http.ServeMux, *memory.Store) {
	t.Helper()
	st := memory.New()
	h := New(st, HeaderAuth, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return h.Routes(), st
}

func post(t *testing.T, mux *http.ServeMux, user string, actions []basket.Action) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(syncRequest{Actions: actions})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/cart/sync", bytes.NewReader(body))
	if user != "" {
		req.Header.Set("X-User-Id", user)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) basket.SyncResult {
	t.Helper()
	var out basket.SyncResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return out
}

func add(id, dev string, seq uint64, sku string, qty int) basket.Action {
	return basket.Action{ID: id, DeviceID: dev, Seq: seq, Kind: basket.KindAdd, SKU: sku, Qty: qty}
}

func TestSyncAppliesAndReturnsTheWholeCart(t *testing.T) {
	mux, _ := newServer(t)
	rec := post(t, mux, "u1", []basket.Action{
		add("a1", "d1", 1, "S1", 2),
		add("a2", "d1", 2, "S2", 1),
	})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	out := decode(t, rec)
	if len(out.Cart) != 2 {
		t.Fatalf("cart has %d lines, want 2: %v", len(out.Cart), out.Cart)
	}
	if out.Version != 2 {
		t.Fatalf("version %d, want 2", out.Version)
	}
	if c := out.Counts(); c[basket.Applied] != 2 {
		t.Fatalf("applied %d, want 2", c[basket.Applied])
	}
}

// TestReplayIsASuccessNotAConflict is the behaviour the client depends on. A
// phone that sent a batch and never saw the response sends it again. Answering
// 409 would be true and useless: the client cannot tell from it whether the
// cart holds the items or not.
func TestReplayIsASuccessNotAConflict(t *testing.T) {
	mux, _ := newServer(t)
	acts := []basket.Action{add("a1", "d1", 1, "S1", 2), add("a2", "d1", 2, "S1", 3)}

	first := decode(t, post(t, mux, "u1", acts))
	second := decode(t, post(t, mux, "u1", acts))

	if first.Cart[0].Qty != 5 {
		t.Fatalf("first sync settled at %d, want 5", first.Cart[0].Qty)
	}
	if second.Cart[0].Qty != 5 {
		t.Fatalf("replay double counted: %d, want 5", second.Cart[0].Qty)
	}
	if c := second.Counts(); c[basket.Duplicate] != 2 {
		t.Fatalf("replay reported %v, want 2 duplicates", c)
	}
}

// TestEmptyBatchIsAFetch covers the cold start. The app opens, has nothing
// queued, and wants the cart.
func TestEmptyBatchIsAFetch(t *testing.T) {
	mux, _ := newServer(t)
	post(t, mux, "u1", []basket.Action{add("a1", "d1", 1, "S1", 4)})

	out := decode(t, post(t, mux, "u1", nil))
	if len(out.Cart) != 1 || out.Cart[0].Qty != 4 {
		t.Fatalf("empty batch did not return the cart: %v", out.Cart)
	}
	if len(out.Results) != 0 {
		t.Fatalf("results for an empty batch: %v", out.Results)
	}
}

func TestCartsAreSeparatePerUser(t *testing.T) {
	mux, _ := newServer(t)
	post(t, mux, "u1", []basket.Action{add("a1", "d1", 1, "S1", 2)})
	out := decode(t, post(t, mux, "u2", nil))
	if len(out.Cart) != 0 {
		t.Fatalf("u2 can see u1's cart: %v", out.Cart)
	}
}

// TestSameActionIdForTwoUsers pins that the dedupe record is per user. Action
// ids come off devices and nothing makes them unique across the estate, so a
// collision must not let one person's action be swallowed as another's
// duplicate.
func TestSameActionIdForTwoUsers(t *testing.T) {
	mux, _ := newServer(t)
	post(t, mux, "u1", []basket.Action{add("same-id", "d1", 1, "S1", 2)})
	out := decode(t, post(t, mux, "u2", []basket.Action{add("same-id", "d2", 1, "S9", 7)}))
	if len(out.Cart) != 1 || out.Cart[0].SKU != "S9" || out.Cart[0].Qty != 7 {
		t.Fatalf("u2's action was treated as a duplicate of u1's: %v", out.Cart)
	}
}

func TestOversizeBatchIsRefusedWhole(t *testing.T) {
	mux, _ := newServer(t)
	var acts []basket.Action
	for i := 0; i <= basket.MaxBatch; i++ {
		acts = append(acts, add(fmt.Sprintf("a%d", i), "d1", uint64(i+1), "S1", 1))
	}
	rec := post(t, mux, "u1", acts)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", rec.Code)
	}
	// Nothing may have been applied, or the client cannot safely resend.
	out := decode(t, post(t, mux, "u1", nil))
	if len(out.Cart) != 0 {
		t.Fatalf("an oversize batch applied part of itself: %v", out.Cart)
	}
}

func TestExactlyMaxBatchIsAccepted(t *testing.T) {
	mux, _ := newServer(t)
	var acts []basket.Action
	for i := 0; i < basket.MaxBatch; i++ {
		acts = append(acts, add(fmt.Sprintf("a%d", i), "d1", uint64(i+1), "S1", 1))
	}
	rec := post(t, mux, "u1", acts)
	if rec.Code != 200 {
		t.Fatalf("status %d at exactly the limit: %s", rec.Code, rec.Body)
	}
	if q := decode(t, rec).Cart[0].Qty; q != basket.MaxQty {
		t.Fatalf("200 adds of 1 settled at %d, want the ceiling %d", q, basket.MaxQty)
	}
}

// TestBadActionDoesNotStrandTheGoodOnes is the reason a malformed action is
// reported rather than failing the batch. A client bug in one action would
// otherwise block every action behind it, permanently, with no way out.
func TestBadActionDoesNotStrandTheGoodOnes(t *testing.T) {
	mux, _ := newServer(t)
	out := decode(t, post(t, mux, "u1", []basket.Action{
		add("a1", "d1", 1, "S1", 2),
		{ID: "bad", DeviceID: "d1", Seq: 2, Kind: basket.KindAdd}, // no sku
		add("a3", "d1", 3, "S2", 1),
	}))
	c := out.Counts()
	if c[basket.Applied] != 2 || c[basket.Rejected] != 1 {
		t.Fatalf("outcomes %v, want 2 applied and 1 rejected", c)
	}
	if len(out.Cart) != 2 {
		t.Fatalf("good actions did not land: %v", out.Cart)
	}
	for _, r := range out.Results {
		if r.ID == "bad" && r.Error == "" {
			t.Fatal("rejected action came back with no reason")
		}
	}
}

func TestUnauthenticated(t *testing.T) {
	mux, _ := newServer(t)
	if rec := post(t, mux, "", nil); rec.Code != 401 {
		t.Fatalf("status %d, want 401", rec.Code)
	}
}

func TestMalformedBody(t *testing.T) {
	mux, _ := newServer(t)
	for _, body := range []string{`{`, `{"actions": "no"}`, `{"nope": 1}`, ``} {
		req := httptest.NewRequest("POST", "/v1/cart/sync", strings.NewReader(body))
		req.Header.Set("X-User-Id", "u1")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Fatalf("body %q gave status %d, want 400", body, rec.Code)
		}
	}
}

func TestWrongMethodAndPath(t *testing.T) {
	mux, _ := newServer(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/cart/sync"},
		{"POST", "/v1/cart"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code == 200 {
			t.Fatalf("%s %s answered 200", tc.method, tc.path)
		}
	}
}

func TestHealthz(t *testing.T) {
	mux, _ := newServer(t)
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("healthz status %d", rec.Code)
	}
}

// TestConcurrentSyncsForOneUser is the case two devices create every time
// somebody has a phone and a tablet. Without the lock inside Tx the read and
// the write interleave and one device's actions are lost.
func TestConcurrentSyncsForOneUser(t *testing.T) {
	mux, _ := newServer(t)
	const devices, each = 8, 20

	var wg sync.WaitGroup
	for d := 0; d < devices; d++ {
		wg.Add(1)
		go func(d int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				post(t, mux, "u1", []basket.Action{
					add(fmt.Sprintf("d%d-a%d", d, i), fmt.Sprintf("dev%d", d), uint64(i+1), "S1", 1),
				})
			}
		}(d)
	}
	wg.Wait()

	out := decode(t, post(t, mux, "u1", nil))
	if len(out.Cart) != 1 {
		t.Fatalf("cart has %d lines, want 1", len(out.Cart))
	}
	// Every add is a distinct action id, so all of them count, and the total
	// is above the ceiling, so the line sits at the ceiling. The assertion
	// that matters is that nothing was lost on the way.
	if got := out.Cart[0].Qty; got != basket.MaxQty {
		t.Fatalf("%d adds settled at %d, want the ceiling %d: writes were lost",
			devices*each, got, basket.MaxQty)
	}
}
