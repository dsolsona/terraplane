package orchestratorconfig_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xyzjace/terraplane/config"
	"github.com/xyzjace/terraplane/pkg/orchestratorconfig"
)

const validYAML = `
agents:
  agent-prod:
    token_env: AGENT_TOKEN_PROD
  agent-data:
    token_env: AGENT_TOKEN_DATA

repos:
  Acme/Infra:
    agent: agent-prod
    permissions:
      plan: ["@acme/engineering"]
      apply: [" alice "]
      unlock: ["alice", "bob"]
    stacks:
      reporting:
        dir: ./stacks/reporting/
        agent: agent-data
        permissions:
          plan: ["alice", "carol"]
          apply: ["carol"]
      no-override:
        dir: stacks/no-override
  acme/platform:
    agent: agent-prod
    permissions:
      plan: ["@acme/platform"]
      apply: ["@acme/platform-leads"]
  acme/empty:
    agent: agent-data
`

const minimalYAML = "agents:\n  a:\n    token_env: X\nrepos:\n  acme/infra:\n    agent: a\n"

func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func validEnv() func(string) string {
	return env(map[string]string{
		"AGENT_TOKEN_PROD": "prod-token",
		"AGENT_TOKEN_DATA": "data-token",
	})
}

func parseValid(t *testing.T) *orchestratorconfig.ConfigFile {
	t.Helper()
	cfg, err := orchestratorconfig.Parse([]byte(validYAML), validEnv())
	require.NoError(t, err)
	return cfg
}

func TestParse(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		cfg := parseValid(t)
		require.True(t, cfg.Enabled())
		require.Len(t, cfg.Agents, 2)
		require.Len(t, cfg.Repos, 3)
		require.Equal(t, "AGENT_TOKEN_PROD", cfg.Agents["agent-prod"].TokenEnv)
		require.Equal(t, "agent-data", cfg.Repos["acme/infra"].Stacks["reporting"].Agent)
	})

	t.Run("invalid yaml", func(t *testing.T) {
		cfg, err := orchestratorconfig.Parse([]byte("agents: ["), validEnv())
		require.Error(t, err)
		require.Nil(t, cfg)
		require.Contains(t, err.Error(), "parse orchestrator config")
	})

	t.Run("unknown keys are rejected", func(t *testing.T) {
		_, err := orchestratorconfig.Parse([]byte(`
agents:
  a:
    token_env: X
    secret: nope
repos:
  acme/infra:
    agent: a
`), env(map[string]string{"X": "t"}))
		require.Error(t, err)
		require.Contains(t, err.Error(), "parse orchestrator config")
		require.Contains(t, err.Error(), "secret")
	})

	t.Run("empty document fails validation", func(t *testing.T) {
		cfg, err := orchestratorconfig.Parse([]byte(""), validEnv())
		require.Error(t, err)
		require.Nil(t, cfg)
		require.Contains(t, err.Error(), "no agents configured")
	})

	t.Run("missing token env var", func(t *testing.T) {
		_, err := orchestratorconfig.Parse([]byte(validYAML), env(map[string]string{"AGENT_TOKEN_PROD": "p"}))
		require.Error(t, err)
		require.Contains(t, err.Error(), `agent "agent-data" token env var AGENT_TOKEN_DATA is not set`)
	})

	t.Run("duplicate tokens", func(t *testing.T) {
		_, err := orchestratorconfig.Parse([]byte(validYAML), env(map[string]string{
			"AGENT_TOKEN_PROD": "same",
			"AGENT_TOKEN_DATA": "same",
		}))
		require.Error(t, err)
		require.Contains(t, err.Error(), "share the same token")
	})
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "no agents",
			yaml: "repos:\n  acme/infra:\n    agent: a\n",
			want: "no agents configured",
		},
		{
			name: "empty agent name",
			yaml: "agents:\n  \"\":\n    token_env: X\nrepos:\n  acme/infra:\n    agent: a\n",
			want: "agent name is required",
		},
		{
			name: "agent missing token_env",
			yaml: "agents:\n  a: {}\nrepos:\n  acme/infra:\n    agent: a\n",
			want: `agent "a" is missing token_env`,
		},
		{
			name: "no repos",
			yaml: "agents:\n  a:\n    token_env: X\n",
			want: "no repos configured",
		},
		{
			name: "empty repo name",
			yaml: "agents:\n  a:\n    token_env: X\nrepos:\n  \"\":\n    agent: a\n",
			want: "repo name is required",
		},
		{
			name: "repo missing agent",
			yaml: "agents:\n  a:\n    token_env: X\nrepos:\n  acme/infra: {}\n",
			want: `repo "acme/infra" is missing agent`,
		},
		{
			name: "repo unknown agent",
			yaml: "agents:\n  a:\n    token_env: X\nrepos:\n  acme/infra:\n    agent: nope\n",
			want: `repo "acme/infra" references unknown agent "nope"`,
		},
		{
			name: "apply_requires approved not supported yet",
			yaml: minimalYAML + "    apply_requires:\n      approved: true\n",
			want: `repo "acme/infra": apply_requires is not supported yet`,
		},
		{
			name: "apply_requires checks_green not supported yet",
			yaml: minimalYAML + "    apply_requires:\n      checks_green: true\n",
			want: `repo "acme/infra": apply_requires is not supported yet`,
		},
		{
			name: "repo invalid permission entry",
			yaml: minimalYAML + "    permissions:\n      apply: [\"\"]\n",
			want: `repo "acme/infra" apply permissions: empty entry`,
		},
		{
			name: "empty stack name",
			yaml: minimalYAML + "    stacks:\n      \"\": {dir: x}\n",
			want: `stack name is required in repo "acme/infra"`,
		},
		{
			name: "stack missing dir",
			yaml: minimalYAML + "    stacks:\n      s: {}\n",
			want: `stack "s" in repo "acme/infra" is missing dir`,
		},
		{
			name: "stack absolute dir",
			yaml: minimalYAML + "    stacks:\n      s: {dir: /etc}\n",
			want: `stack "s" in repo "acme/infra" dir "/etc" must be relative`,
		},
		{
			name: "stack dir escapes repository",
			yaml: minimalYAML + "    stacks:\n      s: {dir: ../other}\n",
			want: `stack "s" in repo "acme/infra" dir "../other" must be relative`,
		},
		{
			name: "stack dir escapes repository after cleaning",
			yaml: minimalYAML + "    stacks:\n      s: {dir: a/../..}\n",
			want: `must be relative`,
		},
		{
			name: "two stacks share a dir",
			yaml: minimalYAML + "    stacks:\n      s: {dir: stacks/x}\n      t: {dir: ./stacks/x/}\n",
			want: `share dir "stacks/x"`,
		},
		{
			name: "stack unknown agent",
			yaml: minimalYAML + "    stacks:\n      s:\n        dir: x\n        agent: nope\n",
			want: `stack "s" in repo "acme/infra" references unknown agent "nope"`,
		},
		{
			name: "stack invalid permission entry",
			yaml: minimalYAML + "    stacks:\n      s:\n        dir: x\n        permissions:\n          plan: [\"@acme\"]\n",
			want: `stack "s" in repo "acme/infra" plan permissions: invalid team reference "@acme" (expected @org/team)`,
		},
		{
			name: "repo listed twice with different case",
			yaml: "agents:\n  a:\n    token_env: X\nrepos:\n  acme/infra:\n    agent: a\n  Acme/Infra:\n    agent: a\n",
			want: `repo "acme/infra" is listed more than once`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := orchestratorconfig.Parse([]byte(tt.yaml), env(map[string]string{"X": "t"}))
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestPermissionEntries(t *testing.T) {
	valid := []string{"alice", "alice-1", "@acme/team", "@acme/team-leads"}
	for _, entry := range valid {
		yaml := minimalYAML + "    permissions:\n      plan: [\"" + entry + "\"]\n"
		_, err := orchestratorconfig.Parse([]byte(yaml), env(map[string]string{"X": "t"}))
		require.NoError(t, err, entry)
	}

	invalid := map[string]string{
		"":             "empty entry",
		"   ":          "empty entry",
		"@":            "invalid team reference",
		"@acme":        "invalid team reference",
		"@acme/":       "invalid team reference",
		"@/team":       "invalid team reference",
		"@acme/a/b":    "invalid team reference",
		"acme/team":    "invalid user",
		"alice@x":      "invalid user",
		"alice/branch": "invalid user",
	}
	for entry, want := range invalid {
		yaml := minimalYAML + "    permissions:\n      unlock: [\"" + entry + "\"]\n"
		_, err := orchestratorconfig.Parse([]byte(yaml), env(map[string]string{"X": "t"}))
		require.Error(t, err, entry)
		require.Contains(t, err.Error(), want, entry)
	}
}

func TestAgentByToken(t *testing.T) {
	cfg := parseValid(t)

	name, ok := cfg.AgentByToken("prod-token")
	require.True(t, ok)
	require.Equal(t, "agent-prod", name)

	name, ok = cfg.AgentByToken("data-token")
	require.True(t, ok)
	require.Equal(t, "agent-data", name)

	_, ok = cfg.AgentByToken("other")
	require.False(t, ok)

	_, ok = cfg.AgentByToken("")
	require.False(t, ok)

	_, ok = (&orchestratorconfig.ConfigFile{}).AgentByToken("prod-token")
	require.False(t, ok)
}

func TestHasRepo(t *testing.T) {
	cfg := parseValid(t)
	require.True(t, cfg.HasRepo("acme/infra"))
	require.True(t, cfg.HasRepo("ACME/Infra"), "slugs are case-insensitive")
	require.False(t, cfg.HasRepo("acme/unknown"))
	require.False(t, (&orchestratorconfig.ConfigFile{}).HasRepo("acme/infra"))
}

func TestAgentForStack(t *testing.T) {
	cfg := parseValid(t)

	agent, ok := cfg.AgentForStack("acme/infra", "reporting")
	require.True(t, ok)
	require.Equal(t, "agent-data", agent)

	agent, ok = cfg.AgentForStack("acme/infra", "no-override")
	require.True(t, ok)
	require.Equal(t, "agent-prod", agent)

	agent, ok = cfg.AgentForStack("acme/infra", "not-listed")
	require.True(t, ok)
	require.Equal(t, "agent-prod", agent)

	_, ok = cfg.AgentForStack("acme/unknown", "reporting")
	require.False(t, ok)
}

func TestStackDir(t *testing.T) {
	cfg := parseValid(t)

	dir, ok := cfg.StackDir("acme/infra", "reporting")
	require.True(t, ok)
	require.Equal(t, "stacks/reporting", dir, "dirs are cleaned at load")

	_, ok = cfg.StackDir("acme/infra", "not-listed")
	require.False(t, ok)

	_, ok = cfg.StackDir("acme/unknown", "reporting")
	require.False(t, ok)
}

func TestStackByDir(t *testing.T) {
	cfg := parseValid(t)

	name, ok := cfg.StackByDir("acme/infra", "stacks/reporting/")
	require.True(t, ok)
	require.Equal(t, "reporting", name)

	_, ok = cfg.StackByDir("acme/infra", "stacks/other")
	require.False(t, ok)

	_, ok = cfg.StackByDir("acme/unknown", "stacks/reporting")
	require.False(t, ok)
}

func TestPermissionsFor(t *testing.T) {
	cfg := parseValid(t)

	t.Run("repo level with explicit unlock and trimmed entries", func(t *testing.T) {
		perms, ok := cfg.PermissionsFor("acme/infra", "not-listed")
		require.True(t, ok)
		require.Equal(t, []string{"@acme/engineering"}, perms.Plan)
		require.Equal(t, []string{"alice"}, perms.Apply)
		require.Equal(t, []string{"alice", "bob"}, perms.Unlock)
	})

	t.Run("stack block replaces repo block and unlock defaults to apply", func(t *testing.T) {
		perms, ok := cfg.PermissionsFor("acme/infra", "reporting")
		require.True(t, ok)
		require.Equal(t, []string{"alice", "carol"}, perms.Plan)
		require.Equal(t, []string{"carol"}, perms.Apply)
		require.Equal(t, []string{"carol"}, perms.Unlock)
	})

	t.Run("stack without permissions inherits repo block", func(t *testing.T) {
		perms, ok := cfg.PermissionsFor("acme/infra", "no-override")
		require.True(t, ok)
		require.Equal(t, []string{"alice"}, perms.Apply)
	})

	t.Run("repo without permissions denies everything", func(t *testing.T) {
		perms, ok := cfg.PermissionsFor("acme/empty", "anything")
		require.True(t, ok)
		require.Empty(t, perms.Plan)
		require.Empty(t, perms.Apply)
		require.Empty(t, perms.Unlock)
	})

	t.Run("unknown repo", func(t *testing.T) {
		_, ok := cfg.PermissionsFor("acme/unknown", "reporting")
		require.False(t, ok)
	})
}

func TestApplyRequiresFor(t *testing.T) {
	cfg := parseValid(t)
	require.Equal(t, orchestratorconfig.ApplyRequires{}, cfg.ApplyRequiresFor("acme/platform"))
	require.Equal(t, orchestratorconfig.ApplyRequires{}, cfg.ApplyRequiresFor("acme/unknown"))
}

func TestLoad(t *testing.T) {
	t.Setenv("AGENT_TOKEN_PROD", "prod-token")
	t.Setenv("AGENT_TOKEN_DATA", "data-token")

	path := filepath.Join(t.TempDir(), "orchestrator.yaml")
	require.NoError(t, os.WriteFile(path, []byte(validYAML), 0o644))

	cfg, err := orchestratorconfig.Load(path)
	require.NoError(t, err)
	require.True(t, cfg.Enabled())
	name, ok := cfg.AgentByToken("prod-token")
	require.True(t, ok)
	require.Equal(t, "agent-prod", name)

	_, err = orchestratorconfig.Load(filepath.Join(t.TempDir(), "missing.yaml"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "read orchestrator config")
}

func TestNew(t *testing.T) {
	t.Run("unset path is disabled", func(t *testing.T) {
		cfg, err := orchestratorconfig.New(&config.Config{})
		require.NoError(t, err)
		require.False(t, cfg.Enabled())
		require.False(t, cfg.HasRepo("acme/infra"))
	})

	t.Run("set path loads the file", func(t *testing.T) {
		t.Setenv("AGENT_TOKEN_PROD", "prod-token")
		t.Setenv("AGENT_TOKEN_DATA", "data-token")
		path := filepath.Join(t.TempDir(), "orchestrator.yaml")
		require.NoError(t, os.WriteFile(path, []byte(validYAML), 0o644))

		cfg, err := orchestratorconfig.New(&config.Config{OrchestratorConfigPath: path})
		require.NoError(t, err)
		require.True(t, cfg.Enabled())
	})
}
