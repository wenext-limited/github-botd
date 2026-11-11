package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestNormalizeFixtures(t *testing.T) {
	fixtures := []struct {
		kind, payload           string
		number, comment, thread int64
		body, reviewer, head    string
	}{
		{"pull_request", `{"action":"review_requested","number":4,"pull_request":{"number":4,"body":"Please @bot review","head":{"sha":"abc"}},"requested_reviewer":{"login":"bot"}}`, 4, 0, 0, "Please @bot review", "bot", "abc"},
		{"issue_comment", `{"action":"created","issue":{"number":5,"body":"parent"},"comment":{"id":6,"body":"@Bot ping @bot and mail@example.com @org/team","created_at":"2026-01-01T12:00:00Z"}}`, 5, 6, 6, "@Bot ping @bot and mail@example.com @org/team", "", ""},
		{"pull_request_review_comment", `{"action":"created","pull_request":{"number":5,"head":{"sha":"abc"}},"comment":{"id":7,"in_reply_to_id":6,"body":"reply"}}`, 5, 7, 6, "reply", "", "abc"},
		{"pull_request_review", `{"action":"submitted","pull_request":{"number":5},"review":{"id":9,"body":"review"}}`, 5, 0, 0, "review", "", ""},
		{"workflow_run", `{"action":"completed","workflow_run":{"id":8,"head_sha":"abc","conclusion":"success"}}`, 0, 0, 0, "", "", "abc"},
		{"check_run", `{"action":"completed","check_run":{"id":8,"head_sha":"abc","conclusion":"success"}}`, 0, 0, 0, "", "", "abc"},
		{"push", `{"ref":"refs/heads/main","commits":[{"message":"structured"}]}`, 0, 0, 0, "", "", ""},
	}
	for _, f := range fixtures {
		t.Run(f.kind, func(t *testing.T) {
			var m map[string]any
			json.Unmarshal([]byte(f.payload), &m)
			m["repository"] = map[string]string{"full_name": "o/r"}
			m["installation"] = map[string]int64{"id": 123}
			m["sender"] = map[string]string{"login": "example[bot]", "type": "Bot"}
			p, _ := json.Marshal(m)
			e, err := Normalize(f.kind, "delivery-1", p)
			if err != nil {
				t.Fatal(err)
			}
			if e.Number != f.number || e.CommentID != f.comment || e.ThreadID != f.thread || e.Body != f.body || e.RequestedReviewer != f.reviewer || e.HeadSHA != f.head || e.ActorType != "Bot" || e.InstallationID != 123 {
				t.Fatalf("%+v", e)
			}
			if f.kind == "issue_comment" && strings.Join(e.Mentioned, ",") != "Bot,org/team" {
				t.Fatal(e.Mentioned)
			}
			if !json.Valid(e.Data) {
				t.Fatal("structured payload lost")
			}
		})
	}
}
func TestNormalizeInvalid(t *testing.T) {
	for _, p := range []string{`null`, `[]`, `bad`, `{"comment":"oops"}`, `{"repository":{"full_name":"../repo"}}`} {
		if _, e := Normalize("issue_comment", "id", []byte(p)); e == nil {
			t.Fatal(p)
		}
	}
	if _, e := Normalize("../evil", "id", []byte(`{}`)); e == nil {
		t.Fatal("bad event type")
	}
}
func TestPollingFixturesAndGap(t *testing.T) {
	c := machine(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("Link", `<https://evil.example/x>; rel="next"`)
			fmt.Fprint(w, `[{"id":"3","type":"IssueCommentEvent","created_at":"2026-01-02T00:00:00Z","actor":{"login":"alice"},"payload":{"action":"created","issue":{"number":4},"comment":{"id":8,"body":"@bot hi"}}},{"id":"2","type":"PullRequestEvent","actor":{"login":"bob"},"payload":{"action":"opened","number":4,"pull_request":{"number":4}}}]`)
		} else {
			fmt.Fprint(w, `[{"id":"1","type":"PushEvent","actor":{"login":"bob"},"payload":{"ref":"refs/heads/main"}}]`)
		}
	})
	events, next, e := c.PollRepository(context.Background(), 0, "o/r", "1")
	if e != nil || next != "3" || len(events) != 2 || events[0].ID != "poll:o/r:2" || events[1].Body != "@bot hi" || events[1].Source != "poll" {
		t.Fatalf("%+v %s %v", events, next, e)
	}
	events, next, e = c.PollRepository(context.Background(), 0, "o/r", "3")
	if e != nil || len(events) != 0 || next != "3" {
		t.Fatal(events, next, e)
	}
	events, next, e = c.PollRepository(context.Background(), 0, "o/r", "missing")
	if e == nil || events != nil || next != "missing" {
		t.Fatal(events, next, e)
	}
	events, next, e = c.PollRepository(context.Background(), 0, "o/r", "")
	if e != nil || len(events) != 2 || next != "3" {
		t.Fatal(events, next, e)
	}
}
func TestStructuredNumbersPreserved(t *testing.T) {
	event, e := Normalize("push", "d", []byte(`{"repository":{"full_name":"o/r"},"custom":{"id":9007199254740993}}`))
	if e != nil || !strings.Contains(string(event.Data), "9007199254740993") {
		t.Fatal(string(event.Data), e)
	}
}

func TestNormalizedRunIDs(t *testing.T) {
	for _, tc := range []struct {
		kind            string
		check, workflow int64
	}{{"check_run", 123, 0}, {"workflow_run", 0, 123}} {
		e, err := Normalize(tc.kind, "d", []byte(fmt.Sprintf(`{"repository":{"full_name":"o/r"},%q:{"id":123}}`, tc.kind)))
		if err != nil || e.CheckRunID != tc.check || e.WorkflowRunID != tc.workflow {
			t.Fatalf("%+v %v", e, err)
		}
	}
}
func TestEmptyChildBodyDoesNotInheritMention(t *testing.T) {
	for _, child := range []string{"comment", "review"} {
		for _, body := range []string{`""`, `null`} {
			e, err := Normalize("issue_comment", "d", []byte(fmt.Sprintf(`{"repository":{"full_name":"o/r"},"issue":{"number":1,"body":"@bot parent mention"},%q:{"id":9,"body":%s}}`, child, body)))
			if err != nil || e.Body != "" || len(e.Mentioned) != 0 {
				t.Fatalf("%+v %v", e, err)
			}
		}
	}
}
func TestMentionBoundariesAndBotAlias(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"@bot-extra", "bot-extra"}, {"@bot_extra", ""}, {"x@bot", ""}, {"mail@example.com", ""},
		{"@" + strings.Repeat("a", 40), ""}, {"@bot-", ""}, {"@bot[bot] @bot", "bot[bot],bot"},
		{"hello @org/team and @Other", "org/team,Other"}, {"@bot @bot-extra @bot", "bot,bot-extra"},
	} {
		payload, _ := json.Marshal(map[string]any{"comment": map[string]any{"body": tc.body}})
		e, err := Normalize("issue_comment", "d", payload)
		if err != nil || strings.Join(e.Mentioned, ",") != tc.want {
			t.Fatalf("%q: %+v %v", tc.body, e.Mentioned, err)
		}
	}
}
