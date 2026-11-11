package github

import (
	"encoding/json"
	v1 "github.com/wenext-limited/github-botd/api/v1"
	"regexp"
	"strings"
	"time"
)

// Capture the whole candidate before validating length/syntax, so an invalid
// long login or underscore cannot produce a valid-prefix mention. Explicit
// @app[bot] retains its suffix; @app remains "app". Policy must configure desired
// aliases explicitly instead of inferring a different account from the text.
var mentionPattern = regexp.MustCompile(`(?:^|[^A-Za-z0-9_/@-])@([A-Za-z0-9_-]+)(\[bot\])?(?:/([A-Za-z0-9_-]+))?`)
var loginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
var eventPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Normalize retains structured webhook data; it is context, never trusted instructions.
// Signature verification and delivery replay protection belong to the ingress layer.
func Normalize(eventType, deliveryID string, payload []byte) (v1.Event, error) {
	var event v1.Event
	if !eventPattern.MatchString(eventType) || deliveryID == "" || len(deliveryID) > 255 || len(payload) > maxBody {
		return event, failure("invalid webhook envelope")
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(payload, &root) != nil || root == nil {
		return event, failure("invalid webhook JSON object")
	}
	var p struct {
		Action     string `json:"action"`
		Number     int64  `json:"number"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
		Sender struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"sender"`
		RequestedReviewer struct {
			Login string `json:"login"`
		} `json:"requested_reviewer"`
		RequestedTeam struct {
			Slug string `json:"slug"`
		} `json:"requested_team"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return event, failure("invalid webhook fields")
	}
	if p.Repository.FullName != "" {
		if _, e := repoPath(p.Repository.FullName); e != nil {
			return event, e
		}
	}
	event = v1.Event{Version: v1.Version, ID: deliveryID, DeliveryID: deliveryID, Source: "webhook", Type: eventType, Action: p.Action, Repository: p.Repository.FullName, InstallationID: p.Installation.ID, Actor: p.Sender.Login, ActorType: p.Sender.Type, Number: p.Number, RequestedReviewer: p.RequestedReviewer.Login, RequestedTeam: p.RequestedTeam.Slug, Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Data: sanitize(payload)}
	// Parent objects establish PR number/head; the most specific child supplies text.
	for _, key := range []string{"issue", "pull_request", "workflow_run", "check_suite", "check_run", "review", "comment"} {
		raw := root[key]
		if len(raw) == 0 {
			continue
		}
		var o struct {
			ID          int64  `json:"id"`
			Number      int64  `json:"number"`
			Body        string `json:"body"`
			HTMLURL     string `json:"html_url"`
			HeadSHA     string `json:"head_sha"`
			InReplyToID int64  `json:"in_reply_to_id"`
			Updated     string `json:"updated_at"`
			Created     string `json:"created_at"`
			Submitted   string `json:"submitted_at"`
			Head        struct {
				SHA string `json:"sha"`
			} `json:"head"`
		}
		if json.Unmarshal(raw, &o) != nil {
			return v1.Event{}, failure("invalid webhook resource")
		}
		if o.Number > 0 {
			event.Number = o.Number
		}
		if o.Head.SHA != "" {
			event.HeadSHA = o.Head.SHA
		}
		if o.HeadSHA != "" {
			event.HeadSHA = o.HeadSHA
		}
		if key == "issue" || key == "pull_request" || key == "review" || key == "comment" {
			event.Body = o.Body
		}
		if key == "check_run" {
			event.CheckRunID = o.ID
		}
		if key == "workflow_run" {
			event.WorkflowRunID = o.ID
		}
		if o.HTMLURL != "" {
			event.URL = o.HTMLURL
		}
		if key == "comment" {
			event.CommentID = o.ID
			event.ThreadID = o.InReplyToID
			if event.ThreadID == 0 {
				event.ThreadID = o.ID
			}
		}
		for _, t := range []string{o.Created, o.Submitted, o.Updated} {
			if _, e := time.Parse(time.RFC3339, t); e == nil {
				event.Timestamp = t
			}
		}
	}
	seen := map[string]bool{}
	for _, m := range mentionPattern.FindAllStringSubmatch(event.Body, -1) {
		if !loginPattern.MatchString(m[1]) {
			continue
		}
		name := m[1] + m[2]
		if m[3] != "" {
			name += "/" + m[3]
		}
		key := strings.ToLower(name)
		if !seen[key] {
			event.Mentioned = append(event.Mentioned, name)
			seen[key] = true
		}
	}
	return event, nil
}
