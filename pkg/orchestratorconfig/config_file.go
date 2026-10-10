package orchestratorconfig

import (
	"bytes"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/xyzjace/terraplane/config"
)

// ConfigFile is the orchestrator-side configuration that turns on per-agent
// tokens, orchestrator-owned routing, and authorization. When no file is
// configured the zero value is used and Enabled reports false, which keeps
// the legacy single-team behaviour.
type ConfigFile struct {
	Agents map[string]Agent `yaml:"agents"`
	Repos  map[string]Repo  `yaml:"repos"`

	enabled bool
	tokens  map[string]string // agent name → bearer token resolved from the environment
}

type Agent struct {
	TokenEnv string `yaml:"token_env"`
}

type Repo struct {
	Agent         string           `yaml:"agent"`
	Permissions   *Permissions     `yaml:"permissions,omitempty"`
	Stacks        map[string]Stack `yaml:"stacks,omitempty"`
	ApplyRequires ApplyRequires    `yaml:"apply_requires,omitempty"`
}

type Stack struct {
	Agent       string       `yaml:"agent,omitempty"`
	Permissions *Permissions `yaml:"permissions,omitempty"`
}

// Permissions lists GitHub logins or @org/team slugs allowed to run each action.
type Permissions struct {
	Plan   []string `yaml:"plan"`
	Apply  []string `yaml:"apply"`
	Unlock []string `yaml:"unlock"`
}

type ApplyRequires struct {
	Approved    bool `yaml:"approved"`
	ChecksGreen bool `yaml:"checks_green"`
}

// New loads the file named by ORCHESTRATOR_CONFIG_PATH, or returns a disabled
// config when the variable is unset.
func New(cfg *config.Config) (*ConfigFile, error) {
	if cfg.OrchestratorConfigPath == "" {
		return &ConfigFile{}, nil
	}
	return Load(cfg.OrchestratorConfigPath)
}

func Load(path string) (*ConfigFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read orchestrator config %q: %w", path, err)
	}
	return Parse(data, os.Getenv)
}

// Parse decodes, validates, and resolves agent tokens through getenv.
func Parse(data []byte, getenv func(string) string) (*ConfigFile, error) {
	var cfg ConfigFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse orchestrator config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := cfg.resolveTokens(getenv); err != nil {
		return nil, err
	}
	cfg.enabled = true
	return &cfg, nil
}

// Enabled reports whether a config file was loaded.
func (c *ConfigFile) Enabled() bool {
	return c.enabled
}

// Validate checks structural rules: at least one agent and repo, every agent
// reference resolves, and permission entries are logins or @org/team slugs.
func (c *ConfigFile) Validate() error {
	if len(c.Agents) == 0 {
		return fmt.Errorf("no agents configured")
	}
	for name, agent := range c.Agents {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("agent name is required")
		}
		if strings.TrimSpace(agent.TokenEnv) == "" {
			return fmt.Errorf("agent %q is missing token_env", name)
		}
	}

	if len(c.Repos) == 0 {
		return fmt.Errorf("no repos configured")
	}
	for slug, repo := range c.Repos {
		if strings.TrimSpace(slug) == "" {
			return fmt.Errorf("repo name is required")
		}
		if strings.TrimSpace(repo.Agent) == "" {
			return fmt.Errorf("repo %q is missing agent", slug)
		}
		if _, ok := c.Agents[repo.Agent]; !ok {
			return fmt.Errorf("repo %q references unknown agent %q", slug, repo.Agent)
		}
		if err := repo.Permissions.validate(fmt.Sprintf("repo %q", slug)); err != nil {
			return err
		}
		for stackName, stack := range repo.Stacks {
			if strings.TrimSpace(stackName) == "" {
				return fmt.Errorf("stack name is required in repo %q", slug)
			}
			if stack.Agent != "" {
				if _, ok := c.Agents[stack.Agent]; !ok {
					return fmt.Errorf("stack %q in repo %q references unknown agent %q", stackName, slug, stack.Agent)
				}
			}
			if err := stack.Permissions.validate(fmt.Sprintf("stack %q in repo %q", stackName, slug)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Permissions) validate(where string) error {
	if p == nil {
		return nil
	}
	for action, entries := range map[string][]string{"plan": p.Plan, "apply": p.Apply, "unlock": p.Unlock} {
		for _, entry := range entries {
			if err := validateEntry(entry); err != nil {
				return fmt.Errorf("%s %s permissions: %w", where, action, err)
			}
		}
	}
	return nil
}

// validateEntry accepts a GitHub login or a team slug of the form @org/team.
func validateEntry(entry string) error {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return fmt.Errorf("empty entry")
	}
	if strings.HasPrefix(entry, "@") {
		org, team, ok := strings.Cut(strings.TrimPrefix(entry, "@"), "/")
		if !ok || org == "" || team == "" || strings.Contains(team, "/") {
			return fmt.Errorf("invalid team reference %q (expected @org/team)", entry)
		}
		return nil
	}
	if strings.ContainsAny(entry, "/@") {
		return fmt.Errorf("invalid user %q", entry)
	}
	return nil
}

func (c *ConfigFile) resolveTokens(getenv func(string) string) error {
	c.tokens = make(map[string]string, len(c.Agents))
	byToken := make(map[string]string, len(c.Agents)) // token → agent name
	for name, agent := range c.Agents {
		token := getenv(agent.TokenEnv)
		if token == "" {
			return fmt.Errorf("agent %q token env var %s is not set", name, agent.TokenEnv)
		}
		if other, ok := byToken[token]; ok {
			return fmt.Errorf("agents %q and %q share the same token", other, name)
		}
		byToken[token] = name
		c.tokens[name] = token
	}
	return nil
}

// AgentByToken returns the agent whose token matches. Every configured token
// is compared so the result does not depend on map iteration order.
func (c *ConfigFile) AgentByToken(token string) (string, bool) {
	matched := ""
	for name, candidate := range c.tokens {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(token)) == 1 {
			matched = name
		}
	}
	return matched, matched != ""
}

func (c *ConfigFile) HasRepo(repo string) bool {
	_, ok := c.Repos[repo]
	return ok
}

// AgentForStack returns the agent that runs stack in repo: the stack override
// when set, otherwise the repo default.
func (c *ConfigFile) AgentForStack(repo, stack string) (string, bool) {
	r, ok := c.Repos[repo]
	if !ok {
		return "", false
	}
	if s, ok := r.Stacks[stack]; ok && s.Agent != "" {
		return s.Agent, true
	}
	return r.Agent, true
}

// PermissionsFor returns the permissions that apply to stack in repo. A stack
// block replaces the repo block entirely. Unlock defaults to the apply list
// when omitted. A repo without permissions yields empty lists, which denies
// every action.
func (c *ConfigFile) PermissionsFor(repo, stack string) (Permissions, bool) {
	r, ok := c.Repos[repo]
	if !ok {
		return Permissions{}, false
	}
	perms := r.Permissions
	if s, ok := r.Stacks[stack]; ok && s.Permissions != nil {
		perms = s.Permissions
	}
	if perms == nil {
		return Permissions{}, true
	}
	out := *perms
	if out.Unlock == nil {
		out.Unlock = out.Apply
	}
	return out, true
}

func (c *ConfigFile) ApplyRequiresFor(repo string) ApplyRequires {
	return c.Repos[repo].ApplyRequires
}
