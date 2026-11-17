package github

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/wenext-limited/github-botd/api/v1"
)

func mergeAction() v1.Action {
	return v1.Action{Version: v1.Version, Kind: "pull.merge", Repository: "YouPast/FrameUI", RepositoryID: 111, Number: 69,
		HeadSHA: strings.Repeat("a", 40), Merge: &v1.MergeEvidence{RequesterID: 1, BaseSHA: strings.Repeat("b", 40),
			ReviewDigest: strings.Repeat("c", 64), TrustedChecks: []v1.TrustedCheck{{Name: "compile", AppID: 7}}}}
}

func mergePR() map[string]any {
	return map[string]any{"state": "open", "draft": false, "merged": false, "mergeable": true, "mergeable_state": "clean",
		"head": map[string]any{"sha": strings.Repeat("a", 40)},
		"base": map[string]any{"sha": strings.Repeat("b", 40), "repo": map[string]any{"full_name": "YouPast/FrameUI", "id": 111}},
		"user": map[string]any{"id": 1}, "assignees": []any{map[string]any{"id": 2}}}
}

func TestGuardedMergeConditions(t *testing.T) {
	for _, name := range []string{"author", "assignee", "unrelated-requester", "closed", "draft", "merged", "head-moved", "base-moved", "wrong-repo", "reused-repository", "conflict", "unknown", "blocked", "failed-build", "pending-build", "skipped-build", "wrong-producer", "old-build", "newer-failed-build", "changes-requested", "review-commented", "review-approved", "review-dismissed"} {
		t.Run(name, func(t *testing.T) {
			a := mergeAction()
			pr := mergePR()
			writes := 0
			check := map[string]any{"id": 10, "name": "compile", "app": map[string]any{"id": 7}, "head_sha": a.HeadSHA, "status": "completed", "conclusion": "success"}
			checks := []any{check}
			reviews := []any{}
			pass := name == "author" || name == "assignee" || name == "review-dismissed" || name == "review-approved"
			switch name {
			case "assignee":
				a.Merge.RequesterID = 2
			case "unrelated-requester":
				a.Merge.RequesterID = 3
			case "closed":
				pr["state"] = "closed"
			case "draft":
				pr["draft"] = true
			case "merged":
				pr["merged"] = true
			case "head-moved":
				pr["head"] = map[string]any{"sha": strings.Repeat("d", 40)}
			case "base-moved":
				pr["base"].(map[string]any)["sha"] = strings.Repeat("d", 40)
			case "wrong-repo":
				pr["base"].(map[string]any)["repo"] = map[string]any{"full_name": "someone/else"}
			case "reused-repository":
				pr["base"].(map[string]any)["repo"].(map[string]any)["id"] = 222
			case "conflict":
				pr["mergeable"] = false
			case "unknown":
				pr["mergeable"] = nil
			case "blocked":
				pr["mergeable_state"] = "blocked"
			case "failed-build":
				check["conclusion"] = "failure"
			case "pending-build":
				check["status"] = "in_progress"
			case "skipped-build":
				check["conclusion"] = "skipped"
			case "wrong-producer":
				check["app"] = map[string]any{"id": 8}
			case "old-build":
				check["head_sha"] = strings.Repeat("d", 40)
			case "newer-failed-build":
				checks = append(checks, map[string]any{"id": 11, "name": "compile", "app": map[string]any{"id": 7}, "head_sha": a.HeadSHA, "status": "completed", "conclusion": "failure"})
			case "changes-requested", "review-commented", "review-approved", "review-dismissed":
				reviews = append(reviews, map[string]any{"id": 1, "state": "CHANGES_REQUESTED", "user": map[string]any{"id": 30}})
				switch name {
				case "review-dismissed":
					reviews[0].(map[string]any)["state"] = "DISMISSED"
				case "review-commented":
					reviews = append(reviews, map[string]any{"id": 2, "state": "COMMENTED", "user": map[string]any{"id": 30}})
				case "review-approved":
					reviews = append(reviews, map[string]any{"id": 2, "state": "APPROVED", "user": map[string]any{"id": 30}})
				}
			}
			client := machine(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /repos/YouPast/FrameUI/pulls/69":
					json.NewEncoder(w).Encode(pr)
				case "GET /repos/YouPast/FrameUI/pulls/69/reviews":
					json.NewEncoder(w).Encode(reviews)
				case "GET /repos/YouPast/FrameUI/commits/" + a.HeadSHA + "/check-runs":
					json.NewEncoder(w).Encode(map[string]any{"check_runs": checks})
				case "PUT /repos/YouPast/FrameUI/pulls/69/merge":
					writes++
					var body map[string]any
					json.NewDecoder(r.Body).Decode(&body)
					if len(body) != 2 || body["sha"] != a.HeadSHA || body["merge_method"] != "squash" {
						t.Errorf("unsafe merge payload: %v", body)
					}
					json.NewEncoder(w).Encode(map[string]any{"merged": true, "sha": strings.Repeat("e", 40)})
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			})
			_, err := client.Execute(context.Background(), 0, a)
			if pass && (err != nil || writes != 1) {
				t.Fatalf("expected one guarded merge, writes=%d err=%v", writes, err)
			}
			if !pass && (err == nil || writes != 0) {
				t.Fatalf("unsafe merge accepted, writes=%d err=%v", writes, err)
			}
		})
	}
}

func TestMergeRechecksAfterPreflight(t *testing.T) {
	a := mergeAction()
	pr := mergePR()
	writes := 0
	client := machine(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			json.NewEncoder(w).Encode(map[string]any{"check_runs": []any{map[string]any{"id": 1, "name": "compile", "app": map[string]any{"id": 7}, "head_sha": a.HeadSHA, "status": "completed", "conclusion": "success"}}})
		case r.Method == "PUT":
			writes++
			w.Write([]byte(`{"merged":true}`))
		default:
			json.NewEncoder(w).Encode(pr)
		}
	})
	if err := client.CheckMerge(context.Background(), 0, a); err != nil {
		t.Fatal(err)
	}
	pr["head"] = map[string]any{"sha": strings.Repeat("f", 40)}
	if _, err := client.Execute(context.Background(), 0, a); err == nil || writes != 0 {
		t.Fatal("stale preflight authorized a write")
	}
}

func TestMergeNeedsNativeReviewAndCompileProof(t *testing.T) {
	for _, name := range []string{"no-review", "bad-digest", "no-requester", "wrong-build-sha", "no-producer"} {
		t.Run(name, func(t *testing.T) {
			a := mergeAction()
			switch name {
			case "no-review":
				a.Merge = nil
			case "bad-digest":
				a.Merge.ReviewDigest = "clean"
			case "no-requester":
				a.Merge.RequesterID = 0
			case "wrong-build-sha":
				a.Merge.Build = &v1.BuildEvidence{Provider: "jenkins", Run: 1, Source: "job", HeadSHA: strings.Repeat("f", 40)}
			case "no-producer":
				a.Merge.TrustedChecks = nil
			}
			if name != "no-producer" {
				if err := ValidateAction(a); err == nil {
					t.Fatal("invalid proof accepted")
				}
				return
			}
			client := machine(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/reviews") {
					w.Write([]byte(`[]`))
				} else {
					json.NewEncoder(w).Encode(mergePR())
				}
			})
			if _, err := client.Execute(context.Background(), 0, a); err == nil {
				t.Fatal("missing compilation proof accepted")
			}
		})
	}
}

func TestGitHubActionsCompilationNeedsTheActualTrustedWorkflow(t *testing.T) {
	for _, name := range []string{"trusted", "missing-path", "other-workflow", "rewritten-workflow", "wrong-suite"} {
		t.Run(name, func(t *testing.T) {
			a := mergeAction()
			a.Merge.TrustedChecks[0].WorkflowPath = ".github/workflows/build.yml"
			if name == "missing-path" {
				a.Merge.TrustedChecks[0].WorkflowPath = ""
			}
			writes := 0
			client := machine(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/reviews"):
					w.Write([]byte(`[]`))
				case strings.HasSuffix(r.URL.Path, "/check-runs"):
					json.NewEncoder(w).Encode(map[string]any{"check_runs": []any{map[string]any{"id": 1, "name": "compile", "head_sha": a.HeadSHA, "status": "completed", "conclusion": "success", "app": map[string]any{"id": 7, "slug": "github-actions"}, "check_suite": map[string]any{"id": 55}}}})
				case strings.HasSuffix(r.URL.Path, "/actions/runs"):
					path := ".github/workflows/build.yml"
					suite := 55
					if name == "other-workflow" {
						path = ".github/workflows/fake.yml"
					}
					if name == "wrong-suite" {
						suite = 56
					}
					json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []any{map[string]any{"head_sha": a.HeadSHA, "path": path, "check_suite_id": suite, "status": "completed", "conclusion": "success"}}})
				case strings.Contains(r.URL.Path, "/contents/"):
					sha := strings.Repeat("c", 40)
					if name == "rewritten-workflow" && r.URL.Query().Get("ref") == a.HeadSHA {
						sha = strings.Repeat("d", 40)
					}
					json.NewEncoder(w).Encode(map[string]any{"type": "file", "sha": sha})
				case r.Method == "PUT":
					writes++
					json.NewEncoder(w).Encode(map[string]any{"merged": true, "sha": strings.Repeat("e", 40)})
				default:
					json.NewEncoder(w).Encode(mergePR())
				}
			})
			_, err := client.Execute(context.Background(), 0, a)
			if name == "trusted" && (err != nil || writes != 1) {
				t.Fatalf("trusted compilation refused: %v", err)
			}
			if name != "trusted" && (err == nil || writes != 0) {
				t.Fatalf("untrusted compilation accepted: %v", err)
			}
		})
	}
}
