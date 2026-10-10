package authz

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/xyzjace/terraplane/config"
	"github.com/xyzjace/terraplane/pkg/log"
	"github.com/xyzjace/terraplane/pkg/orchestratorconfig"
	"github.com/xyzjace/terraplane/pkg/scm"
)

//go:generate mockgen -source=authorizer.go -destination=mock_authz/mock_authorizer.go -package=mock_authz

type Action string

const (
	ActionPlan   Action = "plan"
	ActionApply  Action = "apply"
	ActionUnlock Action = "unlock"
)

// Authorizer decides whether a user may run an action on a stack.
type Authorizer interface {
	// Allowed reports whether user may perform action on stack in repo. When
	// the answer is no, reason says why in a sentence fit for a PR comment.
	Allowed(ctx context.Context, user, repo, stack string, action Action) (bool, string, error)
}

// TeamMembership resolves @org/team permission entries. scm.Provider satisfies it.
type TeamMembership interface {
	TeamMember(ctx context.Context, org, team, user string) (bool, error)
}

type allowAll struct{}

func (allowAll) Allowed(context.Context, string, string, string, Action) (bool, string, error) {
	return true, "", nil
}

// AllowAll permits everything. Used when no orchestrator config is set.
func AllowAll() Authorizer {
	return allowAll{}
}

type authorizer struct {
	logger log.Logger
	config *orchestratorconfig.ConfigFile
	teams  TeamMembership
	cache  *teamCache
}

func New(cfg *config.Config, logger log.Logger, orchestratorConfig *orchestratorconfig.ConfigFile, provider scm.Provider) Authorizer {
	if !orchestratorConfig.Enabled() {
		return AllowAll()
	}
	return NewWith(logger, orchestratorConfig, provider, cfg.OrchestratorTeamCacheTTL)
}

// NewWith constructs a config-backed Authorizer with explicit dependencies (useful in tests).
func NewWith(logger log.Logger, orchestratorConfig *orchestratorconfig.ConfigFile, teams TeamMembership, cacheTTL time.Duration) Authorizer {
	return &authorizer{
		logger: logger,
		config: orchestratorConfig,
		teams:  teams,
		cache:  newTeamCache(cacheTTL),
	}
}

func (a *authorizer) Allowed(ctx context.Context, user, repo, stack string, action Action) (bool, string, error) {
	perms, ok := a.config.PermissionsFor(repo, stack)
	if !ok {
		return false, fmt.Sprintf("repository %s is not configured on the orchestrator", repo), nil
	}

	for _, entry := range entriesFor(perms, action) {
		if !strings.HasPrefix(entry, "@") {
			// GitHub logins are case-insensitive.
			if strings.EqualFold(entry, user) {
				return true, "", nil
			}
			continue
		}

		org, team, _ := strings.Cut(strings.TrimPrefix(entry, "@"), "/")
		member, err := a.isTeamMember(ctx, org, team, user)
		if err != nil {
			return false, "", fmt.Errorf("failed to check whether %s is a member of %s: %w", user, entry, err)
		}
		if member {
			return true, "", nil
		}
	}

	return false, fmt.Sprintf("%s is not allowed to %s stack %s in %s", user, action, stack, repo), nil
}

func (a *authorizer) isTeamMember(ctx context.Context, org, team, user string) (bool, error) {
	key := org + "/" + team + "/" + strings.ToLower(user)
	if member, ok := a.cache.get(key); ok {
		return member, nil
	}

	member, err := a.teams.TeamMember(ctx, org, team, user)
	if err != nil {
		return false, err
	}
	a.logger.Debug("Resolved team membership", "org", org, "team", team, "user", user, "member", member)
	a.cache.set(key, member)
	return member, nil
}

func entriesFor(perms orchestratorconfig.Permissions, action Action) []string {
	switch action {
	case ActionPlan:
		return perms.Plan
	case ActionApply:
		return perms.Apply
	case ActionUnlock:
		return perms.Unlock
	default:
		return nil
	}
}

// teamCache remembers team membership answers for ttl so a command touching
// many stacks does not ask GitHub once per stack. A ttl <= 0 disables it.
type teamCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[string]teamCacheEntry
}

type teamCacheEntry struct {
	member  bool
	expires time.Time
}

func newTeamCache(ttl time.Duration) *teamCache {
	return &teamCache{
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[string]teamCacheEntry),
	}
}

func (c *teamCache) get(key string) (bool, bool) {
	if c.ttl <= 0 {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return false, false
	}
	if !c.now().Before(entry.expires) {
		delete(c.entries, key)
		return false, false
	}
	return entry.member, true
}

func (c *teamCache) set(key string, member bool) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = teamCacheEntry{member: member, expires: c.now().Add(c.ttl)}
}
