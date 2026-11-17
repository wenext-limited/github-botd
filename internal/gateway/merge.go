package gateway

import (
	"context"
	"errors"

	v1 "github.com/wenext-limited/github-botd/api/v1"
	"github.com/wenext-limited/github-botd/internal/config"
)

func (g *Gateway) prepareMerge(client string, a *v1.Action) error {
	p, ok := g.Config.Clients[client]
	if !ok || !config.Contains(p.Capabilities, "pull.merge") || a.Kind != "pull.merge" || a.Merge == nil {
		return ErrForbidden
	}
	if a.Merge.Build != nil {
		proof := a.Merge.Build
		if proof.Provider != "jenkins" || proof.HeadSHA != a.HeadSHA || proof.Run <= 0 ||
			!config.Contains(p.MergeJenkinsSources, proof.Source) {
			return errors.New("Jenkins compilation evidence is missing or from an untrusted source")
		}
	}
	// Trusted producer rules belong to current operator policy, never the call.
	a.Merge.TrustedChecks = p.MergeChecks
	return nil
}

// CheckMerge is a read-only preflight. Execution repeats it immediately before
// the SHA-conditional GitHub write, under the latest client grants.
func (g *Gateway) CheckMerge(ctx context.Context, client string, a v1.Action) error {
	if a.Version != v1.Version {
		return errors.New("unsupported action version")
	}
	if err := g.prepareMerge(client, &a); err != nil {
		return err
	}
	repo, err := g.ResolveRepository(client, a.Repository)
	if err != nil {
		return err
	}
	a.Repository = repo.FullName
	checker, ok := g.GitHub.(interface {
		CheckMerge(context.Context, int64, v1.Action) error
	})
	if !ok {
		return errors.New("GitHub merge verification is unavailable")
	}
	return checker.CheckMerge(ctx, repo.InstallationID, a)
}
