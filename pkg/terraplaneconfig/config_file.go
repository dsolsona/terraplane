package terraplaneconfig

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

type ConfigFile struct {
	Environments []Environment `yaml:"environments"`
}

type Environment struct {
	Name   string  `yaml:"name"`
	Agent  string  `yaml:"agent,omitempty"`
	Tool   string  `yaml:"tool,omitempty"`
	Stacks []Stack `yaml:"stacks"`
}

type Stack struct {
	Name        string `yaml:"name"`
	Agent       string `yaml:"agent,omitempty"`
	Dir         string `yaml:"dir"`
	Tool        string `yaml:"tool,omitempty"`
	ToolVersion string `yaml:"tool_version,omitempty"`
}

// ResolvedStack is a stack with environment inheritance applied (agent, tool).
type ResolvedStack struct {
	Environment string
	Name        string
	Agent       string
	Dir         string
	Tool        string
	ToolVersion string
}

// Tools terraplane can run a stack with. Empty leaves the choice to the agent.
const (
	ToolTerraform = "terraform"
	ToolTofu      = "tofu"
)

func ParseConfigFile(data []byte) (*ConfigFile, error) {
	var cfg ConfigFile
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse terraplane config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate checks structural rules: unique stack names and required fields.
// Agent assignment is checked separately by ValidateAgents because the
// orchestrator may own routing instead of this file.
func (c *ConfigFile) Validate() error {
	if len(c.Environments) == 0 {
		return fmt.Errorf("no environments configured")
	}

	seenEnvs := make(map[string]struct{}, len(c.Environments))
	seenStacks := make(map[string]string) // stack name → environment name

	for _, env := range c.Environments {
		name := strings.TrimSpace(env.Name)
		if name == "" {
			return fmt.Errorf("environment name is required")
		}
		if hasControlChars(name) {
			return fmt.Errorf("environment name %q contains control characters", name)
		}
		if _, ok := seenEnvs[name]; ok {
			return fmt.Errorf("duplicate environment name %q", name)
		}
		seenEnvs[name] = struct{}{}

		if len(env.Stacks) == 0 {
			return fmt.Errorf("environment %q has no stacks", name)
		}
		if err := validateTool(env.Tool); err != nil {
			return fmt.Errorf("environment %q: %w", name, err)
		}

		for _, stack := range env.Stacks {
			stackName := strings.TrimSpace(stack.Name)
			if stackName == "" {
				return fmt.Errorf("stack name is required in environment %q", name)
			}
			if hasControlChars(stackName) {
				return fmt.Errorf("stack name %q in environment %q contains control characters", stackName, name)
			}
			if strings.TrimSpace(stack.Dir) == "" {
				return fmt.Errorf("stack %q in environment %q is missing dir", stackName, name)
			}
			if hasControlChars(stack.Dir) {
				return fmt.Errorf("stack %q in environment %q dir contains control characters", stackName, name)
			}
			if err := validateTool(stack.Tool); err != nil {
				return fmt.Errorf("stack %q in environment %q: %w", stackName, name, err)
			}
			if prevEnv, ok := seenStacks[stackName]; ok {
				return fmt.Errorf("stack name %q is not unique (found in environments %q and %q)", stackName, prevEnv, name)
			}
			seenStacks[stackName] = name
		}
	}
	return nil
}

// ValidateAgents checks that every stack resolves to an agent via stack or
// environment config. Only required when terraplane.yaml owns routing.
func (c *ConfigFile) ValidateAgents() error {
	for _, stack := range c.allResolved() {
		if stack.Agent == "" {
			return fmt.Errorf("stack %q in environment %q has no agent (set agent on the stack or environment)", stack.Name, stack.Environment)
		}
	}
	return nil
}

func (c *ConfigFile) allResolved() []ResolvedStack {
	out := make([]ResolvedStack, 0)
	for _, env := range c.Environments {
		for _, stack := range env.Stacks {
			agent := strings.TrimSpace(stack.Agent)
			if agent == "" {
				agent = strings.TrimSpace(env.Agent)
			}
			tool := strings.ToLower(strings.TrimSpace(stack.Tool))
			if tool == "" {
				tool = strings.ToLower(strings.TrimSpace(env.Tool))
			}
			out = append(out, ResolvedStack{
				Environment: strings.TrimSpace(env.Name),
				Name:        strings.TrimSpace(stack.Name),
				Agent:       agent,
				Dir:         strings.TrimSpace(stack.Dir),
				Tool:        tool,
				ToolVersion: strings.TrimSpace(stack.ToolVersion),
			})
		}
	}
	return out
}

// ResolveStacks returns stacks for the given stack and/or environment selectors.
//
//   - neither set → all stacks
//   - only environments → all stacks in those environments
//   - only stacks → those stacks
//   - both → named stacks that belong to one of the named environments
func (c *ConfigFile) ResolveStacks(stackNames, environmentNames []string) ([]ResolvedStack, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}

	all := c.allResolved()
	byName := make(map[string]ResolvedStack, len(all))
	byEnv := make(map[string][]ResolvedStack, len(c.Environments))
	for _, stack := range all {
		byName[stack.Name] = stack
		byEnv[stack.Environment] = append(byEnv[stack.Environment], stack)
	}

	if len(stackNames) == 0 && len(environmentNames) == 0 {
		return all, nil
	}

	if len(environmentNames) > 0 {
		for _, envName := range environmentNames {
			if _, ok := byEnv[envName]; !ok {
				return nil, fmt.Errorf("unknown environment %q", envName)
			}
		}
	}

	var resolved []ResolvedStack
	seen := make(map[string]struct{})

	add := func(stack ResolvedStack) {
		if _, ok := seen[stack.Name]; ok {
			return
		}
		seen[stack.Name] = struct{}{}
		resolved = append(resolved, stack)
	}

	switch {
	case len(environmentNames) > 0 && len(stackNames) == 0:
		for _, envName := range environmentNames {
			for _, stack := range byEnv[envName] {
				add(stack)
			}
		}
	case len(environmentNames) == 0 && len(stackNames) > 0:
		for _, name := range stackNames {
			stack, ok := byName[name]
			if !ok {
				return nil, fmt.Errorf("unknown stack %q", name)
			}
			add(stack)
		}
	default:
		allowed := make(map[string]struct{})
		for _, envName := range environmentNames {
			for _, stack := range byEnv[envName] {
				allowed[stack.Name] = struct{}{}
			}
		}
		for _, name := range stackNames {
			stack, ok := byName[name]
			if !ok {
				return nil, fmt.Errorf("unknown stack %q", name)
			}
			if _, ok := allowed[name]; !ok {
				return nil, fmt.Errorf("stack %q is not in environment%s %s", name, plural(len(environmentNames)), quoteList(environmentNames))
			}
			add(stack)
		}
	}

	return resolved, nil
}

func validateTool(tool string) error {
	switch strings.ToLower(strings.TrimSpace(tool)) {
	case "", ToolTerraform, ToolTofu:
		return nil
	default:
		return fmt.Errorf("unsupported tool %q (expected %s or %s)", tool, ToolTerraform, ToolTofu)
	}
}

// hasControlChars reports whether s contains characters that could break out
// of the markdown terraplane writes names into (newlines, tabs, escapes).
func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func quoteList(values []string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = fmt.Sprintf("%q", v)
	}
	return strings.Join(parts, ", ")
}
