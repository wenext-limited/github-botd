package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// GitHub Actions is a shared producer: an app id and check name alone do not
// prove that a PR did not replace its compilation workflow with `exit 0`.
// Require the configured workflow blob to be unchanged from the reviewed base.
func (c *Client) trustedWorkflow(ctx context.Context, token, base, head, baseSHA string, suiteID int64, path string) error {
	if !strings.HasPrefix(path, ".github/workflows/") || strings.Contains(path, "..") || strings.ContainsAny(path, "\\?&#") {
		return failure("GitHub Actions compilation requires a trusted workflow_path")
	}
	if suiteID <= 0 {
		return failure("compilation check has no trusted workflow run")
	}
	found := false
	for page := 1; page <= 100 && !found; page++ {
		b, h, err := c.request(ctx, "GET", fmt.Sprintf("%s/actions/runs?head_sha=%s&per_page=100&page=%d", base, head, page), token, nil, false)
		if err != nil {
			return err
		}
		var list struct {
			Runs []struct {
				Head       string `json:"head_sha"`
				SuiteID    int64  `json:"check_suite_id"`
				Path       string `json:"path"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"workflow_runs"`
		}
		if json.Unmarshal(b, &list) != nil || list.Runs == nil {
			return failure("workflow provenance is unreadable")
		}
		for _, run := range list.Runs {
			if run.SuiteID == suiteID && run.Head == head && run.Path == path && run.Status == "completed" && run.Conclusion == "success" {
				found = true
			}
		}
		if !strings.Contains(h.Get("Link"), `rel="next"`) {
			break
		}
	}
	if !found {
		return failure("compilation check did not come from the configured successful workflow at this head")
	}
	var hashes []string
	for _, ref := range []string{baseSHA, head} {
		b, _, err := c.request(ctx, "GET", fmt.Sprintf("%s/contents/%s?ref=%s", base, path, ref), token, nil, false)
		if err != nil {
			return err
		}
		var file struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		}
		if json.Unmarshal(b, &file) != nil || file.Type != "file" || !shaPattern.MatchString(file.SHA) {
			return failure("trusted workflow is not a regular GitHub file")
		}
		hashes = append(hashes, file.SHA)
	}
	if hashes[0] != hashes[1] {
		return failure("PR changed the trusted compilation workflow; independent Jenkins compilation is required")
	}
	return nil
}
