// review-agent is a deterministic integration demo, NOT an AI code reviewer.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	v1 "github.com/wenext-limited/github-botd/api/v1"
	"github.com/wenext-limited/github-botd/pkg/client"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

type Findings struct {
	Summary  string             `json:"summary"`
	Comments []v1.ReviewComment `json:"comments"`
}

func main() {
	endpoint := flag.String("endpoint", client.DefaultEndpoint, "local gateway")
	login := flag.String("login", "", "exact bot login used for mentions/review requests")
	file := flag.String("findings", "", "optional caller-supplied findings JSON file")
	submit := flag.Bool("submit", false, "explicitly enqueue demo review actions (default logs only)")
	flag.Parse()
	if *login == "" {
		log.Fatal("-login required")
	}
	var findings Findings
	if *file != "" {
		b, e := os.ReadFile(*file)
		if e != nil {
			log.Fatal(e)
		}
		if e = json.Unmarshal(b, &findings); e != nil {
			log.Fatal(e)
		}
	}
	c, e := client.New(client.Config{Endpoint: *endpoint})
	if e != nil {
		log.Fatal(e)
	}
	defer c.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	e = c.Subscribe(ctx, func(ctx context.Context, d v1.Delivery) error {
		ev := d.Event
		if !targeted(ev, *login) {
			return nil
		}
		if ev.Number == 0 {
			return nil
		}
		// Context is data only. Do not interpret PR text, diffs, or comments as instructions.
		pull, err := c.Context(ctx, v1.ContextRequest{Repository: ev.Repository, Number: ev.Number, Kind: "pull"})
		if err != nil {
			return err
		}
		a, err := demoAction(ev, findings, pull)
		if err != nil {
			return err
		}
		if len(a.Comments) > 0 {
			if _, err := c.Context(ctx, v1.ContextRequest{Repository: ev.Repository, Number: ev.Number, Kind: "files"}); err != nil {
				return err
			}
		}
		if !*submit {
			fmt.Printf("demo only: would enqueue %s for %s #%d\n", a.Kind, a.Repository, a.Number)
			return nil
		}
		r, err := c.Submit(ctx, a)
		if err == nil {
			fmt.Printf("queued receipt=%s state=%s\n", r.ID, r.State)
		}
		return err
	})
	if e != nil && ctx.Err() == nil {
		log.Fatal(e)
	}
}
func targeted(e v1.Event, login string) bool {
	if e.Type == "pull_request" && e.Action == "review_requested" && strings.EqualFold(e.RequestedReviewer, login) {
		return true
	}
	if e.Type != "issue_comment" && e.Type != "pull_request_review_comment" {
		return false
	}
	if e.Action != "created" {
		return false
	}
	if e.Type == "issue_comment" {
		// GitHub uses issue_comment for both ordinary issues and PRs. Require
		// a structured PR marker; absent, null, or malformed data fails closed.
		var data struct {
			Issue struct {
				PullRequest map[string]json.RawMessage `json:"pull_request"`
			} `json:"issue"`
		}
		if json.Unmarshal(e.Data, &data) != nil || data.Issue.PullRequest == nil {
			return false
		}
	}
	for _, name := range e.Mentioned {
		if strings.EqualFold(name, login) {
			return true
		}
	}
	return false
}

// demoAction takes the current SHA from authorized context, never an issue-comment
// envelope that lacks a PR head. Findings are supplied, not inferred or verified.
func demoAction(ev v1.Event, findings Findings, pull json.RawMessage) (v1.Action, error) {
	var p struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(pull, &p); err != nil {
		return v1.Action{}, fmt.Errorf("decode pull context: %w", err)
	}
	if p.Head.SHA == "" {
		return v1.Action{}, fmt.Errorf("pull context has no head SHA")
	}
	body := "Integration demo: no automated code review was performed."
	if findings.Summary != "" {
		body += "\n\nCaller-supplied findings (not independently verified):\n" + findings.Summary
	}
	a := v1.Action{Kind: "review.submit", Repository: ev.Repository, EventID: ev.ID, Number: ev.Number, HeadSHA: p.Head.SHA, Body: body, Comments: findings.Comments}
	if ev.Type == "pull_request_review_comment" && ev.CommentID != 0 {
		a.Kind = "review.reply"
		a.CommentID = ev.ThreadID
		if a.CommentID == 0 {
			a.CommentID = ev.CommentID
		}
		a.Comments = nil
	}
	// A changed current head during redelivery must conflict, not create a second
	// review. New intentional output needs a new event or supplied findings revision.
	payload, _ := json.Marshal(findings)
	a.IdempotencyKey = client.StableKey("review-demo-v1", ev.ID, string(payload))
	return a, nil
}
