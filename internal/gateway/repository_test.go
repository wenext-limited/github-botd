package gateway

import (
	"testing"

	v1 "github.com/wenext-limited/github-botd/api/v1"
)

func TestCanonicalRepositoriesNeedNoAlias(t *testing.T) {
	g := fixture(t)
	client := g.Config.Clients["agent"]
	client.AllRepositories = true
	g.Config.Clients["agent"] = client
	for _, slug := range []string{"YouPast/FrameUI", "torvalds/linux", "YouPast/.github"} {
		repo, err := g.ResolveRepository("agent", slug)
		if err != nil || repo.FullName != slug {
			t.Fatalf("%s: %v %v", slug, repo, err)
		}
	}
	for _, bad := range []string{"../etc", "YouPast/../etc", "https://github.com/YouPast/FrameUI", "YouPast/FrameUI?x", "YouPast/FrameUI\\x", "/FrameUI", "YouPast/"} {
		if _, err := g.ResolveRepository("agent", bad); err == nil {
			t.Fatalf("unsafe canonical target: %q", bad)
		}
	}
	client.AllRepositories = false
	g.Config.Clients["agent"] = client
	if _, err := g.ResolveRepository("agent", "YouPast/FrameUI"); err == nil {
		t.Fatal("broad grant revocation ignored")
	}
	if _, err := g.ResolveRepository("unknown", "YouPast/FrameUI"); err == nil {
		t.Fatal("unknown client accepted")
	}
}

func TestNativeJenkinsEvidenceIsSourceScoped(t *testing.T) {
	g := fixture(t)
	p := g.Config.Clients["agent"]
	p.Capabilities = append(p.Capabilities, "pull.merge")
	p.MergeJenkinsSources = []string{"Lama-iOS"}
	g.Config.Clients["agent"] = p
	a := v1.Action{Kind: "pull.merge", HeadSHA: "head", Merge: &v1.MergeEvidence{Build: &v1.BuildEvidence{Provider: "jenkins", Source: "Lama-iOS", HeadSHA: "head", Run: 7}}}
	if err := g.prepareMerge("agent", &a); err != nil {
		t.Fatal(err)
	}
	a.Merge.Build.Source = "someone-else"
	if err := g.prepareMerge("agent", &a); err == nil {
		t.Fatal("untrusted Jenkins producer accepted")
	}
	if err := g.prepareMerge("unknown", &a); err == nil {
		t.Fatal("unknown principal attestation accepted")
	}
}
