// Package store provides a transactional, fsync-backed inbox and action outbox.
// A successful method return means the transaction committed. Running outbound
// actions are quarantined as uncertain after a restart, never automatically retried.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	v1 "github.com/wenext-limited/github-botd/api/v1"
	bolt "go.etcd.io/bbolt"
)

var (
	ErrNotFound = errors.New("not found")
	ErrLease    = errors.New("invalid, expired, or foreign lease")
	ErrConflict = errors.New("idempotency key or state conflict")
	ErrCapacity = errors.New("durable storage capacity reached")
)

const MaxAttempts = 8

type Options struct {
	MaxAttempts int
	MaxPending  int
}

var bucketNames = []string{"events", "dedupe", "deliveries", "actions", "action_keys", "meta"}

type Store struct {
	db             *bolt.DB
	now            func() time.Time
	options        atomic.Pointer[Options]
	closeDirectory func() error
}
type delivery struct {
	ID        string
	Client    string
	EventID   string
	State     string
	Token     string
	Expires   time.Time
	Available time.Time
	Attempts  int
	Error     string
}
type PendingAction struct {
	Client    string
	Action    v1.Action
	Receipt   v1.Receipt
	NotBefore time.Time
}

func Open(dir string) (*Store, error) { return OpenWithOptions(dir, Options{}) }
func normalizeOptions(options Options) (Options, error) {
	if options.MaxAttempts == 0 {
		options.MaxAttempts = MaxAttempts
	}
	if options.MaxPending == 0 {
		options.MaxPending = 100000
	}
	if options.MaxAttempts < 1 || options.MaxAttempts > 30 || options.MaxPending < 1 {
		return Options{}, fmt.Errorf("invalid store options")
	}
	return options, nil
}
func OpenWithOptions(dir string, options Options) (*Store, error) {
	options, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	db, closeDirectory, err := openDatabase(dir)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, now: time.Now, closeDirectory: closeDirectory}
	s.options.Store(&options)
	err = db.Update(func(tx *bolt.Tx) error {
		if m := tx.Bucket([]byte("meta")); m != nil {
			if string(m.Get([]byte("schema"))) != "1" {
				return fmt.Errorf("unsupported database schema")
			}
		} else {
			if err := tx.ForEach(func(_ []byte, _ *bolt.Bucket) error { return fmt.Errorf("unrecognized database") }); err != nil {
				return err
			}
		}
		for _, name := range bucketNames {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		if err := b(tx, "meta").Put([]byte("schema"), []byte("1")); err != nil {
			return err
		}
		var recovering []PendingAction
		if err := b(tx, "actions").ForEach(func(_, v []byte) error {
			var a PendingAction
			if err := json.Unmarshal(v, &a); err != nil {
				return err
			}
			if a.Receipt.State == "running" {
				recovering = append(recovering, a)
			}
			return nil
		}); err != nil {
			return err
		}
		for _, a := range recovering {
			a.Receipt.State = "uncertain"
			a.Receipt.Error = "process stopped while action was running; reconcile with GitHub before submitting a new action"
			a.Receipt.UpdatedAt = stamp(s.now())
			if err := put(tx, "actions", a.Receipt.ID, a); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		closeDirectory()
		return nil, err
	}
	return s, nil
}
func b(tx *bolt.Tx, name string) *bolt.Bucket { return tx.Bucket([]byte(name)) }
func put(tx *bolt.Tx, bucket, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return b(tx, bucket).Put([]byte(key), raw)
}
func get(tx *bolt.Tx, bucket, key string, out any) error {
	raw := b(tx, bucket).Get([]byte(key))
	if raw == nil {
		return ErrNotFound
	}
	return json.Unmarshal(raw, out)
}
func scoped(parts ...string) string { raw, _ := json.Marshal(parts); return string(raw) }
func uid() (string, error) {
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// Ingest deduplicates by source route (including app identity) and delivery ID.
// The complete event and every fanout record commit together. Duplicate retries
// deliberately do not add new recipients; use explicit replay for old deliveries.
func (s *Store) Ingest(source, deliveryID string, event v1.Event, clients []string) (bool, error) {
	if source == "" || deliveryID == "" || event.ID == "" {
		return false, fmt.Errorf("source, delivery ID, and event ID required")
	}
	added := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		options := s.options.Load()
		key := scoped(source, deliveryID)
		if b(tx, "dedupe").Get([]byte(key)) != nil {
			return nil
		}
		if b(tx, "events").Stats().KeyN >= options.MaxPending || b(tx, "deliveries").Stats().KeyN+len(clients) > options.MaxPending {
			return ErrCapacity
		}
		if b(tx, "events").Get([]byte(event.ID)) != nil {
			return ErrConflict
		}
		if err := put(tx, "events", event.ID, event); err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, client := range clients {
			if client == "" {
				return fmt.Errorf("empty client")
			}
			if seen[client] {
				continue
			}
			seen[client] = true
			id, err := uid()
			if err != nil {
				return err
			}
			if err = put(tx, "deliveries", id, delivery{ID: id, Client: client, EventID: event.ID, State: "pending", Available: s.now()}); err != nil {
				return err
			}
		}
		if err := b(tx, "dedupe").Put([]byte(key), []byte(event.ID)); err != nil {
			return err
		}
		added = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return added, nil
}

// Claim never blocks. Callers implement cancellable bounded long polling around
// this operation. Each claim rotates an opaque token and persists its deadline.
func (s *Store) Claim(client string, limit int, lease time.Duration) ([]v1.Delivery, error) {
	return s.claim(client, limit, lease, nil)
}

// ClaimAllowed filters persisted events against current repository grants before
// creating a lease. Unlike Claim, an empty or nil allowlist denies every event.
func (s *Store) ClaimAllowed(client string, limit int, lease time.Duration, allowedRepositories []string) ([]v1.Delivery, error) {
	allowed := make(map[string]bool, len(allowedRepositories))
	for _, repo := range allowedRepositories {
		allowed[repo] = true
	}
	return s.claim(client, limit, lease, allowed)
}
func (s *Store) claim(client string, limit int, lease time.Duration, allowed map[string]bool) ([]v1.Delivery, error) {
	if client == "" || limit < 1 || limit > 100 || lease <= 0 || lease > 24*time.Hour {
		return nil, fmt.Errorf("invalid claim bounds")
	}
	out := []v1.Delivery{}
	err := s.db.Update(func(tx *bolt.Tx) error {
		options := s.options.Load()
		now := s.now()
		var updates []delivery
		err := b(tx, "deliveries").ForEach(func(_, raw []byte) error {
			var d delivery
			if err := json.Unmarshal(raw, &d); err != nil {
				return err
			}
			if d.Client != client || d.State == "acked" || d.State == "dead" || d.Available.After(now) || (d.State == "leased" && d.Expires.After(now)) {
				return nil
			}
			var event v1.Event
			if err := get(tx, "events", d.EventID, &event); err != nil {
				return err
			}
			if allowed != nil && !allowed[event.Repository] {
				return nil
			}
			if d.Attempts >= options.MaxAttempts {
				d.State = "dead"
				d.Token = ""
				d.Error = "maximum delivery attempts exhausted"
				updates = append(updates, d)
				return nil
			}
			if len(out) >= limit {
				return nil
			}
			token, err := uid()
			if err != nil {
				return err
			}
			d.Token = token
			d.State = "leased"
			d.Attempts++
			d.Expires = now.Add(lease)
			out = append(out, v1.Delivery{ID: d.ID, LeaseToken: token, LeaseExpiresAt: stamp(d.Expires), Attempt: d.Attempts, Event: event})
			updates = append(updates, d)
			return nil
		})
		if err != nil {
			return err
		}
		for _, d := range updates {
			if err := put(tx, "deliveries", d.ID, d); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
func (s *Store) Ack(client, id, token string, retry bool, reason string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		var d delivery
		if err := get(tx, "deliveries", id, &d); err != nil {
			return err
		}
		if d.Client != client || token == "" || d.Token != token || d.State != "leased" || !d.Expires.After(s.now()) {
			return ErrLease
		}
		d.Token = ""
		d.Error = reason
		d.State = "acked"
		if retry {
			if d.Attempts >= s.options.Load().MaxAttempts {
				d.State = "dead"
			} else {
				d.State = "pending"
				d.Available = s.now().Add(time.Second * time.Duration(1<<uint(d.Attempts-1)))
			}
		}
		return put(tx, "deliveries", id, d)
	})
}
func (s *Store) Event(client, eventID string) (v1.Event, error) {
	var event v1.Event
	err := s.db.View(func(tx *bolt.Tx) error {
		found := false
		if err := b(tx, "deliveries").ForEach(func(_, raw []byte) error {
			var d delivery
			if err := json.Unmarshal(raw, &d); err != nil {
				return err
			}
			if d.Client == client && d.EventID == eventID {
				found = true
			}
			return nil
		}); err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		return get(tx, "events", eventID, &event)
	})
	return event, err
}
func (s *Store) HasEvent(client, eventID string) bool {
	_, err := s.Event(client, eventID)
	return err == nil
}

// Replay resets exactly one client's dead-letter delivery. Active or acknowledged work cannot be replayed.
func (s *Store) Replay(client, id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		var d delivery
		if err := get(tx, "deliveries", id, &d); err != nil {
			return err
		}
		if d.Client != client {
			return ErrNotFound
		}
		if d.State != "dead" {
			return ErrConflict
		}
		d.State = "pending"
		d.Attempts = 0
		d.Token = ""
		d.Error = ""
		d.Available = s.now()
		d.Expires = time.Time{}
		return put(tx, "deliveries", id, d)
	})
}
func (s *Store) Enqueue(client string, action v1.Action) (v1.Receipt, error) {
	var receipt v1.Receipt
	if client == "" || action.IdempotencyKey == "" {
		return receipt, fmt.Errorf("client and idempotency key required")
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		key := scoped(client, action.IdempotencyKey)
		if id := b(tx, "action_keys").Get([]byte(key)); id != nil {
			var old PendingAction
			if err := get(tx, "actions", string(id), &old); err != nil {
				return err
			}
			a, _ := json.Marshal(old.Action)
			z, _ := json.Marshal(action)
			if string(a) != string(z) {
				return ErrConflict
			}
			receipt = old.Receipt
			return nil
		}
		if b(tx, "actions").Stats().KeyN >= s.options.Load().MaxPending {
			return ErrCapacity
		}
		id, err := uid()
		if err != nil {
			return err
		}
		now := stamp(s.now())
		receipt = v1.Receipt{ID: id, IdempotencyKey: action.IdempotencyKey, State: "pending", CreatedAt: now, UpdatedAt: now}
		if err = put(tx, "actions", id, PendingAction{Client: client, Action: action, Receipt: receipt}); err != nil {
			return err
		}
		return b(tx, "action_keys").Put([]byte(key), []byte(id))
	})
	if err != nil {
		return v1.Receipt{}, err
	}
	return receipt, nil
}
func (s *Store) Receipt(client, id string) (v1.Receipt, error) {
	var a PendingAction
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := get(tx, "actions", id, &a); err != nil {
			return err
		}
		if a.Client != client {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return v1.Receipt{}, err
	}
	return a.Receipt, nil
}
func (s *Store) NextAction() (*PendingAction, error) {
	var selected *PendingAction
	err := s.db.Update(func(tx *bolt.Tx) error {
		if err := b(tx, "actions").ForEach(func(_, raw []byte) error {
			if selected != nil {
				return nil
			}
			var a PendingAction
			if err := json.Unmarshal(raw, &a); err != nil {
				return err
			}
			if a.Receipt.State == "pending" && !a.NotBefore.After(s.now()) {
				selected = &a
			}
			return nil
		}); err != nil {
			return err
		}
		if selected == nil {
			return nil
		}
		selected.Receipt.State = "running"
		selected.Receipt.Attempts++
		selected.Receipt.UpdatedAt = stamp(s.now())
		return put(tx, "actions", selected.Receipt.ID, selected)
	})
	if err != nil {
		return nil, err
	}
	return selected, nil
}
func (s *Store) CompleteAction(id, state string, result json.RawMessage, reason string) error {
	if state != "succeeded" && state != "failed" && state != "uncertain" {
		return fmt.Errorf("invalid terminal state")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		var a PendingAction
		if err := get(tx, "actions", id, &a); err != nil {
			return err
		}
		if a.Receipt.State != "running" {
			return ErrConflict
		}
		a.Receipt.State = state
		a.Receipt.Result = result
		a.Receipt.Error = reason
		a.Receipt.UpdatedAt = stamp(s.now())
		return put(tx, "actions", id, a)
	})
}
func (s *Store) GetMeta(key string) (string, error) {
	var value string
	err := s.db.View(func(tx *bolt.Tx) error {
		v := b(tx, "meta").Get([]byte("user:" + key))
		if v == nil {
			return ErrNotFound
		}
		value = string(v)
		return nil
	})
	return value, err
}
func (s *Store) SetMeta(key, value string) error {
	return s.db.Update(func(tx *bolt.Tx) error { return b(tx, "meta").Put([]byte("user:"+key), []byte(value)) })
}
func (s *Store) Stats() (map[string]int, error) {
	out := map[string]int{}
	err := s.db.View(func(tx *bolt.Tx) error {
		out["events"] = b(tx, "events").Stats().KeyN
		if err := b(tx, "deliveries").ForEach(func(_, raw []byte) error {
			var d delivery
			if err := json.Unmarshal(raw, &d); err != nil {
				return err
			}
			out["deliveries_"+d.State]++
			return nil
		}); err != nil {
			return err
		}
		return b(tx, "actions").ForEach(func(_, raw []byte) error {
			var a PendingAction
			if err := json.Unmarshal(raw, &a); err != nil {
				return err
			}
			out["actions_"+a.Receipt.State]++
			return nil
		})
	})
	return out, err
}
func (s *Store) Health() error {
	return s.db.View(func(tx *bolt.Tx) error {
		for _, name := range bucketNames {
			if b(tx, name) == nil {
				return fmt.Errorf("missing bucket %s", name)
			}
		}
		return nil
	})
}
func (s *Store) Close() error {
	err := s.db.Close()
	if s.closeDirectory != nil {
		s.closeDirectory()
	}
	return err
}

// ScheduleActionRetry is only for a response that proves GitHub did not execute
// the mutation (e.g. a confirmed rate limit). Ambiguous transport/5xx outcomes
// must be completed uncertain, not scheduled here.
func (s *Store) ScheduleActionRetry(id string, after time.Duration, reason string) error {
	if after < 0 {
		return fmt.Errorf("negative retry delay")
	}
	if after > 24*time.Hour {
		after = 24 * time.Hour
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		var a PendingAction
		if err := get(tx, "actions", id, &a); err != nil {
			return err
		}
		if a.Receipt.State != "running" {
			return ErrConflict
		}
		a.Receipt.Error = reason
		a.Receipt.UpdatedAt = stamp(s.now())
		if a.Receipt.Attempts >= s.options.Load().MaxAttempts {
			a.Receipt.State = "failed"
			a.Receipt.Error = "maximum safe retry attempts exhausted: " + reason
		} else {
			a.Receipt.State = "pending"
			backoff := time.Second * time.Duration(1<<uint(a.Receipt.Attempts-1))
			if backoff > time.Hour {
				backoff = time.Hour
			}
			if after < backoff {
				after = backoff
			}
			a.NotBefore = s.now().Add(after)
		}
		return put(tx, "actions", id, a)
	})
}
func (s *Store) ClientStats(client string) (map[string]int, error) {
	out := map[string]int{"pending": 0, "inflight": 0, "deadletter": 0, "acked": 0, "actions_pending": 0, "actions_running": 0, "actions_succeeded": 0, "actions_failed": 0, "actions_uncertain": 0}
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := b(tx, "deliveries").ForEach(func(_, raw []byte) error {
			var d delivery
			if err := json.Unmarshal(raw, &d); err != nil {
				return err
			}
			if d.Client == client {
				state := d.State
				if state == "leased" {
					state = "inflight"
				}
				if state == "dead" {
					state = "deadletter"
				}
				out[state]++
			}
			return nil
		}); err != nil {
			return err
		}
		return b(tx, "actions").ForEach(func(_, raw []byte) error {
			var a PendingAction
			if err := json.Unmarshal(raw, &a); err != nil {
				return err
			}
			if a.Client == client {
				out["actions_"+a.Receipt.State]++
			}
			return nil
		})
	})
	return out, err
}

// DeliveryEvent reveals a delivery's context only to its owning client.
func (s *Store) DeliveryEvent(client, id string) (v1.Event, error) {
	var event v1.Event
	err := s.db.View(func(tx *bolt.Tx) error {
		var d delivery
		if err := get(tx, "deliveries", id, &d); err != nil {
			return err
		}
		if d.Client != client {
			return ErrNotFound
		}
		return get(tx, "events", d.EventID, &event)
	})
	if err != nil {
		return v1.Event{}, err
	}
	return event, nil
}

// Action returns the original submission for current-grant checks on receipts.
func (s *Store) Action(client, id string) (v1.Action, error) {
	var a PendingAction
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := get(tx, "actions", id, &a); err != nil {
			return err
		}
		if a.Client != client {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return v1.Action{}, err
	}
	return a.Action, nil
}

// BindRepositories pins each alias to an immutable canonical repository and
// installation identity supplied by configuration. Removed aliases stay pinned
// forever. Validation and additions commit atomically; callers must invoke this
// before exposing handlers or starting workers.
func (s *Store) BindRepositories(bindings map[string]string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		for alias, identity := range bindings {
			if alias == "" || identity == "" {
				return fmt.Errorf("empty repository binding")
			}
			key := []byte("repo-binding:" + alias)
			if previous := b(tx, "meta").Get(key); previous != nil && string(previous) != identity {
				return fmt.Errorf("%w: repository alias %q changed identity", ErrConflict, alias)
			}
		}
		for alias, identity := range bindings {
			if err := b(tx, "meta").Put([]byte("repo-binding:"+alias), []byte(identity)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Reconfigure preserves durable alias identities before publishing new limits.
// A rejected binding or invalid option leaves the previous limits in place.
func (s *Store) Reconfigure(bindings map[string]string, options Options) error {
	options, err := normalizeOptions(options)
	if err != nil {
		return err
	}
	if err = s.BindRepositories(bindings); err != nil {
		return err
	}
	s.options.Store(&options)
	return nil
}

// ListDeadLetters returns bounded dead-letter context within current repository
// grants. A nil or empty allowlist denies all repositories.
func (s *Store) ListDeadLetters(client string, allowedRepositories []string, limit int) ([]v1.DeadLetter, error) {
	if client == "" || limit < 1 || limit > 100 {
		return nil, fmt.Errorf("invalid dead-letter listing bounds")
	}
	allowed := map[string]bool{}
	for _, repo := range allowedRepositories {
		allowed[repo] = true
	}
	out := []v1.DeadLetter{}
	err := s.db.View(func(tx *bolt.Tx) error {
		return b(tx, "deliveries").ForEach(func(_, raw []byte) error {
			if len(out) >= limit {
				return nil
			}
			var d delivery
			if err := json.Unmarshal(raw, &d); err != nil {
				return err
			}
			if d.Client != client || d.State != "dead" {
				return nil
			}
			var event v1.Event
			if err := get(tx, "events", d.EventID, &event); err != nil {
				return err
			}
			if !allowed[event.Repository] {
				return nil
			}
			out = append(out, v1.DeadLetter{ID: d.ID, Event: event, Attempts: d.Attempts, Error: d.Error})
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
