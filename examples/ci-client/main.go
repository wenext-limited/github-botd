// ci-client never runs scripts: it consumes CI events or explicitly publishes a
// caller-provided completed check result. A published result is not verification.
package main

import (
	"context"
	"flag"
	"fmt"
	v1 "github.com/wenext-limited/github-botd/api/v1"
	"github.com/wenext-limited/github-botd/pkg/client"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	endpoint := flag.String("endpoint", client.DefaultEndpoint, "local gateway")
	publish := flag.Bool("publish", false, "publish caller-provided completed check instead of consuming events")
	repo := flag.String("repo", "", "configured repository alias")
	sha := flag.String("sha", "", "commit SHA")
	event := flag.String("event-id", "", "correlating delivered event ID (unless actions.proactive allowed)")
	key := flag.String("operation-id", "", "stable caller-chosen operation ID, required for publish")
	name := flag.String("name", "demo-ci", "check name")
	conclusion := flag.String("conclusion", "", "explicit supplied result: success, failure, neutral, cancelled, skipped, timed_out, action_required")
	summary := flag.String("summary", "", "caller-provided check summary")
	flag.Parse()
	c, e := client.New(client.Config{Endpoint: *endpoint})
	if e != nil {
		log.Fatal(e)
	}
	defer c.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *publish {
		if *repo == "" || *sha == "" || *key == "" || *summary == "" {
			log.Fatal("publish requires repo, sha, operation-id, summary and conclusion")
		}
		switch *conclusion {
		case "success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required":
		default:
			log.Fatal("valid explicit conclusion required")
		}
		r, err := c.Submit(ctx, v1.Action{Kind: "check.create", Repository: *repo, EventID: *event, HeadSHA: *sha, Name: *name, Status: "completed", Conclusion: *conclusion, Summary: "Caller-supplied result; this client did not execute CI.\n\n" + *summary, IdempotencyKey: client.StableKey("ci-result-v1", *repo, *sha, *name, *key)})
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("receipt=%s state=%s\n", r.ID, r.State)
		return
	}
	e = c.Subscribe(ctx, func(_ context.Context, d v1.Delivery) error {
		switch d.Event.Type {
		case "workflow_run", "workflow_job", "check_run", "check_suite":
			fmt.Printf("CI event=%q type=%q repo=%q action=%q\n", d.Event.ID, d.Event.Type, d.Event.Repository, d.Event.Action)
		}
		return nil
	})
	if e != nil && ctx.Err() == nil {
		log.Fatal(e)
	}
}
