package store

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	v1 "github.com/wenext-limited/github-botd/api/v1"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(privateDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func event(id string) v1.Event { return v1.Event{ID: id, Repository: "o/r", Type: "issues"} }
func action(key string) v1.Action {
	return v1.Action{IdempotencyKey: key, Kind: "issue_comment", Repository: "o/r", Body: "hi"}
}

func TestReconfigureLimitsAndBindingRollback(t *testing.T) {
	s := openTest(t)
	if err := s.Reconfigure(map[string]string{"repo": "acme/repo:0"}, Options{MaxAttempts: 1, MaxPending: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue("alice", action("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue("alice", action("two")); !errors.Is(err, ErrCapacity) {
		t.Fatal("new capacity not enforced", err)
	}
	if err := s.Reconfigure(map[string]string{"repo": "acme/other:0", "new": "acme/new:0"}, Options{MaxAttempts: 2, MaxPending: 2}); !errors.Is(err, ErrConflict) {
		t.Fatal("changed alias accepted", err)
	}
	if _, err := s.Enqueue("alice", action("two")); !errors.Is(err, ErrCapacity) {
		t.Fatal("rejected config changed limits", err)
	}
	// A failed reload must not pin aliases from the rest of its candidate.
	if err := s.Reconfigure(map[string]string{"repo": "acme/repo:0", "new": "acme/different:0"}, Options{MaxAttempts: 2, MaxPending: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue("alice", action("two")); err != nil {
		t.Fatal("increased capacity not applied", err)
	}
	if _, err := s.Ingest("source", "event", event("event"), []string{"alice"}); err != nil {
		t.Fatal(err)
	}
	delivery := claim(t, s, "alice")
	if err := s.Reconfigure(nil, Options{MaxAttempts: 1, MaxPending: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.Ack("alice", delivery.ID, delivery.LeaseToken, true, "retry"); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.ListDeadLetters("alice", []string{"o/r"}, 10); err != nil || len(rows) != 1 {
		t.Fatal("new attempt cap not applied", rows, err)
	}
}
func mustIngest(t *testing.T, s *Store, id string) {
	t.Helper()
	ok, err := s.Ingest("app1/route1", id, event(id), []string{"alice", "bob"})
	if err != nil || !ok {
		t.Fatalf("ingest: %v %v", ok, err)
	}
}
func claim(t *testing.T, s *Store, client string) v1.Delivery {
	t.Helper()
	ds, err := s.Claim(client, 1, time.Minute)
	if err != nil || len(ds) != 1 {
		t.Fatalf("claim: %#v %v", ds, err)
	}
	return ds[0]
}

// These invariant tests act as an ablation study: removing the scoped dedupe
// index produces duplicate fanout; removing lease tokens permits stale owners to
// ack a new owner's work; removing the client scope exposes foreign receipts.
func TestAblationDedupeAndRouteScope(t *testing.T) {
	s := openTest(t)
	mustIngest(t, s, "e1")
	added, err := s.Ingest("app1/route1", "e1", event("ignored"), []string{"mallory"})
	if err != nil || added {
		t.Fatalf("duplicate accepted: %v %v", added, err)
	}
	if s.HasEvent("mallory", "e1") {
		t.Fatal("duplicate altered fanout")
	}
	added, err = s.Ingest("app2/route1", "e1", event("e2"), []string{"alice"})
	if err != nil || !added {
		t.Fatalf("source scope lost: %v %v", added, err)
	}
	ds, err := s.Claim("alice", 100, time.Minute)
	if err != nil || len(ds) != 2 {
		t.Fatalf("want exactly 2: %d %v", len(ds), err)
	}
	if _, err := s.Event("mallory", "e1"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
func TestAblationLeaseRotationAndOwnership(t *testing.T) {
	s := openTest(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	mustIngest(t, s, "e")
	d := claim(t, s, "alice")
	if err := s.Ack("bob", d.ID, d.LeaseToken, false, ""); !errors.Is(err, ErrLease) {
		t.Fatal("cross-client ack", err)
	}
	if ds, _ := s.Claim("alice", 1, time.Minute); len(ds) != 0 {
		t.Fatal("leased twice")
	}
	now = now.Add(time.Minute)
	if err := s.Ack("alice", d.ID, d.LeaseToken, false, ""); !errors.Is(err, ErrLease) {
		t.Fatal("expired ack", err)
	}
	newer := claim(t, s, "alice")
	if newer.LeaseToken == d.LeaseToken || newer.Attempt != 2 {
		t.Fatal("lease did not rotate")
	}
	if err := s.Ack("alice", d.ID, d.LeaseToken, false, ""); !errors.Is(err, ErrLease) {
		t.Fatal("stale ack", err)
	}
	if err := s.Ack("alice", newer.ID, newer.LeaseToken, false, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Replay("bob", newer.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("foreign replay", err)
	}
	if err := s.Replay("alice", newer.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("replayed acknowledged work", err)
	}
}
func TestBackoffDeadletterAndBounds(t *testing.T) {
	s := openTest(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	mustIngest(t, s, "e")
	for i := 1; i <= MaxAttempts; i++ {
		d := claim(t, s, "alice")
		if d.Attempt != i {
			t.Fatal(d.Attempt)
		}
		if err := s.Ack("alice", d.ID, d.LeaseToken, true, "retry"); err != nil {
			t.Fatal(err)
		}
		if ds, _ := s.Claim("alice", 1, time.Minute); len(ds) != 0 {
			t.Fatal("retry skipped backoff")
		}
		now = now.Add(time.Hour)
	}
	stats, err := s.Stats()
	if err != nil || stats["deliveries_dead"] != 1 {
		t.Fatal(stats, err)
	}
	for _, limit := range []int{-1, 0, 101} {
		if _, err := s.Claim("alice", limit, time.Minute); err == nil {
			t.Fatal("unbounded claim")
		}
	}
	if _, err := s.Claim("alice", 1, 0); err == nil {
		t.Fatal("zero lease")
	}
}
func TestConcurrentActionDedupeScopeMismatchAndClaim(t *testing.T) {
	s := openTest(t)
	var wg sync.WaitGroup
	ids := make(chan string, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.Enqueue("alice", action("key"))
			if err != nil {
				t.Error(err)
				return
			}
			ids <- r.ID
		}()
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatal("multiple outbox entries")
		}
	}
	changed := action("key")
	changed.Body = "different"
	if _, err := s.Enqueue("alice", changed); !errors.Is(err, ErrConflict) {
		t.Fatal("key mismatch not rejected", err)
	}
	bob, err := s.Enqueue("bob", action("key"))
	if err != nil || bob.ID == first {
		t.Fatal("client key scope", err)
	}
	if _, err := s.Receipt("bob", first); !errors.Is(err, ErrNotFound) {
		t.Fatal("receipt disclosure", err)
	}
	claimed := make(chan string, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := s.NextAction()
			if err != nil {
				t.Error(err)
			}
			if a != nil {
				claimed <- a.Receipt.ID
			}
		}()
	}
	wg.Wait()
	close(claimed)
	seen := map[string]bool{}
	for id := range claimed {
		if seen[id] {
			t.Fatal("double claimed")
		}
		seen[id] = true
	}
	if len(seen) != 2 {
		t.Fatal(seen)
	}
	if err := s.CompleteAction(first, "succeeded", json.RawMessage(`{"id":1}`), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteAction(first, "failed", nil, "oops"); !errors.Is(err, ErrConflict) {
		t.Fatal("terminal receipt rewritten", err)
	}
}
func TestTransactionRollbackAndClosedStore(t *testing.T) {
	s := openTest(t)
	if _, err := s.Ingest("src", "bad", event("bad"), []string{"alice", ""}); err == nil {
		t.Fatal("invalid recipient accepted")
	}
	stats, _ := s.Stats()
	if stats["events"] != 0 {
		t.Fatal("partial transaction", stats)
	}
	ok, err := s.Ingest("src", "bad", event("bad"), []string{"alice"})
	if err != nil || !ok {
		t.Fatal("dedupe marker survived rollback", err)
	}
	r, err := s.Enqueue("alice", action("key"))
	if err != nil {
		t.Fatal(err)
	}
	s.NextAction()
	if err = s.CompleteAction(r.ID, "succeeded", json.RawMessage(`{broken`), ""); err == nil {
		t.Fatal("invalid result accepted")
	}
	r, err = s.Receipt("alice", r.ID)
	if err != nil || r.State != "running" {
		t.Fatal("failed persist changed receipt", r, err)
	}
	s.Close()
	if _, err = s.Enqueue("alice", action("other")); err == nil {
		t.Fatal("closed write succeeded")
	}
	if err = s.Health(); err == nil {
		t.Fatal("closed store healthy")
	}
}
func TestAbruptExitRecovery(t *testing.T) {
	if os.Getenv("BOTD_STORE_CRASH_HELPER") == "1" {
		s, err := Open(os.Getenv("BOTD_STORE_DIR"))
		if err != nil {
			os.Exit(2)
		}
		if _, err = s.Ingest("src", "delivery", event("e"), []string{"alice"}); err != nil {
			os.Exit(3)
		}
		if _, err = s.Enqueue("alice", action("running")); err != nil {
			os.Exit(4)
		}
		if _, err = s.NextAction(); err != nil {
			os.Exit(5)
		}
		if _, err = s.Enqueue("alice", action("pending")); err != nil {
			os.Exit(6)
		}
		if err = s.SetMeta("cursor", "123"); err != nil {
			os.Exit(7)
		}
		os.Exit(0) // No Close: abrupt process exit after acknowledged commits.
	}
	dir := privateDir(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestAbruptExitRecovery$")
	cmd.Env = append(os.Environ(), "BOTD_STORE_CRASH_HELPER=1", "BOTD_STORE_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("crash helper %v %s", err, out)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, err := s.Enqueue("alice", action("running"))
	if err != nil || r.State != "uncertain" {
		t.Fatal("unsafe retry on recovery", r, err)
	}
	a, err := s.NextAction()
	if err != nil || a == nil || a.Action.IdempotencyKey != "pending" {
		t.Fatal(a, err)
	}
	if a, err = s.NextAction(); err != nil || a != nil {
		t.Fatal("uncertain retried", a, err)
	}
	if v, err := s.GetMeta("cursor"); err != nil || v != "123" {
		t.Fatal(v, err)
	}
	claim(t, s, "alice")
	if added, err := s.Ingest("src", "delivery", event("other"), []string{"alice"}); err != nil || added {
		t.Fatal("dedupe lost on crash", err)
	}
}
func TestPermissionsAndExclusiveOpen(t *testing.T) {
	dir := privateDir(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if other, err := Open(dir); err == nil {
		other.Close()
		t.Fatal("simultaneous writers opened")
	}
	info, err := os.Stat(dir + "/github-botd.db")
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	linkDir := privateDir(t)
	if err := os.Symlink(dir+"/github-botd.db", linkDir+"/github-botd.db"); err != nil {
		t.Fatal(err)
	}
	if other, err := Open(linkDir); err == nil {
		other.Close()
		t.Fatal("followed symlink")
	}
}

func TestCapacityAndSafeActionRetry(t *testing.T) {
	s, err := OpenWithOptions(privateDir(t), Options{MaxAttempts: 2, MaxPending: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	s.now = func() time.Time { return now }
	if _, err = s.Ingest("src", "one", event("one"), []string{"alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Ingest("src", "two", event("two"), []string{"alice"}); !errors.Is(err, ErrCapacity) {
		t.Fatal("capacity", err)
	}
	if added, err := s.Ingest("src", "one", event("one"), []string{"alice"}); err != nil || added {
		t.Fatal("duplicate at capacity", err)
	}
	r, err := s.Enqueue("alice", action("key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Enqueue("alice", action("other")); !errors.Is(err, ErrCapacity) {
		t.Fatal("outbox capacity", err)
	}
	if duplicate, err := s.Enqueue("alice", action("key")); err != nil || duplicate.ID != r.ID {
		t.Fatal("receipt at capacity", err)
	}
	if _, err = s.NextAction(); err != nil {
		t.Fatal(err)
	}
	if err = s.ScheduleActionRetry(r.ID, 2*time.Minute, "rate limit"); err != nil {
		t.Fatal(err)
	}
	if a, err := s.NextAction(); err != nil || a != nil {
		t.Fatal("not-before ignored", a, err)
	}
	now = now.Add(2 * time.Minute)
	a, err := s.NextAction()
	if err != nil || a == nil || a.Receipt.Attempts != 2 {
		t.Fatal(a, err)
	}
	if err = s.ScheduleActionRetry(r.ID, 0, "rate limit"); err != nil {
		t.Fatal(err)
	}
	r, err = s.Receipt("alice", r.ID)
	if err != nil || r.State != "failed" {
		t.Fatal(r, err)
	}
	alice, err := s.ClientStats("alice")
	if err != nil || alice["pending"] != 1 || alice["actions_failed"] != 1 {
		t.Fatal(alice, err)
	}
	bob, err := s.ClientStats("bob")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range bob {
		if n != 0 {
			t.Fatal("stats disclosure", bob)
		}
	}
}
func TestLeaseSurvivesReopenAndExpiredAttemptsDie(t *testing.T) {
	dir := privateDir(t)
	s, err := OpenWithOptions(dir, Options{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	mustIngest(t, s, "e")
	d := claim(t, s, "alice")
	s.Close()
	s, err = OpenWithOptions(dir, Options{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if ds, err := s.Claim("alice", 1, time.Minute); err != nil || len(ds) != 0 {
		t.Fatal("unexpired lease lost", ds, err)
	}
	deadline, err := time.Parse(time.RFC3339Nano, d.LeaseExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return deadline.Add(time.Second) }
	if ds, err := s.Claim("alice", 1, time.Minute); err != nil || len(ds) != 0 {
		t.Fatal("attempt cap lost", ds, err)
	}
	stats, err := s.ClientStats("alice")
	if err != nil || stats["deadletter"] != 1 {
		t.Fatal(stats, err)
	}
}

func privateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestClaimAllowedAndScopedLookup(t *testing.T) {
	s := openTest(t)
	for _, repo := range []string{"allowed", "revoked"} {
		e := event(repo)
		e.Repository = repo
		if _, err := s.Ingest("src", repo, e, []string{"alice"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, repos := range [][]string{nil, {}, {"missing"}} {
		ds, err := s.ClaimAllowed("alice", 100, time.Minute, repos)
		if err != nil || len(ds) != 0 {
			t.Fatal("empty scope leaked event", ds, err)
		}
	}
	ds, err := s.ClaimAllowed("alice", 100, time.Minute, []string{"allowed"})
	if err != nil || len(ds) != 1 || ds[0].Event.Repository != "allowed" {
		t.Fatal(ds, err)
	}
	e, err := s.DeliveryEvent("alice", ds[0].ID)
	if err != nil || e.ID != "allowed" {
		t.Fatal(e, err)
	}
	if e, err = s.DeliveryEvent("bob", ds[0].ID); !errors.Is(err, ErrNotFound) || e.ID != "" {
		t.Fatal("foreign delivery disclosure", e, err)
	}
	// A previously filtered entry must not consume attempts or acquire a lease.
	ds, err = s.ClaimAllowed("alice", 100, time.Minute, []string{"revoked"})
	if err != nil || len(ds) != 1 || ds[0].Attempt != 1 {
		t.Fatal(ds, err)
	}
	r, err := s.Enqueue("alice", action("key"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Action("alice", r.ID)
	if err != nil || a.IdempotencyKey != "key" {
		t.Fatal(a, err)
	}
	if a, err = s.Action("bob", r.ID); !errors.Is(err, ErrNotFound) || a.IdempotencyKey != "" {
		t.Fatal("foreign action disclosure", a, err)
	}
}
func TestImmutableRepositoryBindings(t *testing.T) {
	dir := privateDir(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BindRepositories(map[string]string{"repo": "owner/repo#123"}); err != nil {
		t.Fatal(err)
	}
	if err = s.BindRepositories(map[string]string{}); err != nil {
		t.Fatal(err)
	}
	// A failed multi-alias update must not pin unrelated entries.
	if err = s.BindRepositories(map[string]string{"new": "owner/new#123", "repo": "owner/other#123"}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err = s.BindRepositories(map[string]string{"new": "owner/different#456"}); err != nil {
		t.Fatal("partial binding persisted", err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.BindRepositories(map[string]string{"repo": "owner/repo#123"}); err != nil {
		t.Fatal(err)
	}
	if err = s.BindRepositories(map[string]string{"repo": "owner/repo#456"}); !errors.Is(err, ErrConflict) {
		t.Fatal("installation identity changed", err)
	}
	if err = s.BindRepositories(map[string]string{"": "bad"}); err == nil {
		t.Fatal("empty alias accepted")
	}
	if err = s.BindRepositories(map[string]string{"empty": ""}); err == nil {
		t.Fatal("empty identity accepted")
	}
}
func TestRefuseUnsafeDirectoryAndDatabase(t *testing.T) {
	dir := privateDir(t)
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(dir); err == nil {
		s.Close()
		t.Fatal("permissive directory accepted")
	}
	info, _ := os.Stat(dir)
	if info.Mode().Perm() != 0755 {
		t.Fatal("changed existing directory permissions")
	}
	private := privateDir(t)
	link := privateDir(t) + "/link"
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(link); err == nil {
		s.Close()
		t.Fatal("symlink directory accepted")
	}
	db := private + "/github-botd.db"
	if err := os.WriteFile(db, []byte("sentinel"), 0644); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(private); err == nil {
		s.Close()
		t.Fatal("permissive database accepted")
	}
	bytes, err := os.ReadFile(db)
	if err != nil || string(bytes) != "sentinel" {
		t.Fatal("unsafe database modified", err)
	}
	if err = os.Chmod(db, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Link(db, private+"/second-link"); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(private); err == nil {
		s.Close()
		t.Fatal("hardlinked database accepted")
	}
}

func TestDeadLetterListingAndExplicitReplay(t *testing.T) {
	s, err := OpenWithOptions(privateDir(t), Options{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mustIngest(t, s, "e")
	d := claim(t, s, "alice")
	if err = s.Replay("alice", d.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("replayed inflight work", err)
	}
	if err = s.Ack("alice", d.ID, d.LeaseToken, true, "test exhausted"); err != nil {
		t.Fatal(err)
	}
	for _, scope := range [][]string{nil, {}, {"other/repo"}} {
		rows, err := s.ListDeadLetters("alice", scope, 100)
		if err != nil || len(rows) != 0 {
			t.Fatal("dead-letter scope leaked", rows, err)
		}
	}
	rows, err := s.ListDeadLetters("alice", []string{"o/r"}, 1)
	if err != nil || len(rows) != 1 || rows[0].ID != d.ID || rows[0].Attempts != 1 || rows[0].Error != "test exhausted" {
		t.Fatal(rows, err)
	}
	if rows, err = s.ListDeadLetters("bob", []string{"o/r"}, 100); err != nil || len(rows) != 0 {
		t.Fatal("foreign dead letter exposed", rows, err)
	}
	if _, err = s.ListDeadLetters("alice", []string{"o/r"}, 101); err == nil {
		t.Fatal("unbounded listing")
	}
	if err = s.Replay("alice", d.ID); err != nil {
		t.Fatal(err)
	}
	if rows, err = s.ListDeadLetters("alice", []string{"o/r"}, 100); err != nil || len(rows) != 0 {
		t.Fatal("replayed dead letter remains", rows, err)
	}
	next := claim(t, s, "alice")
	if next.ID != d.ID || next.Attempt != 1 || next.LeaseToken == d.LeaseToken {
		t.Fatal("bad replay", next)
	}
}
