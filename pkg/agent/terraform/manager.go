package terraform

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/xyzjace/terraplane/config"
	"github.com/xyzjace/terraplane/pkg/log"
	"github.com/xyzjace/terraplane/pkg/terraplaneconfig"
)

type Manager interface {
	RunPlan(ctx context.Context, workspaceDir, stackName, tool, toolVersion, planFlags string, live io.Writer) (string, error)
	RunApply(ctx context.Context, workspaceDir, stackName, tool, toolVersion string, live io.Writer) (string, error)
}

//go:generate mockgen -source=manager.go -destination=mock_terraform/mock_manager.go -package=mock_terraform

type manager struct {
	logger          log.Logger
	defaultTool     Tool
	defaultVersions map[Tool]string
	versionManager  VersionManager
	runner          Runner
}

func NewManager(cfg *config.Config, logger log.Logger) (Manager, error) {
	defaultTool, err := ParseTool(cfg.AgentDefaultTool)
	if err != nil {
		return nil, fmt.Errorf("AGENT_DEFAULT_TOOL: %w", err)
	}
	if defaultTool == "" {
		defaultTool = ToolTerraform
	}
	return NewManagerWith(
		logger,
		defaultTool,
		map[Tool]string{
			ToolTerraform: cfg.AgentDefaultTerraformVersion,
			ToolTofu:      cfg.AgentDefaultTofuVersion,
		},
		NewVersionManager(logger, cfg.AgentTerraformBinDir),
		NewRunner(),
	), nil
}

// NewManagerWith constructs a Manager with explicit dependencies (useful in tests).
func NewManagerWith(logger log.Logger, defaultTool Tool, defaultVersions map[Tool]string, versionManager VersionManager, runner Runner) Manager {
	return &manager{
		logger:          logger,
		defaultTool:     defaultTool,
		defaultVersions: defaultVersions,
		versionManager:  versionManager,
		runner:          runner,
	}
}

func (m *manager) RunPlan(ctx context.Context, workspaceDir, stackName, tool, toolVersion, planFlags string, live io.Writer) (string, error) {
	resolved, err := m.resolveStack(workspaceDir, stackName, tool, toolVersion)
	if err != nil {
		return "", err
	}

	toolBin, err := m.versionManager.Ensure(ctx, resolved.tool, resolved.version)
	if err != nil {
		return "", err
	}

	if err := m.runner.Init(ctx, toolBin, resolved.dir, live); err != nil {
		return "", err
	}

	return m.runner.Plan(ctx, toolBin, resolved.dir, planFlags, live)
}

func (m *manager) RunApply(ctx context.Context, workspaceDir, stackName, tool, toolVersion string, live io.Writer) (string, error) {
	resolved, err := m.resolveStack(workspaceDir, stackName, tool, toolVersion)
	if err != nil {
		return "", err
	}

	toolBin, err := m.versionManager.Ensure(ctx, resolved.tool, resolved.version)
	if err != nil {
		return "", err
	}

	return m.runner.Apply(ctx, toolBin, resolved.dir, live)
}

type resolvedStack struct {
	dir     string
	tool    Tool
	version string
}

// resolveStack picks the tool and version for a stack: the job's values win,
// then the stack's terraplane.yaml, then the agent defaults.
func (m *manager) resolveStack(workspaceDir, stackName, tool, toolVersion string) (resolvedStack, error) {
	file, err := os.ReadFile(filepath.Join(workspaceDir, "terraplane.yaml"))
	if err != nil {
		return resolvedStack{}, fmt.Errorf("failed to read terraplane config: %w", err)
	}

	terraplaneConfig, err := terraplaneconfig.ParseConfigFile(file)
	if err != nil {
		return resolvedStack{}, fmt.Errorf("failed to parse terraplane config: %w", err)
	}

	stacks, err := terraplaneConfig.ResolveStacks([]string{stackName}, nil)
	if err != nil {
		return resolvedStack{}, fmt.Errorf("stack %q not found in terraplane config: %w", stackName, err)
	}
	stack := stacks[0]

	resolvedTool, err := ParseTool(tool)
	if err != nil {
		return resolvedStack{}, fmt.Errorf("stack %q: %w", stackName, err)
	}
	if resolvedTool == "" {
		resolvedTool, err = ParseTool(stack.Tool)
		if err != nil {
			return resolvedStack{}, fmt.Errorf("stack %q: %w", stackName, err)
		}
	}
	if resolvedTool == "" {
		resolvedTool = m.defaultTool
	}

	version := strings.TrimSpace(toolVersion)
	if version == "" {
		version = stack.ToolVersion
	}
	if version == "" {
		version = m.defaultVersions[resolvedTool]
	}
	if version == "" {
		return resolvedStack{}, fmt.Errorf("no %s version configured for stack %q and %s is not set", resolvedTool, stackName, defaultVersionEnv(resolvedTool))
	}

	dir, err := resolveStackDir(workspaceDir, stack.Dir)
	if err != nil {
		return resolvedStack{}, fmt.Errorf("stack %q dir %q: %w", stackName, stack.Dir, err)
	}
	return resolvedStack{dir: dir, tool: resolvedTool, version: version}, nil
}

func defaultVersionEnv(tool Tool) string {
	if tool == ToolTofu {
		return "AGENT_DEFAULT_TOFU_VERSION"
	}
	return "AGENT_DEFAULT_TERRAFORM_VERSION"
}

// resolveStackDir joins stackDir onto workspaceDir, rejecting any path that
// escapes the workspace (including via ".." or symlinks). os.Root is used the
// same way workspace provisioning confines AGENT_WORK_DIR.
func resolveStackDir(workspaceDir, stackDir string) (string, error) {
	rel := filepath.Clean(stackDir)
	if rel == "" {
		rel = "."
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("must be relative to the workspace")
	}

	root, err := os.OpenRoot(workspaceDir)
	if err != nil {
		return "", fmt.Errorf("failed to open workspace: %w", err)
	}
	defer func() { _ = root.Close() }()

	info, err := root.Stat(rel)
	if err != nil {
		return "", fmt.Errorf("path escapes workspace or does not exist: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("is not a directory")
	}

	return filepath.Join(workspaceDir, rel), nil
}
