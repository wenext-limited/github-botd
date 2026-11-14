package gateway

import (
	"context"
	"encoding/json"
	"errors"
	v1 "github.com/wenext-limited/github-botd/api/v1"
	"github.com/wenext-limited/github-botd/internal/config"
	gh "github.com/wenext-limited/github-botd/internal/github"
	"github.com/wenext-limited/github-botd/internal/store"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fixture(t *testing.T) *Gateway {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return New(&config.Config{DryRun: true, Repositories: map[string]config.Repository{"main": {FullName: "acme/project"}}, Clients: map[string]config.Client{"agent": {Repositories: []string{"main"}, Capabilities: []string{"events.read", "context.read", "review.reply", "review.submit", "issue.comment.create", "check.update"}}}, Webhook: config.Webhook{Sources: map[string]config.Source{"app": {InstallationIDs: []int64{1}}}}, Routes: []config.Route{{ID: "mentions", Repositories: []string{"main"}, Events: []string{"issue_comment", "pull_request_review_comment"}, Trigger: "mention", Names: []string{"bot"}, Actors: []string{"alice"}, Clients: []string{"agent"}}, {ID: "reviews", Repositories: []string{"main"}, Events: []string{"pull_request"}, Trigger: "review_requested", Names: []string{"reviewer"}, Actors: []string{"alice"}, Clients: []string{"agent"}}}}, s, nil)
}
func TestRoutingAndSignedSourceScope(t *testing.T) {
	g := fixture(t)
	body := []byte(`{"action":"created","repository":{"full_name":"acme/project"},"installation":{"id":1},"sender":{"login":"alice","type":"User"},"issue":{"number":42},"comment":{"id":11,"body":"@bot inspect this"}}`)
	fresh, err := g.Ingest("app", "issue_comment", "d1", body)
	if err != nil || !fresh {
		t.Fatalf("%v %v", fresh, err)
	}
	fresh, err = g.Ingest("app", "issue_comment", "d1", body)
	if err != nil || fresh {
		t.Fatalf("duplicate %v %v", fresh, err)
	}
	items, err := g.Store.Claim("agent", 1, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("%v %v", items, err)
	}
	if items[0].Event.Repository != "main" || items[0].Event.Data == nil {
		t.Fatal("structured context/alias missing")
	}
	if _, err = g.Ingest("app", "issue_comment", "d2", []byte(strings.ReplaceAll(string(body), `"id":1}`, `"id":99}`))); err == nil {
		t.Fatal("foreign installation accepted")
	}
	e := items[0].Event
	e.Actor = "mallory"
	if len(Route(g.Config, e)) != 0 {
		t.Fatal("actor bypass")
	}
	e.Actor = "alice"
	e.ActorType = "Bot"
	if len(Route(g.Config, e)) != 0 {
		t.Fatal("bot loop")
	}
	e.ActorType = "User"
	e.Type = "pull_request"
	e.Action = "review_requested"
	e.RequestedReviewer = "reviewer"
	if len(Route(g.Config, e)) != 1 {
		t.Fatal("review request lost")
	}
}
func TestCorrelatedActionsAndIdempotency(t *testing.T) {
	g := fixture(t)
	e := v1.Event{ID: "e1", Repository: "main", Number: 42, CommentID: 13, ThreadID: 11, Type: "pull_request_review_comment"}
	g.Store.Ingest("app", "d1", e, []string{"agent"})
	a := v1.Action{Version: v1.Version, Kind: "review.reply", Repository: "main", EventID: "e1", Number: 42, CommentID: 11, Body: "reply", IdempotencyKey: "k"}
	r, err := g.Submit("agent", a)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := g.Submit("agent", a)
	if err != nil || r.ID != r2.ID {
		t.Fatal("dedupe failed")
	}
	a.Body = "changed"
	if _, err = g.Submit("agent", a); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("mismatch accepted: %v", err)
	}
	a.IdempotencyKey = "k2"
	a.CommentID = 99
	if _, err = g.Submit("agent", a); err == nil {
		t.Fatal("cross-thread accepted")
	}
	a.CommentID = 11
	a.EventID = ""
	if _, err = g.Submit("agent", a); err == nil {
		t.Fatal("proactive bypass")
	}
	a.EventID = "e1"
	a.Number = 43
	if _, err = g.Submit("agent", a); err == nil {
		t.Fatal("cross-PR accepted")
	}
}

type fakeGitHub struct {
	mu     sync.Mutex
	calls  int
	err    error
	polled chan string
}

func (f *fakeGitHub) Execute(context.Context, int64, v1.Action) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return json.RawMessage(`{"id":9}`), f.err
}
func (f *fakeGitHub) Context(context.Context, int64, v1.ContextRequest) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (f *fakeGitHub) PollRepository(ctx context.Context, _ int64, _ string, cursor string) ([]v1.Event, string, error) {
	select {
	case f.polled <- cursor:
	default:
	}
	return nil, "cursor", nil
}
func TestOutboxUncertainNoRetryAndRevokedProactive(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(string(rune('a'+boolInt(revoke))), func(t *testing.T) {
			g := fixture(t)
			g.Config.DryRun = false
			fake := &fakeGitHub{err: &gh.APIError{State: "uncertain"}}
			g.GitHub = fake
			p := g.Config.Clients["agent"]
			p.Capabilities = append(p.Capabilities, "actions.proactive")
			g.Config.Clients["agent"] = p
			r, err := g.Submit("agent", v1.Action{Version: v1.Version, IdempotencyKey: "one", Kind: "issue.comment.create", Repository: "main", Number: 1, Body: "hello"})
			if err != nil {
				t.Fatal(err)
			}
			if revoke {
				p.Capabilities = p.Capabilities[:len(p.Capabilities)-1]
				g.Config.Clients["agent"] = p
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error)
			go func() { done <- g.RunOutbox(ctx) }()
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				got, _ := g.Store.Receipt("agent", r.ID)
				if got.State == "uncertain" || got.State == "failed" {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			cancel()
			if err = <-done; err != nil {
				t.Fatal(err)
			}
			got, _ := g.Store.Receipt("agent", r.ID)
			fake.mu.Lock()
			calls := fake.calls
			fake.mu.Unlock()
			if revoke {
				if got.State != "failed" || calls != 0 {
					t.Fatalf("revoked ran: %+v %d", got, calls)
				}
			} else if got.State != "uncertain" || calls != 1 {
				t.Fatalf("bad outcome %+v %d", got, calls)
			}
		})
	}
}
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
func TestPollingBootstrapsEmptyMetadata(t *testing.T) {
	g := fixture(t)
	g.Config.Poll = config.Poll{Enabled: true, IntervalSeconds: 30, Repositories: []string{"main"}}
	f := &fakeGitHub{polled: make(chan string, 1)}
	g.GitHub = f
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- g.RunPolling(ctx) }()
	select {
	case cursor := <-f.polled:
		if cursor != "" {
			t.Fatal("bad bootstrap")
		}
	case <-time.After(time.Second):
		t.Fatal("poll never bootstrapped")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPollingObservesConfigurationChanges(t *testing.T) {
	g := fixture(t)
	f := &fakeGitHub{polled: make(chan string, 4)}
	g.GitHub = f
	var active atomic.Pointer[Gateway]
	active.Store(g)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunPolling(ctx, active.Load) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	// Enabling polling must work even when the daemon started with it disabled.
	c := *g.Config
	c.Poll = config.Poll{Enabled: true, IntervalSeconds: 30, Repositories: []string{"main"}}
	active.Store(New(&c, g.Store, f))
	waitPoll := func(expected string) {
		t.Helper()
		select {
		case got := <-f.polled:
			if got != expected {
				t.Fatal("cursor changed across reload", got, expected)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("polling did not observe updated config")
		}
	}
	waitPoll("")
	// A new interval takes effect without waiting for the original 30s timer.
	updated := c
	updated.Poll.IntervalSeconds = 60
	active.Store(New(&updated, g.Store, f))
	waitPoll("cursor")
	disabled := c
	disabled.Poll.Enabled = false
	active.Store(New(&disabled, g.Store, f))
	select {
	case <-f.polled:
		t.Fatal("disabled polling ran")
	case <-time.After(350 * time.Millisecond):
	}
	active.Store(New(&c, g.Store, f))
	waitPoll("cursor")
}
func TestCheckUpdateExactEvent(t *testing.T) {
	g := fixture(t)
	g.Store.Ingest("app", "d", v1.Event{ID: "e", Repository: "main", Type: "check_run", CheckRunID: 10}, []string{"agent"})
	a := v1.Action{Version: v1.Version, IdempotencyKey: "key", Kind: "check.update", Repository: "main", EventID: "e", CheckRunID: 11, Status: "completed", Conclusion: "success", Name: "ci", Summary: "done"}
	if _, err := g.Submit("agent", a); err == nil {
		t.Fatal("cross-check update accepted")
	}
}

func TestCheckCreationUsesEventCommit(t *testing.T) {
	g := fixture(t)
	p := g.Config.Clients["agent"]
	p.Capabilities = append(p.Capabilities, "check.create")
	g.Config.Clients["agent"] = p
	g.Store.Ingest("app", "d", v1.Event{ID: "commit-event", Repository: "main", Type: "workflow_run", HeadSHA: strings.Repeat("a", 40)}, []string{"agent"})
	a := v1.Action{Version: v1.Version, IdempotencyKey: "ci", Kind: "check.create", Repository: "main", EventID: "commit-event", HeadSHA: strings.Repeat("b", 40), Name: "CI", Status: "completed", Conclusion: "success"}
	if _, err := g.Submit("agent", a); err == nil {
		t.Fatal("cross-commit check accepted")
	}
}
