package github

import (
	"context"
	"encoding/json"
	"fmt"
	v1 "github.com/wenext-limited/github-botd/api/v1"
	"strings"
)

// PollRepository polls the repository activity feed, NOT an exhaustive webhook
// substitute: GitHub delays/limits this feed and does not provide complete review
// request or CI history. First poll bootstraps the newest page. Subsequent polls
// stop at the last observed event ID, traversing at most 10 pages. A missing
// cursor is an explicit gap error; callers must decide whether to re-bootstrap.
// Persist returned events and cursor atomically. Event IDs are deterministic so
// a crash/re-poll can safely deduplicate. No internal state is changed here.
func (c *Client) PollRepository(ctx context.Context, installationID int64, repository, cursor string) ([]v1.Event, string, error) {
	base, e := repoPath(repository)
	if e != nil {
		return nil, cursor, e
	}
	if len(cursor) > 255 {
		return nil, cursor, failure("invalid polling cursor")
	}
	token, e := c.credential(ctx, installationID)
	if e != nil {
		return nil, cursor, e
	}
	var out []v1.Event
	next := cursor
	found := false
	for page := 1; page <= 10; page++ {
		b, h, e := c.request(ctx, "GET", fmt.Sprintf("%s/events?per_page=100&page=%d", base, page), token, nil, false)
		if e != nil {
			return nil, cursor, e
		}
		var rows []struct {
			ID      string                     `json:"id"`
			Type    string                     `json:"type"`
			Actor   json.RawMessage            `json:"actor"`
			Payload map[string]json.RawMessage `json:"payload"`
			Created string                     `json:"created_at"`
		}
		if json.Unmarshal(b, &rows) != nil {
			return nil, cursor, failure("invalid repository events response")
		}
		for _, r := range rows {
			if r.ID == "" {
				return nil, cursor, failure("repository event has no ID")
			}
			if page == 1 && next == cursor {
				next = r.ID
			}
			if r.ID == cursor {
				found = true
				break
			}
			kind := pollEventType(r.Type)
			if kind == "" {
				continue
			}
			if r.Payload == nil {
				r.Payload = map[string]json.RawMessage{}
			}
			r.Payload["repository"], _ = json.Marshal(map[string]string{"full_name": repository})
			r.Payload["installation"], _ = json.Marshal(map[string]int64{"id": installationID})
			r.Payload["sender"] = r.Actor
			payload, _ := json.Marshal(r.Payload)
			event, e := Normalize(kind, "poll:"+repository+":"+r.ID, payload)
			if e != nil {
				return nil, cursor, e
			}
			event.Source = "poll"
			if r.Created != "" {
				event.Timestamp = r.Created
			}
			out = append(out, event)
		}
		if cursor == "" || found {
			break
		}
		if !strings.Contains(h.Get("Link"), `rel="next"`) {
			break
		}
	}
	if cursor != "" && !found {
		return nil, cursor, failure("polling history gap: cursor absent from bounded repository feed")
	}
	// Deliver oldest first; the feed itself is newest first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, next, nil
}
func pollEventType(t string) string {
	switch t {
	case "PullRequestEvent":
		return "pull_request"
	case "PullRequestReviewEvent":
		return "pull_request_review"
	case "PullRequestReviewCommentEvent":
		return "pull_request_review_comment"
	case "IssueCommentEvent":
		return "issue_comment"
	case "IssuesEvent":
		return "issues"
	case "PushEvent":
		return "push"
	case "CreateEvent":
		return "create"
	case "DeleteEvent":
		return "delete"
	case "ReleaseEvent":
		return "release"
	case "CommitCommentEvent":
		return "commit_comment"
	case "ForkEvent":
		return "fork"
	case "WatchEvent":
		return "watch"
	case "MemberEvent":
		return "member"
	case "PublicEvent":
		return "public"
	case "GollumEvent":
		return "gollum"
	}
	return ""
}
