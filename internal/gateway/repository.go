package gateway

import (
	"strings"

	"github.com/wenext-limited/github-botd/internal/config"
)

// ResolveRepository keeps authorization independent of the spelling used to
// address a repository. Legacy aliases remain pinned; canonical names need no
// duplicate mapping. Broad access is explicit and still capability-scoped.
func (g *Gateway) ResolveRepository(client, written string) (config.Repository, error) {
	p, ok := g.Config.Clients[client]
	if !ok {
		return config.Repository{}, ErrForbidden
	}
	if repo, ok := g.Config.Repositories[written]; ok {
		if config.Contains(p.Repositories, written) || p.AllRepositories {
			return repo, nil
		}
		return config.Repository{}, ErrForbidden
	}
	if !validRepository(written) {
		return config.Repository{}, ErrForbidden
	}
	for alias, repo := range g.Config.Repositories {
		if strings.EqualFold(repo.FullName, written) {
			if config.Contains(p.Repositories, alias) || p.AllRepositories {
				return repo, nil
			}
			return config.Repository{}, ErrForbidden
		}
	}
	if p.AllRepositories && len(g.Config.GitHub.Apps) == 0 {
		return config.Repository{FullName: written}, nil
	}
	return config.Repository{}, ErrForbidden
}

func validRepository(written string) bool {
	parts := strings.Split(written, "/")
	if len(parts) != 2 || len(parts[0]) > 39 || len(parts[1]) > 100 {
		return false
	}
	for i, part := range parts {
		if part == "" || part[0] == '-' || part == "." || part == ".." || i == 0 && part[0] == '.' {
			return false
		}
		for _, c := range []byte(part) {
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' {
				continue
			}
			if i == 1 && (c == '_' || c == '.') {
				continue
			}
			return false
		}
	}
	return !strings.HasSuffix(parts[0], "-")
}
