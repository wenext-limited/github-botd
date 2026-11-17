package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	v1 "github.com/wenext-limited/github-botd/api/v1"
)

func validateMerge(a v1.Action) error {
	if a.Merge == nil || a.RepositoryID <= 0 || a.Number <= 0 || !shaPattern.MatchString(a.HeadSHA) ||
		!shaPattern.MatchString(a.Merge.BaseSHA) || a.Merge.RequesterID <= 0 ||
		len(a.Merge.ReviewDigest) != 64 || !shaPattern.MatchString(a.Merge.ReviewDigest) {
		return failure("merge requires exact head/base, a verified requester and native clean-review evidence")
	}
	if a.Body != "" || a.Merge.Build != nil && (a.Merge.Build.Provider != "jenkins" ||
		a.Merge.Build.HeadSHA != a.HeadSHA || a.Merge.Build.Run <= 0) {
		return failure("invalid merge compilation evidence")
	}
	return nil
}

// CheckMerge reads authority and compilation facts from fixed GitHub routes.
// A conflict, unknown mergeability, stale assessment or missing proof blocks.
func (c *Client) CheckMerge(ctx context.Context, installationID int64, a v1.Action) error {
	if err := validateMerge(a); err != nil {
		return err
	}
	base, err := repoPath(a.Repository)
	if err != nil {
		return err
	}
	token, err := c.credential(ctx, installationID)
	if err != nil {
		return err
	}
	b, _, err := c.request(ctx, "GET", fmt.Sprintf("%s/pulls/%d", base, a.Number), token, nil, false)
	if err != nil {
		return err
	}
	var pr struct {
		State          string `json:"state"`
		Draft          bool   `json:"draft"`
		Merged         bool   `json:"merged"`
		Mergeable      *bool  `json:"mergeable"`
		MergeableState string `json:"mergeable_state"`
		Head           struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			SHA  string `json:"sha"`
			Repo struct {
				ID       int64  `json:"id"`
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
		User struct {
			ID int64 `json:"id"`
		} `json:"user"`
		Assignees []struct {
			ID int64 `json:"id"`
		} `json:"assignees"`
	}
	if json.Unmarshal(b, &pr) != nil || !strings.EqualFold(pr.Base.Repo.FullName, a.Repository) ||
		pr.Base.Repo.ID != a.RepositoryID || pr.State != "open" || pr.Draft || pr.Merged || pr.Head.SHA != a.HeadSHA || pr.Base.SHA != a.Merge.BaseSHA {
		return failure("PR is closed/draft, targets another repository, or its head/base changed since review")
	}
	allowed := pr.User.ID == a.Merge.RequesterID
	for _, assignee := range pr.Assignees {
		allowed = allowed || assignee.ID == a.Merge.RequesterID
	}
	if !allowed {
		return failure("requester is neither the PR author nor a current assignee")
	}
	if pr.Mergeable == nil || !*pr.Mergeable || pr.MergeableState != "clean" {
		return failure("PR has conflicts, pending mergeability or unmet GitHub merge requirements")
	}
	// A newer review from the same reviewer supersedes an older one.
	reviews, err := c.Context(ctx, installationID, v1.ContextRequest{Repository: a.Repository, Number: a.Number, Kind: "reviews"})
	if err != nil {
		return err
	}
	var all []struct {
		ID    int64  `json:"id"`
		State string `json:"state"`
		User  struct {
			ID int64 `json:"id"`
		} `json:"user"`
	}
	if json.Unmarshal(reviews, &all) != nil {
		return failure("GitHub reviews are unreadable")
	}
	type latest struct {
		id    int64
		state string
	}
	byUser := map[int64]latest{}
	for _, review := range all {
		if review.User.ID > 0 && review.ID > byUser[review.User.ID].id && (review.State == "APPROVED" || review.State == "CHANGES_REQUESTED") {
			byUser[review.User.ID] = latest{review.ID, review.State}
		}
	}
	for _, review := range byUser {
		if review.state == "CHANGES_REQUESTED" {
			return failure("a GitHub reviewer still requests changes")
		}
	}
	if a.Merge.Build == nil {
		return c.compilationPassed(ctx, token, base, a.HeadSHA, a.Merge.BaseSHA, a.Merge.TrustedChecks)
	}
	return nil
}

func (c *Client) compilationPassed(ctx context.Context, token, base, head, baseSHA string, rules []v1.TrustedCheck) error {
	if len(rules) == 0 {
		return failure("no trusted compilation producer is configured and no verified Jenkins build was supplied")
	}
	type observation struct {
		id     int64
		passed bool
	}
	latest := map[string]observation{}
	needChecks, needStatuses := false, false
	for _, rule := range rules {
		needChecks = needChecks || rule.AppID > 0
		needStatuses = needStatuses || rule.CreatorID > 0
	}
	for _, kind := range []string{"check-runs", "statuses"} {
		if kind == "check-runs" && !needChecks || kind == "statuses" && !needStatuses {
			continue
		}
		for page := 1; page <= 100; page++ {
			path := fmt.Sprintf("%s/commits/%s/%s?per_page=100&page=%d", base, head, kind, page)
			b, h, err := c.request(ctx, "GET", path, token, nil, false)
			if err != nil {
				return err
			}
			var records []struct {
				ID         int64  `json:"id"`
				Name       string `json:"name"`
				Context    string `json:"context"`
				HeadSHA    string `json:"head_sha"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
				State      string `json:"state"`
				CheckSuite struct {
					ID int64 `json:"id"`
				} `json:"check_suite"`
				App struct {
					ID   int64  `json:"id"`
					Slug string `json:"slug"`
				} `json:"app"`
				Creator struct {
					ID int64 `json:"id"`
				} `json:"creator"`
			}
			if kind == "check-runs" {
				var envelope struct {
					CheckRuns json.RawMessage `json:"check_runs"`
				}
				if json.Unmarshal(b, &envelope) != nil || envelope.CheckRuns == nil {
					return failure("unreadable check runs")
				}
				b = envelope.CheckRuns
			}
			if json.Unmarshal(b, &records) != nil {
				return failure("unreadable compilation checks")
			}
			for _, record := range records {
				for _, rule := range rules {
					matches := kind == "check-runs" && rule.AppID > 0 && record.App.ID == rule.AppID && record.Name == rule.Name && record.HeadSHA == head ||
						kind == "statuses" && rule.CreatorID > 0 && record.Creator.ID == rule.CreatorID && record.Context == rule.Name
					if !matches {
						continue
					}
					if kind == "check-runs" && record.App.Slug == "github-actions" {
						if err := c.trustedWorkflow(ctx, token, base, head, baseSHA, record.CheckSuite.ID, rule.WorkflowPath); err != nil {
							return err
						}
					}
					key := fmt.Sprintf("%s:%s:%d:%d", kind, rule.Name, rule.AppID, rule.CreatorID)
					passed := kind == "check-runs" && record.Status == "completed" && record.Conclusion == "success" || kind == "statuses" && record.State == "success"
					if record.ID > latest[key].id {
						latest[key] = observation{record.ID, passed}
					}
				}
			}
			if !strings.Contains(h.Get("Link"), `rel="next"`) {
				break
			}
			if page == 100 {
				return failure("compilation check pagination exceeds limit")
			}
		}
	}
	passed := false
	for _, result := range latest {
		if !result.passed {
			return failure("a trusted compilation check failed or is pending")
		}
		passed = true
	}
	if !passed {
		return failure("no successful trusted compilation check exists for the exact PR head")
	}
	return nil
}
