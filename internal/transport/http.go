// Package transport exposes separate signed webhook ingress and authenticated local IPC.
package transport

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	v1 "github.com/wenext-limited/github-botd/api/v1"
	"github.com/wenext-limited/github-botd/internal/config"
	"github.com/wenext-limited/github-botd/internal/gateway"
	"github.com/wenext-limited/github-botd/internal/store"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func problem(w http.ResponseWriter, status int, code, msg string) {
	jsonResponse(w, status, v1.Error{Code: code, Message: msg})
}
func failure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, gateway.ErrForbidden):
		problem(w, 403, "forbidden", "capability or repository access denied")
	case errors.Is(err, store.ErrNotFound):
		problem(w, 404, "not_found", "resource not found")
	case errors.Is(err, store.ErrLease):
		problem(w, 409, "lease_conflict", "lease is invalid or expired")
	case errors.Is(err, store.ErrConflict):
		problem(w, 409, "conflict", "idempotency key or resource state conflict")
	case errors.Is(err, store.ErrCapacity):
		problem(w, 503, "capacity", "durable capacity reached; operator intervention required")
	default:
		problem(w, 400, "invalid_request", err.Error())
	}
}
func decode(w http.ResponseWriter, r *http.Request, max int64, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	defer r.Body.Close()
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return errors.New("invalid or oversized JSON body")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON content")
	}
	return nil
}
func (g handler) auth(w http.ResponseWriter, r *http.Request) (string, bool) {
	// Browsers cannot use local APIs as ambient authority or through DNS rebinding.
	if r.Header.Get("Origin") != "" {
		problem(w, 403, "origin_denied", "browser origins are not permitted")
		return "", false
	}
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") || len(header) > 8192 {
		problem(w, 401, "unauthorized", "client bearer required")
		return "", false
	}
	token := strings.TrimPrefix(header, "Bearer ")
	candidate := sha256.Sum256([]byte(token))
	id := ""
	for key, p := range g.g.Config.Clients {
		expected := sha256.Sum256([]byte(p.Token))
		if subtle.ConstantTimeCompare(expected[:], candidate[:]) == 1 {
			id = key
		}
	}
	if id == "" {
		problem(w, 401, "unauthorized", "invalid client bearer")
		return "", false
	}
	return id, true
}

type handler struct{ g *gateway.Gateway }

func (g handler) capability(w http.ResponseWriter, id, cap string) bool {
	if !config.Contains(g.g.Config.Clients[id].Capabilities, cap) {
		failure(w, gateway.ErrForbidden)
		return false
	}
	return true
}
func IPC(g *gateway.Gateway) http.Handler {
	h := handler{g: g}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, 200, v1.Health{Version: v1.Version, Status: "ok", DryRun: g.Config.DryRun})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		status := 200
		health := "ready"
		checks := map[string]string{"store": "ok", "github": "configured_not_live_verified"}
		if g.Store.Health() != nil {
			status = 503
			health = "not_ready"
			checks["store"] = "unavailable"
		}
		if g.Config.DryRun {
			checks["github"] = "dry_run"
		}
		jsonResponse(w, status, v1.Health{Version: v1.Version, Status: health, DryRun: g.Config.DryRun, Checks: checks})
	})
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		id, ok := h.auth(w, r)
		if !ok {
			return
		}
		p := g.Config.Clients[id]
		jsonResponse(w, 200, v1.ClientInfo{AllRepositories: p.AllRepositories, ID: id, Protocol: v1.Version, Repositories: p.Repositories, Capabilities: p.Capabilities})
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		id, ok := h.auth(w, r)
		if !ok {
			return
		}
		stats, err := g.Store.ClientStats(id)
		if err != nil {
			problem(w, 503, "store_unavailable", "cannot read durable state")
			return
		}
		jsonResponse(w, 200, stats)
	})
	mux.HandleFunc("GET /v1/deadletters", func(w http.ResponseWriter, r *http.Request) {
		id, ok := h.auth(w, r)
		if !ok || !h.capability(w, id, "events.replay") {
			return
		}
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 100 {
				problem(w, 400, "invalid_request", "limit must be 1..100")
				return
			}
			limit = n
		}
		rows, err := g.Store.ListDeadLetters(id, g.Config.Clients[id].Repositories, limit)
		if err != nil {
			failure(w, err)
			return
		}
		if rows == nil {
			rows = []v1.DeadLetter{}
		}
		jsonResponse(w, 200, map[string]any{"deliveries": rows})
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		id, ok := h.auth(w, r)
		if !ok || !h.capability(w, id, "events.read") {
			return
		}
		limit, wait := 1, 25
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 100 {
				problem(w, 400, "invalid_request", "limit must be 1..100")
				return
			}
			limit = n
		}
		if v := r.URL.Query().Get("wait"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 || n > 30 {
				problem(w, 400, "invalid_request", "wait must be 0..30 seconds")
				return
			}
			wait = n
		}
		until := time.Now().Add(time.Duration(wait) * time.Second)
		for {
			if r.Context().Err() != nil {
				jsonResponse(w, 200, map[string]any{"deliveries": []v1.Delivery{}})
				return
			}
			deliveries, err := g.Store.ClaimAllowed(id, limit, time.Duration(g.Config.Limits.LeaseSeconds)*time.Second, g.Config.Clients[id].Repositories)
			if err != nil {
				problem(w, 503, "store_unavailable", "cannot claim events")
				return
			}
			if len(deliveries) > 0 || !time.Now().Before(until) {
				if deliveries == nil {
					deliveries = []v1.Delivery{}
				}
				jsonResponse(w, 200, map[string]any{"deliveries": deliveries})
				return
			}
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-r.Context().Done():
				timer.Stop()
				jsonResponse(w, 200, map[string]any{"deliveries": []v1.Delivery{}})
				return
			case <-timer.C:
			}
		}
	})
	mux.HandleFunc("POST /v1/events/{id}/ack", func(w http.ResponseWriter, r *http.Request) {
		id, ok := h.auth(w, r)
		if !ok || !h.capability(w, id, "events.read") {
			return
		}
		var a v1.AckRequest
		if err := decode(w, r, 4096, &a); err != nil {
			failure(w, err)
			return
		}
		if len(a.Error) > 1000 {
			problem(w, 400, "invalid_request", "error text too long")
			return
		}
		if err := g.Store.Ack(id, r.PathValue("id"), a.LeaseToken, a.Retry, a.Error); err != nil {
			failure(w, err)
			return
		}
		status := "acked"
		if a.Retry {
			status = "retrying"
		}
		jsonResponse(w, 200, map[string]string{"status": status})
	})
	mux.HandleFunc("POST /v1/events/{id}/replay", func(w http.ResponseWriter, r *http.Request) {
		id, ok := h.auth(w, r)
		if !ok || !h.capability(w, id, "events.replay") {
			return
		}
		event, eventErr := g.Store.DeliveryEvent(id, r.PathValue("id"))
		if eventErr != nil {
			failure(w, eventErr)
			return
		}
		if !config.Contains(g.Config.Clients[id].Repositories, event.Repository) {
			failure(w, gateway.ErrForbidden)
			return
		}
		if err := g.Store.Replay(id, r.PathValue("id")); err != nil {
			failure(w, err)
			return
		}
		jsonResponse(w, 200, map[string]string{"status": "requeued"})
	})
	mux.HandleFunc("POST /v1/merge-check", func(w http.ResponseWriter, r *http.Request) {
		id, ok := h.auth(w, r)
		if !ok {
			return
		}
		var a v1.Action
		if err := decode(w, r, g.Config.Limits.MaxBodyBytes, &a); err != nil {
			failure(w, err)
			return
		}
		if err := g.CheckMerge(r.Context(), id, a); err != nil {
			failure(w, err)
			return
		}
		jsonResponse(w, 200, map[string]bool{"ready": true})
	})
	mux.HandleFunc("POST /v1/actions", func(w http.ResponseWriter, r *http.Request) {
		id, ok := h.auth(w, r)
		if !ok {
			return
		}
		var a v1.Action
		if err := decode(w, r, g.Config.Limits.MaxBodyBytes, &a); err != nil {
			failure(w, err)
			return
		}
		receipt, err := g.Submit(id, a)
		if err != nil {
			failure(w, err)
			return
		}
		jsonResponse(w, 202, receipt)
	})
	mux.HandleFunc("GET /v1/actions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := h.auth(w, r)
		if !ok {
			return
		}
		action, actionErr := g.Store.Action(id, r.PathValue("id"))
		if actionErr != nil {
			failure(w, actionErr)
			return
		}
		if _, err := g.ResolveRepository(id, action.Repository); err != nil {
			failure(w, gateway.ErrForbidden)
			return
		}
		receipt, err := g.Store.Receipt(id, r.PathValue("id"))
		if err != nil {
			failure(w, err)
			return
		}
		jsonResponse(w, 200, receipt)
	})
	mux.HandleFunc("POST /v1/context", func(w http.ResponseWriter, r *http.Request) {
		id, ok := h.auth(w, r)
		if !ok {
			return
		}
		var a v1.ContextRequest
		if err := decode(w, r, 4096, &a); err != nil {
			failure(w, err)
			return
		}
		result, err := g.Context(r.Context(), id, a)
		if err != nil {
			failure(w, err)
			return
		}
		jsonResponse(w, 200, result)
	})
	return harden(mux)
}
func Webhook(g *gateway.Gateway) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hooks/{source}", func(w http.ResponseWriter, r *http.Request) {
		source := r.PathValue("source")
		s, ok := g.Config.Webhook.Sources[source]
		if !ok {
			problem(w, 404, "not_found", "unknown webhook source")
			return
		}
		if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
			problem(w, 415, "content_type", "application/json required")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, g.Config.Limits.MaxBodyBytes)
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			problem(w, 413, "too_large", "webhook body exceeds limit")
			return
		}
		signature := r.Header.Get("X-Hub-Signature-256")
		if !VerifySignature(s.Secret, signature, body) {
			problem(w, 401, "invalid_signature", "webhook signature is invalid")
			return
		}
		deliveryID := r.Header.Get("X-GitHub-Delivery")
		eventType := r.Header.Get("X-GitHub-Event")
		if deliveryID == "" || len(deliveryID) > 255 || strings.ContainsAny(deliveryID, "\r\n\x00") {
			problem(w, 400, "invalid_delivery", "delivery ID is required")
			return
		}
		if eventType == "ping" {
			jsonResponse(w, 200, map[string]string{"status": "pong"})
			return
		}
		fresh, err := g.Ingest(source, eventType, deliveryID, body)
		if err != nil {
			failure(w, err)
			return
		}
		jsonResponse(w, 202, map[string]any{"accepted": true, "new": fresh})
	})
	return harden(mux)
}
func VerifySignature(secret, signature string, body []byte) bool {
	if !strings.HasPrefix(signature, "sha256=") || len(signature) != 71 {
		return false
	}
	digest, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(digest, mac.Sum(nil))
}
func harden(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}
