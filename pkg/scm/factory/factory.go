package factory

import (
	"fmt"
	"strings"

	"github.com/xyzjace/terraplane/config"
	"github.com/xyzjace/terraplane/pkg/log"
	"github.com/xyzjace/terraplane/pkg/scm"
	"github.com/xyzjace/terraplane/pkg/scm/github"
)

func NewProvider(logger log.Logger, cfg *config.Config) (scm.Provider, error) {
	switch normalizeProvider(cfg.SCMProvider) {
	case "github":
		client, err := github.NewClient(cfg)
		if err != nil {
			return nil, err
		}
		return github.NewProvider(logger, cfg, client), nil
	default:
		return nil, fmt.Errorf("unknown SCM provider %q", cfg.SCMProvider)
	}
}

func NewPublisher(logger log.Logger, cfg *config.Config) (scm.Publisher, error) {
	switch normalizeProvider(cfg.SCMProvider) {
	case "github":
		client, err := github.NewClient(cfg)
		if err != nil {
			return nil, err
		}
		return github.NewPublisher(logger, client), nil
	default:
		return nil, fmt.Errorf("unknown SCM provider %q", cfg.SCMProvider)
	}
}

func NewRepositoryAccess(logger log.Logger, cfg *config.Config) (scm.RepositoryAccess, error) {
	switch normalizeProvider(cfg.SCMProvider) {
	case "github":
		client, err := github.NewClient(cfg)
		if err != nil {
			return nil, err
		}
		return github.NewRepositoryAccess(logger, cfg, client), nil
	default:
		return nil, fmt.Errorf("unknown SCM provider %q", cfg.SCMProvider)
	}
}

func normalizeProvider(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "github"
	}
	return name
}
