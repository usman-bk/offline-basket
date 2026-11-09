// Package httpapi puts the sync endpoint on the network.
//
// There is one endpoint that matters, POST /v1/cart/sync, and it is the only
// one the app needs for a cold start as well as for a reconnection: sending an
// empty batch returns the current cart. A separate GET would be a second code
// path with the same answer, and a second thing to keep in step.
package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/usmanvirk/offline-basket/internal/basket"
)

type syncRequest struct {
	Actions []basket.Action `json:"actions"`
}

type errorBody struct {
	Error string `json:"error"`
	Limit int    `json:"limit,omitempty"`
}

// Auth resolves a request to a user id. A real deployment verifies a token;
// the point here is that the user id is never taken from the body, where a
// client could name somebody else's cart.
type Auth func(*http.Request) (string, bool)

// HeaderAuth trusts an X-User-Id header. It is for local runs only, and the
// server refuses to start with it unless the operator asks for it by name.
func HeaderAuth(r *http.Request) (string, bool) {
	u := strings.TrimSpace(r.Header.Get("X-User-Id"))
	return u, u != ""
}

type Handler struct {
	store   basket.Store
	auth    Auth
	log     *slog.Logger
	timeout time.Duration
}

func New(s basket.Store, a Auth, log *slog.Logger) *Handler {
	return &Handler{store: s, auth: a, log: log, timeout: 5 * time.Second}
}

func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/cart/sync", h.sync)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *Handler) sync(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.auth(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, errorBody{Error: "no user"})
		return
	}

	// The body is bounded before it is parsed. MaxBatch actions of a plausible
	// size is well inside this, and a client that sends more is not going to
	// be allowed to allocate it first.
	var req syncRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "malformed body"})
		return
	}

	ctx, cancel := contextWithTimeout(r, h.timeout)
	defer cancel()

	res, err := basket.Sync(ctx, h.store, userID, req.Actions)
	if err != nil {
		var tooBig basket.ErrTooBig
		if errors.As(err, &tooBig) {
			// 413 rather than 400: the batch is well formed and the client
			// fixes it by splitting, not by correcting anything.
			writeJSON(w, http.StatusRequestEntityTooLarge,
				errorBody{Error: err.Error(), Limit: basket.MaxBatch})
			return
		}
		h.log.Error("sync failed", "user", userID, "actions", len(req.Actions), "err", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "sync failed"})
		return
	}

	counts := res.Counts()
	h.log.Info("sync",
		"user", userID,
		"sent", len(req.Actions),
		"applied", counts[basket.Applied],
		"duplicate", counts[basket.Duplicate],
		"superseded", counts[basket.Superseded],
		"rejected", counts[basket.Rejected],
		"version", res.Version)

	writeJSON(w, http.StatusOK, res)
}
