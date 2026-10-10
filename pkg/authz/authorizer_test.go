package authz

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xyzjace/terraplane/config"
	"github.com/xyzjace/terraplane/pkg/log"
	"github.com/xyzjace/terraplane/pkg/orchestratorconfig"
)

const policyYAML = `
agents:
  agent-prod:
    token_env: T
repos:
  acme/infra:
    agent: agent-prod
    permissions:
      plan: ["@acme/engineering", "@acme/platform"]
      apply: ["alice"]
    stacks:
      reporting:
        dir: stacks/reporting
        permissions:
          plan: ["bob"]
          apply: ["bob"]
          unlock: ["carol"]
  acme/locked:
    agent: agent-prod
`

type teamCall struct {
	org, team, user string
}

type stubTeams struct {
	members map[string]bool // "org/team/user" → member
	err     error
	calls   []teamCall
}

func (s *stubTeams) TeamMember(_ context.Context, org, team, user string) (bool, error) {
	s.calls = append(s.calls, teamCall{org: org, team: team, user: user})
	if s.err != nil {
		return false, s.err
	}
	return s.members[org+"/"+team+"/"+user], nil
}

func policy(t *testing.T) *orchestratorconfig.ConfigFile {
	t.Helper()
	cfg, err := orchestratorconfig.Parse([]byte(policyYAML), func(string) string { return "token" })
	require.NoError(t, err)
	return cfg
}

func TestNewReturnsAllowAllWithoutConfig(t *testing.T) {
	a := New(&config.Config{}, log.Noop(), &orchestratorconfig.ConfigFile{}, nil)
	ok, reason, err := a.Allowed(context.Background(), "anyone", "acme/anything", "stack", ActionApply)
	require.NoError(t, err)
	require.True(t, ok)
	require.Empty(t, reason)
}

func TestNewReturnsConfigBackedAuthorizerWithConfig(t *testing.T) {
	a := New(&config.Config{OrchestratorTeamCacheTTL: time.Minute}, log.Noop(), policy(t), nil)
	ok, reason, err := a.Allowed(context.Background(), "mallory", "acme/infra", "a", ActionApply)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, "mallory is not allowed to apply stack a in acme/infra", reason)
}

func TestAllowedByUser(t *testing.T) {
	teams := &stubTeams{}
	a := NewWith(log.Noop(), policy(t), teams, 0)

	ok, _, err := a.Allowed(context.Background(), "alice", "acme/infra", "a", ActionApply)
	require.NoError(t, err)
	require.True(t, ok)

	// Logins are case-insensitive.
	ok, _, err = a.Allowed(context.Background(), "Alice", "acme/infra", "a", ActionApply)
	require.NoError(t, err)
	require.True(t, ok)

	require.Empty(t, teams.calls)
}

func TestAllowedByTeam(t *testing.T) {
	teams := &stubTeams{members: map[string]bool{"acme/platform/dave": true}}
	a := NewWith(log.Noop(), policy(t), teams, 0)

	ok, _, err := a.Allowed(context.Background(), "dave", "acme/infra", "a", ActionPlan)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []teamCall{
		{org: "acme", team: "engineering", user: "dave"},
		{org: "acme", team: "platform", user: "dave"},
	}, teams.calls)
}

func TestDeniedWhenNotMemberOfAnyTeam(t *testing.T) {
	teams := &stubTeams{}
	a := NewWith(log.Noop(), policy(t), teams, 0)

	ok, reason, err := a.Allowed(context.Background(), "dave", "acme/infra", "a", ActionPlan)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, "dave is not allowed to plan stack a in acme/infra", reason)
	require.Len(t, teams.calls, 2)
}

func TestTeamLookupErrorFailsClosed(t *testing.T) {
	teams := &stubTeams{err: errors.New("github down")}
	a := NewWith(log.Noop(), policy(t), teams, 0)

	ok, _, err := a.Allowed(context.Background(), "dave", "acme/infra", "a", ActionPlan)
	require.Error(t, err)
	require.False(t, ok)
	require.Contains(t, err.Error(), "failed to check whether dave is a member of @acme/engineering")
	require.Contains(t, err.Error(), "github down")
}

func TestStackPermissionsReplaceRepoPermissions(t *testing.T) {
	a := NewWith(log.Noop(), policy(t), &stubTeams{}, 0)

	ok, _, err := a.Allowed(context.Background(), "alice", "acme/infra", "reporting", ActionApply)
	require.NoError(t, err)
	require.False(t, ok)

	ok, _, err = a.Allowed(context.Background(), "bob", "acme/infra", "reporting", ActionApply)
	require.NoError(t, err)
	require.True(t, ok)

	ok, _, err = a.Allowed(context.Background(), "carol", "acme/infra", "reporting", ActionUnlock)
	require.NoError(t, err)
	require.True(t, ok)
}

func TestUnlockDefaultsToApplyList(t *testing.T) {
	a := NewWith(log.Noop(), policy(t), &stubTeams{}, 0)

	ok, _, err := a.Allowed(context.Background(), "alice", "acme/infra", "a", ActionUnlock)
	require.NoError(t, err)
	require.True(t, ok)
}

func TestRepoWithoutPermissionsDeniesEverything(t *testing.T) {
	a := NewWith(log.Noop(), policy(t), &stubTeams{}, 0)

	for _, action := range []Action{ActionPlan, ActionApply, ActionUnlock} {
		ok, reason, err := a.Allowed(context.Background(), "alice", "acme/locked", "a", action)
		require.NoError(t, err)
		require.False(t, ok, action)
		require.Contains(t, reason, "is not allowed to "+string(action))
	}
}

func TestUnknownRepoIsDenied(t *testing.T) {
	a := NewWith(log.Noop(), policy(t), &stubTeams{}, 0)

	ok, reason, err := a.Allowed(context.Background(), "alice", "acme/unknown", "a", ActionPlan)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, "repository acme/unknown is not configured on the orchestrator", reason)
}

func TestUnknownActionIsDenied(t *testing.T) {
	a := NewWith(log.Noop(), policy(t), &stubTeams{}, 0)

	ok, _, err := a.Allowed(context.Background(), "alice", "acme/infra", "a", Action("destroy"))
	require.NoError(t, err)
	require.False(t, ok)
}

func TestTeamCacheAvoidsRepeatedLookups(t *testing.T) {
	teams := &stubTeams{members: map[string]bool{"acme/engineering/dave": true}}
	a := NewWith(log.Noop(), policy(t), teams, time.Minute)

	for range 3 {
		ok, _, err := a.Allowed(context.Background(), "dave", "acme/infra", "a", ActionPlan)
		require.NoError(t, err)
		require.True(t, ok)
	}
	require.Len(t, teams.calls, 1)

	// Case differences in the login share a cache entry.
	ok, _, err := a.Allowed(context.Background(), "Dave", "acme/infra", "a", ActionPlan)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, teams.calls, 1)
}

func TestTeamCacheCachesNegativeAnswers(t *testing.T) {
	teams := &stubTeams{}
	a := NewWith(log.Noop(), policy(t), teams, time.Minute)

	for range 2 {
		ok, _, err := a.Allowed(context.Background(), "dave", "acme/infra", "a", ActionPlan)
		require.NoError(t, err)
		require.False(t, ok)
	}
	// Two teams checked once each, then served from cache.
	require.Len(t, teams.calls, 2)
}

func TestTeamCacheExpires(t *testing.T) {
	teams := &stubTeams{members: map[string]bool{"acme/engineering/dave": true}}
	a := NewWith(log.Noop(), policy(t), teams, time.Minute)

	now := time.Now()
	a.(*authorizer).cache.now = func() time.Time { return now }

	_, _, err := a.Allowed(context.Background(), "dave", "acme/infra", "a", ActionPlan)
	require.NoError(t, err)
	require.Len(t, teams.calls, 1)

	now = now.Add(2 * time.Minute)
	_, _, err = a.Allowed(context.Background(), "dave", "acme/infra", "a", ActionPlan)
	require.NoError(t, err)
	require.Len(t, teams.calls, 2)
}

func TestTeamCacheDisabledWhenTTLZero(t *testing.T) {
	teams := &stubTeams{members: map[string]bool{"acme/engineering/dave": true}}
	a := NewWith(log.Noop(), policy(t), teams, 0)

	for range 2 {
		_, _, err := a.Allowed(context.Background(), "dave", "acme/infra", "a", ActionPlan)
		require.NoError(t, err)
	}
	require.Len(t, teams.calls, 2)
}

func TestTeamLookupErrorIsNotCached(t *testing.T) {
	teams := &stubTeams{err: errors.New("github down")}
	a := NewWith(log.Noop(), policy(t), teams, time.Minute)

	_, _, err := a.Allowed(context.Background(), "dave", "acme/infra", "a", ActionPlan)
	require.Error(t, err)

	teams.err = nil
	teams.members = map[string]bool{"acme/engineering/dave": true}
	ok, _, err := a.Allowed(context.Background(), "dave", "acme/infra", "a", ActionPlan)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, teams.calls, 2)
}

func TestTeamCacheSweepsExpiredEntriesWhenLarge(t *testing.T) {
	c := newTeamCache(time.Minute)
	now := time.Now()
	c.now = func() time.Time { return now }

	for i := range teamCacheSweepSize {
		c.set(fmt.Sprintf("acme/team/user-%d", i), true)
	}
	require.Len(t, c.entries, teamCacheSweepSize)

	// Still fresh: the sweep runs but removes nothing.
	c.set("acme/team/fresh", true)
	require.Len(t, c.entries, teamCacheSweepSize+1)

	now = now.Add(2 * time.Minute)
	c.set("acme/team/later", true)
	require.Len(t, c.entries, 1)
	member, ok := c.get("acme/team/later")
	require.True(t, ok)
	require.True(t, member)
}
