package terraplaneconfig_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/xyzjace/terraplane/pkg/terraplaneconfig"
)

const validYAML = `
environments:
  - name: staging
    agent: agent-dev
    stacks:
      - name: a
        dir: stacks/a
        tool_version: "1.9.0"
      - name: b
        agent: agent-special
        dir: stacks/b
  - name: production
    agent: agent-prod
    stacks:
      - name: c
        dir: stacks/c
`

func TestParseConfigFile(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		cfg, err := terraplaneconfig.ParseConfigFile([]byte(validYAML))
		require.NoError(t, err)
		require.Len(t, cfg.Environments, 2)
		require.Equal(t, "staging", cfg.Environments[0].Name)
		require.Equal(t, "agent-dev", cfg.Environments[0].Agent)
		require.Equal(t, "a", cfg.Environments[0].Stacks[0].Name)
		require.Equal(t, "1.9.0", cfg.Environments[0].Stacks[0].ToolVersion)
		require.Equal(t, "agent-special", cfg.Environments[0].Stacks[1].Agent)
	})

	t.Run("invalid yaml", func(t *testing.T) {
		cfg, err := terraplaneconfig.ParseConfigFile([]byte("environments: ["))
		require.Error(t, err)
		require.Nil(t, cfg)
		require.Contains(t, err.Error(), "parse terraplane config")
	})

	t.Run("empty document fails validation", func(t *testing.T) {
		cfg, err := terraplaneconfig.ParseConfigFile([]byte(""))
		require.Error(t, err)
		require.Nil(t, cfg)
		require.Contains(t, err.Error(), "no environments configured")
	})

	t.Run("duplicate stack names", func(t *testing.T) {
		_, err := terraplaneconfig.ParseConfigFile([]byte(`
environments:
  - name: staging
    agent: agent-dev
    stacks:
      - name: shared
        dir: a
  - name: production
    agent: agent-prod
    stacks:
      - name: shared
        dir: b
`))
		require.Error(t, err)
		require.Contains(t, err.Error(), `stack name "shared" is not unique`)
	})

	t.Run("missing agent parses but fails ValidateAgents", func(t *testing.T) {
		cfg, err := terraplaneconfig.ParseConfigFile([]byte(`
environments:
  - name: staging
    stacks:
      - name: a
        dir: stacks/a
`))
		require.NoError(t, err)
		err = cfg.ValidateAgents()
		require.Error(t, err)
		require.Contains(t, err.Error(), `stack "a" in environment "staging" has no agent`)

		valid, err := terraplaneconfig.ParseConfigFile([]byte(validYAML))
		require.NoError(t, err)
		require.NoError(t, valid.ValidateAgents())
	})

	t.Run("missing dir", func(t *testing.T) {
		_, err := terraplaneconfig.ParseConfigFile([]byte(`
environments:
  - name: staging
    agent: agent-dev
    stacks:
      - name: a
`))
		require.Error(t, err)
		require.Contains(t, err.Error(), "missing dir")
	})

	t.Run("duplicate environment names", func(t *testing.T) {
		_, err := terraplaneconfig.ParseConfigFile([]byte(`
environments:
  - name: staging
    agent: agent-dev
    stacks:
      - name: a
        dir: stacks/a
  - name: staging
    agent: agent-prod
    stacks:
      - name: b
        dir: stacks/b
`))
		require.Error(t, err)
		require.Contains(t, err.Error(), `duplicate environment name "staging"`)
	})

	t.Run("empty environment name", func(t *testing.T) {
		_, err := terraplaneconfig.ParseConfigFile([]byte(`
environments:
  - name: " "
    agent: agent-dev
    stacks:
      - name: a
        dir: stacks/a
`))
		require.Error(t, err)
		require.Contains(t, err.Error(), "environment name is required")
	})

	t.Run("environment with no stacks", func(t *testing.T) {
		_, err := terraplaneconfig.ParseConfigFile([]byte(`
environments:
  - name: staging
    agent: agent-dev
    stacks: []
`))
		require.Error(t, err)
		require.Contains(t, err.Error(), `environment "staging" has no stacks`)
	})

	t.Run("empty stack name", func(t *testing.T) {
		_, err := terraplaneconfig.ParseConfigFile([]byte(`
environments:
  - name: staging
    agent: agent-dev
    stacks:
      - name: ""
        dir: stacks/a
`))
		require.Error(t, err)
		require.Contains(t, err.Error(), "stack name is required")
	})
}

func TestResolveStacks(t *testing.T) {
	cfg, err := terraplaneconfig.ParseConfigFile([]byte(validYAML))
	require.NoError(t, err)

	t.Run("invalid config", func(t *testing.T) {
		got, err := (&terraplaneconfig.ConfigFile{}).ResolveStacks(nil, nil)
		require.Error(t, err)
		require.Nil(t, got)
		require.Contains(t, err.Error(), "no environments configured")
	})

	t.Run("no selectors returns all", func(t *testing.T) {
		got, err := cfg.ResolveStacks(nil, nil)
		require.NoError(t, err)
		require.Len(t, got, 3)
		require.Equal(t, "agent-dev", got[0].Agent)
		require.Equal(t, "staging", got[0].Environment)
		require.Equal(t, "agent-special", got[1].Agent)
		require.Equal(t, "agent-prod", got[2].Agent)
		require.Equal(t, "1.9.0", got[0].ToolVersion)
		require.Empty(t, got[1].ToolVersion)
	})

	t.Run("by environment", func(t *testing.T) {
		got, err := cfg.ResolveStacks(nil, []string{"staging"})
		require.NoError(t, err)
		require.Len(t, got, 2)
		require.Equal(t, "a", got[0].Name)
		require.Equal(t, "b", got[1].Name)
	})

	t.Run("by stack", func(t *testing.T) {
		got, err := cfg.ResolveStacks([]string{"c", "a"}, nil)
		require.NoError(t, err)
		require.Len(t, got, 2)
		require.Equal(t, "c", got[0].Name)
		require.Equal(t, "a", got[1].Name)
	})

	t.Run("environment and stack intersection", func(t *testing.T) {
		got, err := cfg.ResolveStacks([]string{"a"}, []string{"staging"})
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, "a", got[0].Name)
	})

	t.Run("stack not in environment", func(t *testing.T) {
		got, err := cfg.ResolveStacks([]string{"c"}, []string{"staging"})
		require.Error(t, err)
		require.Nil(t, got)
		require.Contains(t, err.Error(), `stack "c" is not in environment "staging"`)
	})

	t.Run("stack not in multiple environments", func(t *testing.T) {
		// Three envs so we can select two that exclude stack c.
		multi, err := terraplaneconfig.ParseConfigFile([]byte(`
environments:
  - name: staging
    agent: agent-dev
    stacks:
      - name: a
        dir: stacks/a
  - name: sandbox
    agent: agent-dev
    stacks:
      - name: b
        dir: stacks/b
  - name: production
    agent: agent-prod
    stacks:
      - name: c
        dir: stacks/c
`))
		require.NoError(t, err)
		got, err := multi.ResolveStacks([]string{"c"}, []string{"staging", "sandbox"})
		require.Error(t, err)
		require.Nil(t, got)
		require.Contains(t, err.Error(), `stack "c" is not in environments "staging", "sandbox"`)
	})

	t.Run("unknown environment", func(t *testing.T) {
		got, err := cfg.ResolveStacks(nil, []string{"nope"})
		require.Error(t, err)
		require.Nil(t, got)
		require.Contains(t, err.Error(), `unknown environment "nope"`)
	})

	t.Run("unknown stack in intersection", func(t *testing.T) {
		got, err := cfg.ResolveStacks([]string{"missing"}, []string{"staging"})
		require.Error(t, err)
		require.Nil(t, got)
		require.Contains(t, err.Error(), `unknown stack "missing"`)
	})

	t.Run("unknown stack", func(t *testing.T) {
		got, err := cfg.ResolveStacks([]string{"missing"}, nil)
		require.Error(t, err)
		require.Nil(t, got)
		require.Contains(t, err.Error(), `unknown stack "missing"`)
	})

	t.Run("intersection across environments", func(t *testing.T) {
		got, err := cfg.ResolveStacks([]string{"a"}, []string{"staging", "production"})
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.Equal(t, "a", got[0].Name)
	})

	t.Run("dedupes repeated stack selectors", func(t *testing.T) {
		got, err := cfg.ResolveStacks([]string{"a", "a"}, nil)
		require.NoError(t, err)
		require.Len(t, got, 1)
	})

	t.Run("dedupes repeated environment selectors", func(t *testing.T) {
		got, err := cfg.ResolveStacks(nil, []string{"staging", "staging"})
		require.NoError(t, err)
		require.Len(t, got, 2)
	})
}

func TestValidateRejectsControlCharacters(t *testing.T) {
	cases := map[string]string{
		"environment name": "environments:\n  - name: \"stag\\ning\"\n    agent: a\n    stacks:\n      - name: a\n        dir: stacks/a\n",
		"stack name":       "environments:\n  - name: staging\n    agent: a\n    stacks:\n      - name: \"a\\n## pwned\"\n        dir: stacks/a\n",
		"stack dir":        "environments:\n  - name: staging\n    agent: a\n    stacks:\n      - name: a\n        dir: \"stacks/a\\tb\"\n",
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := terraplaneconfig.ParseConfigFile([]byte(yaml))
			require.Error(t, err)
			require.Contains(t, err.Error(), "contains control characters")
		})
	}
}

func TestToolSelection(t *testing.T) {
	cfg, err := terraplaneconfig.ParseConfigFile([]byte(`
environments:
  - name: staging
    agent: agent-dev
    tool: Tofu
    stacks:
      - name: inherits
        dir: stacks/a
      - name: overrides
        dir: stacks/b
        tool: terraform
  - name: production
    agent: agent-prod
    stacks:
      - name: unset
        dir: stacks/c
`))
	require.NoError(t, err)
	got, err := cfg.ResolveStacks(nil, nil)
	require.NoError(t, err)
	byName := map[string]string{}
	for _, stack := range got {
		byName[stack.Name] = stack.Tool
	}
	require.Equal(t, map[string]string{"inherits": "tofu", "overrides": "terraform", "unset": ""}, byName)

	_, err = terraplaneconfig.ParseConfigFile([]byte(`
environments:
  - name: staging
    agent: agent-dev
    tool: pulumi
    stacks:
      - name: a
        dir: stacks/a
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), `environment "staging": unsupported tool "pulumi"`)

	_, err = terraplaneconfig.ParseConfigFile([]byte(`
environments:
  - name: staging
    agent: agent-dev
    stacks:
      - name: a
        dir: stacks/a
        tool: cdk
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), `stack "a" in environment "staging": unsupported tool "cdk"`)
}
