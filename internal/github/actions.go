package github

import (
	"context"
	"encoding/json"
	"fmt"
	v1 "github.com/wenext-limited/github-botd/api/v1"
	"net/url"
	"regexp"
	"strings"
)

var shaPattern = regexp.MustCompile(`^([a-fA-F0-9]{40}|[a-fA-F0-9]{64})$`)

func validBody(s string) bool { return strings.TrimSpace(s) != "" && len(s) <= 65536 }
func validInline(r v1.ReviewComment) bool {
	return validBody(r.Body) && r.Path != "" && len(r.Path) <= 4096 && r.Line > 0 && (r.Side == "LEFT" || r.Side == "RIGHT")
}

// ValidateAction validates the adapter's explicit mutation allowlist without network I/O.
func ValidateAction(a v1.Action) error {
	if _, e := repoPath(a.Repository); e != nil {
		return e
	}
	if a.Version != "" && a.Version != v1.Version {
		return failure("unsupported action version")
	}
	switch a.Kind {
	case "issue.comment.create", "review.reply", "review.comment", "review.submit":
		if a.Number <= 0 {
			return failure("positive issue or pull number required")
		}
	case "issue.comment.update":
		if a.CommentID <= 0 || a.Number <= 0 {
			return failure("positive comment ID and issue number required")
		}
	case "check.create", "check.update":
	default:
		return failure("unsupported GitHub mutation")
	}
	switch a.Kind {
	case "issue.comment.create", "issue.comment.update", "review.reply":
		if !validBody(a.Body) {
			return failure("nonempty bounded body required")
		}
		if a.Kind == "review.reply" && a.CommentID <= 0 {
			return failure("positive reply comment ID required")
		}
	case "review.comment":
		if !shaPattern.MatchString(a.HeadSHA) || !validInline(v1.ReviewComment{Body: a.Body, Path: a.Path, Line: a.Line, Side: a.Side}) {
			return failure("valid commit SHA and inline comment required")
		}
	case "review.submit":
		if len(a.Comments) > 100 || (!validBody(a.Body) && len(a.Comments) == 0) {
			return failure("review requires bounded body or comments")
		}
		if a.Body != "" && !validBody(a.Body) {
			return failure("invalid review body")
		}
		if !shaPattern.MatchString(a.HeadSHA) {
			return failure("invalid commit SHA")
		}
		for _, r := range a.Comments {
			if !validInline(r) {
				return failure("invalid review inline comment")
			}
		}
	case "check.create", "check.update":
		if a.Kind == "check.create" && (a.Name == "" || !shaPattern.MatchString(a.HeadSHA)) {
			return failure("check name and commit SHA required")
		}
		if a.Kind == "check.update" && a.CheckRunID <= 0 {
			return failure("positive check run ID required")
		}
		if len(a.Name) > 255 || len(a.Summary) > 65535 {
			return failure("check output too large")
		}
		if a.Status != "" && a.Status != "queued" && a.Status != "in_progress" && a.Status != "completed" {
			return failure("invalid check status")
		}
		if a.Conclusion != "" {
			switch a.Conclusion {
			case "success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required":
			default:
				return failure("invalid check conclusion")
			}
			if a.Status != "completed" {
				return failure("conclusion requires completed status")
			}
		}
		if a.Status == "completed" && a.Conclusion == "" {
			return failure("completed check requires conclusion")
		}
		if a.DetailsURL != "" {
			u, e := url.Parse(a.DetailsURL)
			if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
				return failure("check details URL must be HTTPS")
			}
		}
	}
	return nil
}
func (c *Client) Execute(ctx context.Context, installationID int64, a v1.Action) (json.RawMessage, error) {
	if e := ValidateAction(a); e != nil {
		return nil, e
	}
	if (a.Kind == "check.create" || a.Kind == "check.update") && (installationID <= 0 || c.apps[installationID] == nil) {
		return nil, failure("check writes require a configured GitHub App installation")
	}
	token, e := c.credential(ctx, installationID)
	if e != nil {
		return nil, e
	}
	base, _ := repoPath(a.Repository)
	if a.Kind == "review.comment" || a.Kind == "review.submit" {
		b, _, err := c.request(ctx, "GET", fmt.Sprintf("%s/pulls/%d", base, a.Number), token, nil, false)
		if err != nil {
			return nil, err
		}
		var pr struct {
			Head struct {
				SHA string `json:"sha"`
			} `json:"head"`
		}
		if json.Unmarshal(b, &pr) != nil || !strings.EqualFold(pr.Head.SHA, a.HeadSHA) {
			return nil, failure("pull request head changed; refresh context before reviewing")
		}
	}

	method := "POST"
	path := ""
	p := map[string]any{}
	switch a.Kind {
	case "issue.comment.create":
		path = fmt.Sprintf("%s/issues/%d/comments", base, a.Number)
		p["body"] = a.Body
	case "issue.comment.update":
		path = fmt.Sprintf("%s/issues/comments/%d", base, a.CommentID)
		b, _, e := c.request(ctx, "GET", path, token, nil, false)
		if e != nil {
			return nil, e
		}
		var comment struct {
			IssueURL string `json:"issue_url"`
			User     struct {
				Login string `json:"login"`
				ID    int64  `json:"id"`
			} `json:"user"`
		}
		if json.Unmarshal(b, &comment) != nil {
			return nil, failure("invalid comment ownership response")
		}
		expectedIssueURL := c.base + fmt.Sprintf("%s/issues/%d", base, a.Number)
		if comment.IssueURL != expectedIssueURL {
			return nil, failure("comment does not belong to the authorized issue")
		}
		if installationID == 0 {
			u, _, e := c.request(ctx, "GET", "/user", token, nil, false)
			if e != nil {
				return nil, e
			}
			var identity struct {
				ID int64 `json:"id"`
			}
			if json.Unmarshal(u, &identity) != nil || identity.ID <= 0 || identity.ID != comment.User.ID {
				return nil, failure("comment is not owned by authenticated user")
			}
		} else {
			owner := c.apps[installationID].login
			if owner == "" || !strings.EqualFold(owner, comment.User.Login) {
				return nil, failure("comment ownership does not match configured app bot")
			}
		}
		method = "PATCH"
		p["body"] = a.Body
	case "review.reply":
		path = fmt.Sprintf("%s/pulls/%d/comments/%d/replies", base, a.Number, a.CommentID)
		p["body"] = a.Body
	case "review.comment":
		path = fmt.Sprintf("%s/pulls/%d/comments", base, a.Number)
		p = map[string]any{"body": a.Body, "commit_id": a.HeadSHA, "path": a.Path, "line": a.Line, "side": a.Side}
	case "review.submit":
		path = fmt.Sprintf("%s/pulls/%d/reviews", base, a.Number)
		p = map[string]any{"event": "COMMENT", "body": a.Body}
		if a.HeadSHA != "" {
			p["commit_id"] = a.HeadSHA
		}
		if len(a.Comments) > 0 {
			p["comments"] = a.Comments
		}
	case "check.create", "check.update":
		if a.Kind == "check.update" {
			existing, _, err := c.request(ctx, "GET", fmt.Sprintf("%s/check-runs/%d", base, a.CheckRunID), token, nil, false)
			if err != nil {
				return nil, err
			}
			var check struct {
				App struct {
					ID int64 `json:"id"`
				} `json:"app"`
			}
			if json.Unmarshal(existing, &check) != nil || check.App.ID != c.apps[installationID].id {
				return nil, failure("check run is not owned by the configured GitHub App")
			}
		}
		path = base + "/check-runs"
		if a.Kind == "check.update" {
			method = "PATCH"
			path += fmt.Sprintf("/%d", a.CheckRunID)
		} else {
			p["head_sha"] = a.HeadSHA
		}
		if a.Name != "" {
			p["name"] = a.Name
		}
		if a.Status != "" {
			p["status"] = a.Status
		}
		if a.Conclusion != "" {
			p["conclusion"] = a.Conclusion
		}
		if a.DetailsURL != "" {
			p["details_url"] = a.DetailsURL
		}
		if a.Summary != "" {
			title := a.Name
			if title == "" {
				title = "Bot result"
			}
			p["output"] = map[string]string{"title": title, "summary": a.Summary}
		}
	}
	b, _, e := c.request(ctx, method, path, token, p, true)
	return b, e
}

// Context reads fixed endpoints. List responses are fully paginated with explicit
// bounds; oversized collections fail rather than silently returning partial data.
func (c *Client) Context(ctx context.Context, installationID int64, r v1.ContextRequest) (json.RawMessage, error) {
	base, e := repoPath(r.Repository)
	if e != nil {
		return nil, e
	}
	path := ""
	list := false
	switch r.Kind {
	case "pull", "files", "issue_comments", "review_comments", "reviews":
		if r.Number <= 0 {
			return nil, failure("positive number required")
		}
		path = fmt.Sprintf("%s/pulls/%d", base, r.Number)
		switch r.Kind {
		case "files":
			path += "/files"
			list = true
		case "issue_comments":
			path = fmt.Sprintf("%s/issues/%d/comments", base, r.Number)
			list = true
		case "review_comments":
			path += "/comments"
			list = true
		case "reviews":
			path += "/reviews"
			list = true
		}
	case "workflow_run", "check_run":
		if r.ID <= 0 {
			return nil, failure("positive resource ID required")
		}
		if r.Kind == "workflow_run" {
			path = fmt.Sprintf("%s/actions/runs/%d", base, r.ID)
		} else {
			path = fmt.Sprintf("%s/check-runs/%d", base, r.ID)
		}
	default:
		return nil, failure("unsupported context kind")
	}
	token, e := c.credential(ctx, installationID)
	if e != nil {
		return nil, e
	}
	if !list {
		b, _, e := c.request(ctx, "GET", path, token, nil, false)
		return b, e
	}
	all := []json.RawMessage{}
	total := 0
	for page := 1; page <= 100; page++ {
		b, h, e := c.request(ctx, "GET", fmt.Sprintf("%s?per_page=100&page=%d", path, page), token, nil, false)
		if e != nil {
			return nil, e
		}
		var rows []json.RawMessage
		if json.Unmarshal(b, &rows) != nil {
			return nil, failure("invalid GitHub list response")
		}
		all = append(all, rows...)
		total += len(b)
		if total > maxBody {
			return nil, failure("GitHub context exceeds response limit")
		}
		// Never follow an upstream Link URL, which could leak the credential to a different host.
		if !strings.Contains(h.Get("Link"), `rel="next"`) {
			return json.Marshal(all)
		}
	}
	return nil, failure("GitHub pagination exceeds page limit")
}
