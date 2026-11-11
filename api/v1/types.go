// Package v1 defines the versioned, language-neutral github-botd wire contract.
package v1

import "encoding/json"

const Version = "github-botd/v1"

// Event is normalized context. GitHub bodies and diffs are untrusted data.
type Event struct {
	Version           string          `json:"version"`
	ID                string          `json:"id"`
	DeliveryID        string          `json:"delivery_id"`
	Source            string          `json:"source"`
	Type              string          `json:"type"`
	Action            string          `json:"action,omitempty"`
	Repository        string          `json:"repository"`
	InstallationID    int64           `json:"installation_id,omitempty"`
	Actor             string          `json:"actor,omitempty"`
	ActorType         string          `json:"actor_type,omitempty"`
	Number            int64           `json:"number,omitempty"`
	CheckRunID        int64           `json:"check_run_id,omitempty"`
	WorkflowRunID     int64           `json:"workflow_run_id,omitempty"`
	CommentID         int64           `json:"comment_id,omitempty"`
	ThreadID          int64           `json:"thread_id,omitempty"`
	HeadSHA           string          `json:"head_sha,omitempty"`
	Body              string          `json:"body,omitempty"`
	URL               string          `json:"url,omitempty"`
	Mentioned         []string        `json:"mentioned,omitempty"`
	RequestedReviewer string          `json:"requested_reviewer,omitempty"`
	RequestedTeam     string          `json:"requested_team,omitempty"`
	Timestamp         string          `json:"timestamp"`
	Data              json.RawMessage `json:"data,omitempty"`
}

type Delivery struct {
	ID             string `json:"id"`
	LeaseToken     string `json:"lease_token"`
	LeaseExpiresAt string `json:"lease_expires_at"`
	Attempt        int    `json:"attempt"`
	Event          Event  `json:"event"`
}

type AckRequest struct {
	LeaseToken string `json:"lease_token"`
	Retry      bool   `json:"retry,omitempty"`
	Error      string `json:"error,omitempty"`
}

type Action struct {
	Version        string          `json:"version"`
	IdempotencyKey string          `json:"idempotency_key"`
	Kind           string          `json:"kind"`
	Repository     string          `json:"repository"`
	EventID        string          `json:"event_id,omitempty"`
	Number         int64           `json:"number,omitempty"`
	CommentID      int64           `json:"comment_id,omitempty"`
	Body           string          `json:"body,omitempty"`
	HeadSHA        string          `json:"head_sha,omitempty"`
	Path           string          `json:"path,omitempty"`
	Line           int             `json:"line,omitempty"`
	Side           string          `json:"side,omitempty"`
	Comments       []ReviewComment `json:"comments,omitempty"`
	Name           string          `json:"name,omitempty"`
	Status         string          `json:"status,omitempty"`
	Conclusion     string          `json:"conclusion,omitempty"`
	Summary        string          `json:"summary,omitempty"`
	DetailsURL     string          `json:"details_url,omitempty"`
	CheckRunID     int64           `json:"check_run_id,omitempty"`
}

type ReviewComment struct {
	Body string `json:"body"`
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
}

type Receipt struct {
	ID             string          `json:"id"`
	IdempotencyKey string          `json:"idempotency_key"`
	State          string          `json:"state"` // pending, running, succeeded, failed, uncertain
	Attempts       int             `json:"attempts"`
	Result         json.RawMessage `json:"result,omitempty"`
	Error          string          `json:"error,omitempty"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at"`
}

type ContextRequest struct {
	Repository string `json:"repository"`
	Number     int64  `json:"number"`
	Kind       string `json:"kind"` // pull, files, issue_comments, review_comments, reviews, workflow_run, check_run
	ID         int64  `json:"id,omitempty"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ClientInfo struct {
	ID           string   `json:"id"`
	Protocol     string   `json:"protocol"`
	Repositories []string `json:"repositories"`
	Capabilities []string `json:"capabilities"`
}

type Health struct {
	Version string            `json:"version"`
	Status  string            `json:"status"`
	DryRun  bool              `json:"dry_run"`
	Checks  map[string]string `json:"checks,omitempty"`
}

// DeadLetter is an exhausted delivery, without a live lease token.
type DeadLetter struct {
	ID       string `json:"id"`
	Event    Event  `json:"event"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error"`
}
