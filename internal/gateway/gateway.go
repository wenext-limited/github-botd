// Package gateway composes routing, durable queues, and GitHub adapters.
package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	v1 "github.com/wenext-limited/github-botd/api/v1"
	"github.com/wenext-limited/github-botd/internal/config"
	gh "github.com/wenext-limited/github-botd/internal/github"
	"github.com/wenext-limited/github-botd/internal/store"
	"log/slog"
	"strings"
	"time"
)

type GitHub interface {
	Execute(context.Context, int64, v1.Action) (json.RawMessage, error)
	Context(context.Context, int64, v1.ContextRequest) (json.RawMessage, error)
}
type Poller interface {
	PollRepository(context.Context, int64, string, string) ([]v1.Event, string, error)
}
type Gateway struct {
	Config *config.Config
	Store  *store.Store
	GitHub GitHub
	Log    *slog.Logger
}

func New(c *config.Config, s *store.Store, g GitHub) *Gateway {
	return &Gateway{Config: c, Store: s, GitHub: g, Log: slog.Default()}
}

// Ingest validates identity and repository routing before committing any local delivery.
func (g *Gateway) Ingest(source, eventType, deliveryID string, body []byte) (bool, error) {
	src, ok := g.Config.Webhook.Sources[source]
	if !ok {
		return false, errors.New("unknown webhook source")
	}
	e, err := gh.Normalize(eventType, deliveryID, body)
	if err != nil {
		return false, err
	}
	if len(src.InstallationIDs) > 0 {
		found := false
		for _, id := range src.InstallationIDs {
			if id == e.InstallationID {
				found = true
			}
		}
		if !found {
			return false, errors.New("installation is not allowed for this source")
		}
	}
	return g.ingestEvent("webhook:"+source, deliveryID, e)
}
func (g *Gateway) ingestEvent(source, deliveryID string, e v1.Event) (bool, error) {
	alias := ""
	for a, r := range g.Config.Repositories {
		if strings.EqualFold(e.Repository, r.FullName) {
			if r.InstallationID != 0 && r.InstallationID != e.InstallationID {
				return false, errors.New("repository installation mismatch")
			}
			alias = a
			break
		}
	}
	if alias == "" {
		return false, nil
	} // Unconfigured repositories are intentionally ignored.
	e.Repository = alias
	e.Version = v1.Version
	e.Source = source
	e.DeliveryID = deliveryID
	hash := sha256.Sum256([]byte(source + "\x00" + deliveryID))
	e.ID = "evt_" + hex.EncodeToString(hash[:16])
	if e.Timestamp == "" {
		e.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	// The adapter retains bounded structured context, recursively redacting credential-like fields.
	// GitHub repository/installation identifiers are context, never credentials.
	return g.Store.Ingest(source, deliveryID, e, Route(g.Config, e))
}

func (g *Gateway) Submit(client string, a v1.Action) (v1.Receipt, error) {
	p, ok := g.Config.Clients[client]
	if !ok {
		return v1.Receipt{}, errors.New("unknown client")
	}
	if a.Version != v1.Version {
		return v1.Receipt{}, errors.New("unsupported protocol version")
	}
	if !config.Contains(p.Repositories, a.Repository) || !config.Contains(p.Capabilities, a.Kind) {
		return v1.Receipt{}, ErrForbidden
	}
	if a.EventID == "" && !config.Contains(p.Capabilities, "actions.proactive") {
		return v1.Receipt{}, errors.New("event_id is required without actions.proactive capability")
	}
	if a.EventID != "" {
		e, err := g.Store.Event(client, a.EventID)
		if err != nil {
			return v1.Receipt{}, ErrForbidden
		}
		if e.Repository != a.Repository {
			return v1.Receipt{}, ErrForbidden
		}
		if a.Number != 0 && e.Number != a.Number {
			return v1.Receipt{}, errors.New("action number must match its event")
		}
		if a.Kind == "review.reply" && a.CommentID != e.ThreadID && a.CommentID != e.CommentID {
			return v1.Receipt{}, errors.New("reply must target the delivered thread")
		}
		if a.Kind == "check.create" && (e.HeadSHA == "" || !strings.EqualFold(e.HeadSHA, a.HeadSHA)) {
			return v1.Receipt{}, errors.New("check creation must match the delivered commit")
		}
		if a.Kind == "check.update" && a.CheckRunID > 0 && (e.Type != "check_run" || e.CheckRunID != a.CheckRunID) {
			return v1.Receipt{}, errors.New("check.update requires a check_run event or proactive action")
		}
	}
	if len(a.IdempotencyKey) < 1 || len(a.IdempotencyKey) > 128 {
		return v1.Receipt{}, errors.New("idempotency_key must be 1..128 bytes")
	}
	if a.Kind == "review.submit" || a.Kind == "review.comment" {
		if a.HeadSHA == "" {
			return v1.Receipt{}, errors.New("review requires head_sha")
		}
	}
	// Validation belongs before the durable outbox so invalid work is rejected synchronously.
	if err := ValidateAction(a); err != nil {
		return v1.Receipt{}, err
	}
	actual := a
	actual.Repository = g.Config.Repositories[a.Repository].FullName
	if err := gh.ValidateAction(actual); err != nil {
		return v1.Receipt{}, err
	}
	return g.Store.Enqueue(client, a)
}

var ErrForbidden = errors.New("capability or repository access denied")

func ValidateAction(a v1.Action) error {
	if !config.Capabilities[a.Kind] || a.Kind == "events.read" || a.Kind == "events.replay" || a.Kind == "context.read" || a.Kind == "actions.proactive" {
		return errors.New("unsupported action kind")
	}
	if len(a.Body) > 60000 || len(a.Summary) > 60000 || len(a.Comments) > 100 {
		return errors.New("action content exceeds limit")
	}
	switch a.Kind {
	case "issue.comment.create", "issue.comment.update", "review.reply", "review.comment", "review.submit":
		if a.Number <= 0 {
			return errors.New("positive number required")
		}
		if a.Body == "" && len(a.Comments) == 0 {
			return errors.New("body or review comments required")
		}
		if (a.Kind == "issue.comment.update" || a.Kind == "review.reply") && a.CommentID <= 0 {
			return errors.New("positive comment_id required")
		}
		if a.Kind == "review.comment" {
			if err := validateLine(a.Path, a.Line, a.Side, a.Body); err != nil {
				return err
			}
		}
		for _, r := range a.Comments {
			if err := validateLine(r.Path, r.Line, r.Side, r.Body); err != nil {
				return err
			}
		}
	case "check.create", "check.update":
		if a.Kind == "check.create" && (a.Name == "" || a.HeadSHA == "") {
			return errors.New("check.create requires name and head_sha")
		}
		if a.Kind == "check.update" && a.CheckRunID <= 0 {
			return errors.New("positive check_run_id required")
		}
		if a.Status != "queued" && a.Status != "in_progress" && a.Status != "completed" {
			return errors.New("invalid check status")
		}
		if a.Status == "completed" && !config.Contains([]string{"success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required"}, a.Conclusion) {
			return errors.New("completed check requires supported conclusion")
		}
		if a.Status != "completed" && a.Conclusion != "" {
			return errors.New("conclusion requires completed status")
		}
	}
	return nil
}
func validateLine(path string, line int, side, body string) error {
	if path == "" || strings.HasPrefix(path, "/") || line <= 0 || (side != "LEFT" && side != "RIGHT") || body == "" || len(body) > 60000 {
		return errors.New("inline comment needs relative path, positive line, LEFT/RIGHT side and bounded body")
	}
	return nil
}
func (g *Gateway) Context(ctx context.Context, client string, r v1.ContextRequest) (json.RawMessage, error) {
	p, ok := g.Config.Clients[client]
	if !ok || !config.Contains(p.Capabilities, "context.read") || !config.Contains(p.Repositories, r.Repository) {
		return nil, ErrForbidden
	}
	repo, ok := g.Config.Repositories[r.Repository]
	if !ok {
		return nil, ErrForbidden
	}
	if g.GitHub == nil {
		return nil, errors.New("GitHub reads unavailable in offline dry-run mode")
	}
	r.Repository = repo.FullName
	return g.GitHub.Context(ctx, repo.InstallationID, r)
}

// RunOutbox never retries an ambiguous GitHub write. Only explicit pre-execution rate limits are safe.
func (g *Gateway) RunOutbox(ctx context.Context) error {
	return RunOutbox(ctx, func() *Gateway { return g })
}

// RunOutbox selects an immutable configuration for each action. Reloading never
// cancels an outbound write or starts a second outbox worker.
func RunOutbox(ctx context.Context, current func() *Gateway) error {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		pending, err := current().Store.NextAction()
		if err != nil {
			return fmt.Errorf("claim outbox: %w", err)
		}
		if pending == nil {
			continue
		}
		// Claiming can wait on a store transaction. Select policy afterwards,
		// so a reload during that wait is reflected before authorization.
		g := current()
		a := pending.Action
		repo, ok := g.Config.Repositories[a.Repository]
		if !ok {
			if err = g.Store.CompleteAction(pending.Receipt.ID, "failed", nil, "repository removed from config"); err != nil {
				return err
			}
			continue
		}
		// Re-evaluate grants after restart/config changes before executing previously queued work.
		p, ok := g.Config.Clients[pending.Client]
		if !ok || !config.Contains(p.Repositories, a.Repository) || !config.Contains(p.Capabilities, a.Kind) || (a.EventID == "" && !config.Contains(p.Capabilities, "actions.proactive")) {
			if err = g.Store.CompleteAction(pending.Receipt.ID, "failed", nil, "client grant revoked"); err != nil {
				return err
			}
			continue
		}
		if g.Config.DryRun {
			result, _ := json.Marshal(map[string]any{"dry_run": true, "kind": a.Kind, "repository": a.Repository})
			if err = g.Store.CompleteAction(pending.Receipt.ID, "succeeded", result, ""); err != nil {
				return err
			}
			continue
		}
		a.Repository = repo.FullName
		call, cancel := context.WithTimeout(ctx, 45*time.Second)
		result, executeErr := g.GitHub.Execute(call, repo.InstallationID, a)
		cancel()
		state, reason := "succeeded", ""
		if executeErr != nil {
			state = "uncertain"
			reason = "GitHub write outcome is unknown; inspect GitHub before retrying"
			var apiErr *gh.APIError
			if errors.As(executeErr, &apiErr) {
				state = apiErr.State
				reason = executeErr.Error()
				if state == "retryable" {
					if err = g.Store.ScheduleActionRetry(pending.Receipt.ID, apiErr.RetryAfter, reason); err != nil {
						return err
					}
					continue
				}
			}
		}
		if err = g.Store.CompleteAction(pending.Receipt.ID, state, result, reason); err != nil {
			return fmt.Errorf("persist outbox result: %w", err)
		}
	}
}
func (g *Gateway) RunPolling(ctx context.Context) error {
	if !g.Config.Poll.Enabled {
		return nil
	}
	return RunPolling(ctx, func() *Gateway { return g })
}

// RunPolling observes enablement, interval and repository changes without
// overlapping poll batches. An in-flight batch finishes using its snapshot.
func RunPolling(ctx context.Context, current func() *Gateway) error {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	var previous *Gateway
	var next time.Time
	for {
		if ctx.Err() != nil {
			return nil
		}
		g := current()
		if g != previous {
			previous = g
			next = time.Time{}
		}
		if g.Config.Poll.Enabled && !time.Now().Before(next) {
			if err := g.pollOnce(ctx); err != nil {
				return err
			}
			next = time.Now().Add(time.Duration(g.Config.Poll.IntervalSeconds) * time.Second)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func (g *Gateway) pollOnce(ctx context.Context) error {
	poller, ok := g.GitHub.(Poller)
	if !ok {
		return errors.New("configured adapter does not support polling")
	}
	for _, alias := range g.Config.Poll.Repositories {
		if ctx.Err() != nil {
			return nil
		}
		repo := g.Config.Repositories[alias]
		key := "poll:" + alias
		cursor, err := g.Store.GetMeta(key)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			g.Log.Error("poll cursor unavailable", "repository", alias)
			continue
		}
		call, cancel := context.WithTimeout(ctx, 45*time.Second)
		events, next, err := poller.PollRepository(call, repo.InstallationID, repo.FullName, cursor)
		cancel()
		if err != nil {
			g.Log.Warn("poll failed; cursor unchanged", "repository", alias)
			continue
		}
		complete := true
		for _, e := range events {
			e.InstallationID = repo.InstallationID
			if _, err = g.ingestEvent(key, e.DeliveryID, e); err != nil {
				complete = false
				break
			}
		}
		if complete {
			if err = g.Store.SetMeta(key, next); err != nil {
				g.Log.Error("poll cursor persist failed", "repository", alias)
			}
		}
	}
	return nil
}
