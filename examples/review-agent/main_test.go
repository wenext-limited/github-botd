package main

import (
	v1 "github.com/wenext-limited/github-botd/api/v1"
	"testing"
)

func TestTargeted(t *testing.T) {
	for _, tc := range []struct {
		e    v1.Event
		want bool
	}{{v1.Event{Type: "pull_request", Action: "opened"}, false}, {v1.Event{Type: "issue_comment", Action: "created", Mentioned: []string{"demo"}, Data: []byte(`{"issue":{"pull_request":{}}}`)}, true}, {v1.Event{Type: "issue_comment", Action: "edited", Mentioned: []string{"demo"}}, false}, {v1.Event{Type: "pull_request", Action: "review_requested", RequestedReviewer: "demo"}, true}, {v1.Event{Type: "pull_request", Action: "review_requested", RequestedReviewer: "other"}, false}} {
		if got := targeted(tc.e, "demo"); got != tc.want {
			t.Errorf("%+v got %v", tc.e, got)
		}
	}
}

func TestDemoActionUsesContextHeadAndRootThread(t *testing.T) {
	e := v1.Event{ID: "event", Type: "issue_comment", HeadSHA: "stale", Number: 42}
	a, err := demoAction(e, Findings{}, []byte(`{"head":{"sha":"current"}}`))
	if err != nil || a.HeadSHA != "current" || a.Kind != "review.submit" {
		t.Fatalf("%+v %v", a, err)
	}
	b, err := demoAction(e, Findings{}, []byte(`{"head":{"sha":"next"}}`))
	if err != nil || a.IdempotencyKey != b.IdempotencyKey {
		t.Fatal("head change created new retry identity")
	}
	e.Type = "pull_request_review_comment"
	e.CommentID = 8
	e.ThreadID = 7
	a, err = demoAction(e, Findings{}, []byte(`{"head":{"sha":"current"}}`))
	if err != nil || a.Kind != "review.reply" || a.CommentID != 7 {
		t.Fatalf("%+v %v", a, err)
	}
	e.ThreadID = 0
	a, err = demoAction(e, Findings{}, []byte(`{"head":{"sha":"current"}}`))
	if err != nil || a.CommentID != 8 {
		t.Fatalf("%+v %v", a, err)
	}
	if _, err = demoAction(e, Findings{}, []byte(`{}`)); err == nil {
		t.Fatal("missing head accepted")
	}
}

func TestTargetedRejectsOrdinaryIssuesAndMissingContext(t *testing.T) {
	for _, raw := range []string{"", "null", "{}", `{"issue":{}}`, `{"issue":{"pull_request":null}}`, `{"issue":{"pull_request":"not-an-object"}}`, "malformed"} {
		e := v1.Event{Type: "issue_comment", Action: "created", Mentioned: []string{"demo"}, Data: []byte(raw)}
		if targeted(e, "demo") {
			t.Errorf("ordinary or unconfirmed PR accepted: %q", raw)
		}
	}
	e := v1.Event{Type: "pull_request_review_comment", Action: "created", Mentioned: []string{"demo"}}
	if !targeted(e, "demo") {
		t.Fatal("review comments do not need issue marker")
	}
}
