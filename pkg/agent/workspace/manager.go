package workspace

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/xyzjace/terraplane/config"
	"github.com/xyzjace/terraplane/internal/process"
	"github.com/xyzjace/terraplane/pkg/log"
	"github.com/xyzjace/terraplane/pkg/scm"
)

const (
	// CloneAuthSSH clones over SSH with the deploy key at AGENT_SCM_SSH_KEY_PATH.
	CloneAuthSSH = "ssh"
	// CloneAuthApp clones over HTTPS with a per-job token issued by the orchestrator.
	CloneAuthApp = "app"
)

type Manager interface {
	ProvisionWorkspace(ctx context.Context, jobID string, repo string, revision string, stack string) (string, error)
	FetchWorkspace(ctx context.Context, repo string, revision string, stack string) (string, error)
	RemoveWorkspace(ctx context.Context, path string) error
}

// CloneTokenSource hands out the token for one job's repository. The agent's
// orchestrator client satisfies it.
type CloneTokenSource interface {
	CloneToken(ctx context.Context, jobID string, agentID string) (string, error)
}

//go:generate mockgen -source=manager.go -destination=mock_workspace/mock_manager.go -package=mock_workspace

type manager struct {
	logger     log.Logger
	repoAccess scm.RepositoryAccess
	tokens     CloneTokenSource
	cloneAuth  string
	agentID    string
	sshKeyPath string
	workDir    string
	runner     process.Runner
}

func NewManager(cfg *config.Config, logger log.Logger, repoAccess scm.RepositoryAccess, tokens CloneTokenSource) (Manager, error) {
	return NewManagerWith(cfg, logger, repoAccess, tokens, process.OSRunner{})
}

// NewManagerWith is like NewManager but allows injecting a Command Runner (useful in tests).
func NewManagerWith(cfg *config.Config, logger log.Logger, repoAccess scm.RepositoryAccess, tokens CloneTokenSource, runner process.Runner) (Manager, error) {
	cloneAuth := cfg.AgentSCMAuth
	if cloneAuth == "" {
		cloneAuth = CloneAuthSSH
	}
	switch cloneAuth {
	case CloneAuthSSH, CloneAuthApp:
	default:
		return nil, fmt.Errorf("AGENT_SCM_AUTH must be %q or %q, got %q", CloneAuthSSH, CloneAuthApp, cfg.AgentSCMAuth)
	}
	if runner == nil {
		runner = process.OSRunner{}
	}
	return &manager{
		logger:     logger,
		repoAccess: repoAccess,
		tokens:     tokens,
		cloneAuth:  cloneAuth,
		agentID:    cfg.AgentID,
		sshKeyPath: cfg.AgentSCMSSHKeyPath,
		workDir:    cfg.AgentWorkDir,
		runner:     runner,
	}, nil
}

func (m *manager) ProvisionWorkspace(ctx context.Context, jobID string, repo string, revision string, stack string) (string, error) {
	if m.cloneAuth == CloneAuthSSH {
		if m.sshKeyPath == "" {
			return "", fmt.Errorf("AGENT_SCM_SSH_KEY_PATH is not configured; SSH is required to clone private repositories")
		}
		if _, err := os.Stat(m.sshKeyPath); err != nil {
			return "", fmt.Errorf("SSH key not found at %q: %w", m.sshKeyPath, err)
		}
	}

	if m.workDir == "" {
		return "", fmt.Errorf("AGENT_WORK_DIR is not configured; a work directory is required to provision workspaces")
	}
	if err := os.MkdirAll(m.workDir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create work directory %q: %w", m.workDir, err)
	}

	// os.Root confines every filesystem operation below to m.workDir. Any repo,
	// revision, or stack value that would otherwise escape the work directory
	// (path separators, "..", symlinks) fails safely instead of touching the
	// host filesystem, so no manual path sanitization is required.
	root, err := os.OpenRoot(m.workDir)
	if err != nil {
		return "", fmt.Errorf("failed to open work directory %q: %w", m.workDir, err)
	}
	defer func() { _ = root.Close() }()

	dirName := workspaceDirName(repo, revision, stack)
	repoDirPath := filepath.Join(m.workDir, dirName)

	if info, err := root.Stat(dirName); err == nil {
		if info.IsDir() && workspaceReady(repoDirPath) {
			m.logger.Info(
				"Using existing workspace",
				"repo", repo,
				"revision", revision,
				"stack", stack,
				"path", repoDirPath,
			)
			return repoDirPath, nil
		}

		m.logger.Info(
			"Removing incomplete workspace",
			"repo", repo,
			"revision", revision,
			"stack", stack,
			"path", repoDirPath,
		)
		if err := root.RemoveAll(dirName); err != nil {
			return "", fmt.Errorf("failed to remove incomplete workspace %q: %w", repoDirPath, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("failed to stat workspace directory %q: %w", repoDirPath, err)
	}

	m.pruneStaleWorkspaces(root, repo, stack, dirName)

	if err := root.Mkdir(dirName, 0o755); err != nil {
		return "", fmt.Errorf("failed to create workspace directory %q: %w", repoDirPath, err)
	}

	ok := false
	defer func() {
		if !ok {
			_ = root.RemoveAll(dirName)
		}
	}()

	ok, err = m.cloneRepo(ctx, jobID, repo, revision, repoDirPath)
	if err != nil {
		return "", fmt.Errorf("failed to clone repository %s at revision %s: %w", repo, revision, err)
	}

	return repoDirPath, nil
}

func (m *manager) FetchWorkspace(ctx context.Context, repo string, revision string, stack string) (string, error) {
	if m.workDir == "" {
		return "", fmt.Errorf("AGENT_WORK_DIR is not configured; a work directory is required to fetch workspaces")
	}

	root, err := os.OpenRoot(m.workDir)
	if err != nil {
		return "", fmt.Errorf("failed to open work directory %q: %w", m.workDir, err)
	}
	defer func() { _ = root.Close() }()

	dirName := workspaceDirName(repo, revision, stack)
	repoDirPath := filepath.Join(m.workDir, dirName)

	info, err := root.Stat(dirName)
	if err != nil {
		return "", fmt.Errorf("failed to stat workspace directory %q: %w", repoDirPath, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace path %q is not a directory", repoDirPath)
	}
	if !workspaceReady(repoDirPath) {
		return "", fmt.Errorf("workspace %q is not ready", repoDirPath)
	}

	return repoDirPath, nil
}

func (m *manager) cloneRepo(ctx context.Context, jobID string, repo string, revision string, repoDirPath string) (bool, error) {
	var repoURL string
	var env []string
	var err error
	switch m.cloneAuth {
	case CloneAuthApp:
		repoURL, env, err = m.tokenCloneEnv(ctx, jobID, repo, revision)
	default:
		repoURL, env, err = m.sshCloneEnv(ctx, repo, repoDirPath)
	}
	if err != nil {
		return false, err
	}

	if err := m.runGit(ctx, repoDirPath, env, "init"); err != nil {
		return false, fmt.Errorf("failed to initialize git repository: %w", err)
	}
	if err := m.runGit(ctx, repoDirPath, env, "remote", "add", "origin", repoURL); err != nil {
		return false, fmt.Errorf("failed to add git remote: %w", err)
	}
	if err := m.runGit(ctx, repoDirPath, env, "fetch", "--depth", "1", "origin", revision); err != nil {
		return false, fmt.Errorf("failed to fetch revision %s from %s: %w", revision, repo, err)
	}
	if err := m.runGit(ctx, repoDirPath, env, "checkout", revision); err != nil {
		return false, fmt.Errorf("failed to checkout revision %s: %w", revision, err)
	}

	return true, nil
}

func (m *manager) sshCloneEnv(ctx context.Context, repo string, repoDirPath string) (string, []string, error) {
	host := m.repoAccess.SSHHost()
	knownHostsPath := filepath.Join(repoDirPath, "known_hosts")
	if err := m.scanHostKeys(ctx, host, knownHostsPath); err != nil {
		return "", nil, err
	}

	gitSSH := fmt.Sprintf(
		"ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=%s",
		m.sshKeyPath,
		knownHostsPath,
	)
	return m.repoAccess.CloneURL(repo), []string{"GIT_SSH_COMMAND=" + gitSSH}, nil
}

// tokenCloneEnv fetches the job's token and hands it to git as an
// Authorization header through environment-based config. That keeps the
// token off the command line, out of .git/config, and off disk; it lives
// only in this process and the git processes it spawns.
func (m *manager) tokenCloneEnv(ctx context.Context, jobID string, repo string, revision string) (string, []string, error) {
	token, err := m.tokens.CloneToken(ctx, jobID, m.agentID)
	if err != nil {
		return "", nil, fmt.Errorf("failed to obtain clone token for repository %s: %w", repo, err)
	}

	repoURL := m.repoAccess.HTTPSCloneURL(repo)
	parsed, err := url.Parse(repoURL)
	if err != nil {
		return "", nil, fmt.Errorf("failed to parse clone URL for repository %s: %w", repo, err)
	}

	m.logger.Info("Cloning with an orchestrator-issued token", "repo", repo, "revision", revision)
	credentials := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	env := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1",
		fmt.Sprintf("GIT_CONFIG_KEY_0=http.%s://%s/.extraheader", parsed.Scheme, parsed.Host),
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + credentials,
	}
	return repoURL, env, nil
}

func (m *manager) scanHostKeys(ctx context.Context, host, path string) error {
	result, err := m.runner.Run(ctx, process.Command{
		Name: "ssh-keyscan",
		Args: []string{host},
	})
	if err != nil {
		return fmt.Errorf("failed to run ssh-keyscan for %s: %w", host, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("ssh-keyscan for %s failed with exit code %d: %s", host, result.ExitCode, process.Output(result))
	}
	if strings.TrimSpace(result.Stdout) == "" {
		return fmt.Errorf("ssh-keyscan for %s returned no host keys", host)
	}
	if err := os.WriteFile(path, []byte(result.Stdout), 0o600); err != nil {
		return fmt.Errorf("failed to write known hosts file %q: %w", path, err)
	}
	return nil
}

func (m *manager) runGit(ctx context.Context, dir string, env []string, args ...string) error {
	result, err := m.runner.Run(ctx, process.Command{
		Name: "git",
		Args: args,
		Dir:  dir,
		Env:  env,
	})
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("git %s exited with code %d: %s", strings.Join(args, " "), result.ExitCode, process.Output(result))
	}
	return nil
}

func workspaceReady(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "terraplane.yaml")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return false
	}
	return true
}

var pathSlug = strings.NewReplacer("/", "-", `\`, "-")

func workspaceDirName(repo, revision, stack string) string {
	return fmt.Sprintf(
		"terraplane-workspace-%s-%s-%s",
		pathSlug.Replace(repo),
		pathSlug.Replace(revision),
		pathSlug.Replace(stack),
	)
}

func (m *manager) pruneStaleWorkspaces(root *os.Root, repo, stack, keepDirName string) {
	prefix := fmt.Sprintf("terraplane-workspace-%s-", pathSlug.Replace(repo))
	suffix := fmt.Sprintf("-%s", pathSlug.Replace(stack))

	dir, err := root.Open(".")
	if err != nil {
		m.logger.Error("Failed to open work directory for pruning", "error", err)
		return
	}
	defer func() { _ = dir.Close() }()

	entries, err := dir.ReadDir(-1)
	if err != nil {
		m.logger.Error("Failed to list workspaces for pruning", "error", err)
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == keepDirName {
			continue
		}
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}

		m.logger.Info("Removing stale workspace", "path", filepath.Join(m.workDir, name))
		if err := root.RemoveAll(name); err != nil {
			m.logger.Error("Failed to remove stale workspace", "name", name, "error", err)
		}
	}
}

func (m *manager) RemoveWorkspace(ctx context.Context, path string) error {
	if path == "" {
		return nil
	}
	err := os.RemoveAll(path)
	if err != nil {
		return fmt.Errorf("failed to remove workspace: %w", err)
	}
	return nil
}
