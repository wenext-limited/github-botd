package gateway

import (
	v1 "github.com/wenext-limited/github-botd/api/v1"
	"github.com/wenext-limited/github-botd/internal/config"
	"sort"
	"strings"
)

// Route is deterministic fan-out, not work stealing. Each client owns an independent queue.
func Route(c *config.Config, e v1.Event) []string {
	result := map[string]bool{}
	for _, r := range c.Routes {
		if !config.Contains(r.Repositories, e.Repository) || (!config.Contains(r.Events, e.Type) && !config.Contains(r.Events, "*")) {
			continue
		}
		if len(r.Actions) > 0 && !config.Contains(r.Actions, e.Action) {
			continue
		}
		if !r.IncludeBots && (strings.EqualFold(e.ActorType, "Bot") || strings.HasSuffix(strings.ToLower(e.Actor), "[bot]")) {
			continue
		}
		if !containsFold(r.Actors, "*") && !containsFold(r.Actors, e.Actor) {
			continue
		}
		matched := r.Trigger == "any"
		if r.Trigger == "mention" {
			for _, m := range e.Mentioned {
				if containsFold(r.Names, m) {
					matched = true
				}
			}
		}
		if r.Trigger == "review_requested" && e.Type == "pull_request" && e.Action == "review_requested" {
			matched = containsFold(r.Names, e.RequestedReviewer) || containsFold(r.Names, e.RequestedTeam)
		}
		if !matched {
			continue
		}
		for _, id := range r.Clients {
			p, ok := c.Clients[id]
			if ok && config.Contains(p.Repositories, e.Repository) && config.Contains(p.Capabilities, "events.read") {
				result[id] = true
			}
		}
	}
	out := make([]string, 0, len(result))
	for id := range result {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
func containsFold(xs []string, s string) bool {
	if s == "" {
		return false
	}
	for _, x := range xs {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}
