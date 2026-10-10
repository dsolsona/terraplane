package services

import (
	"context"
	"fmt"

	"github.com/xyzjace/terraplane/pkg/authz"
	"github.com/xyzjace/terraplane/pkg/feedback"
	"github.com/xyzjace/terraplane/pkg/log"
	"github.com/xyzjace/terraplane/pkg/orchestratorconfig"
	"github.com/xyzjace/terraplane/pkg/scm"
	"github.com/xyzjace/terraplane/pkg/terraplaneconfig"
)

// authorizeStacks splits stacks into those user may run action on and the
// reasons for the rest. A lookup failure is returned as an error so callers
// fail closed.
func authorizeStacks(
	ctx context.Context,
	logger log.Logger,
	authorizer authz.Authorizer,
	user, repo string,
	prNumber int,
	stacks []terraplaneconfig.ResolvedStack,
	action authz.Action,
) ([]terraplaneconfig.ResolvedStack, []string, error) {
	var allowed []terraplaneconfig.ResolvedStack
	var denied []string
	for _, stack := range stacks {
		ok, reason, err := authorizer.Allowed(ctx, user, repo, stack.Name, action)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to authorize %s of stack %q in repository %s for %s: %w", action, stack.Name, repo, user, err)
		}
		if !ok {
			logger.Warn(
				"Denied command",
				"repo", repo,
				"pr", prNumber,
				"user", user,
				"stack", stack.Name,
				"action", action,
				"reason", reason,
			)
			denied = append(denied, reason)
			continue
		}
		allowed = append(allowed, stack)
	}
	return allowed, denied, nil
}

// bindStackDirs drops stacks whose directory disagrees with the orchestrator
// config. Permissions and agents are keyed by stack name while the pull
// request controls which directory a name maps to, so a listed stack must
// keep its pinned dir and no other name may claim that dir.
func bindStackDirs(
	logger log.Logger,
	orchestratorConfig *orchestratorconfig.ConfigFile,
	repo string,
	prNumber int,
	stacks []terraplaneconfig.ResolvedStack,
) ([]terraplaneconfig.ResolvedStack, []string) {
	if !orchestratorConfig.Enabled() {
		return stacks, nil
	}

	var kept []terraplaneconfig.ResolvedStack
	var denied []string
	for _, stack := range stacks {
		pinned, namePinned := orchestratorConfig.StackDir(repo, stack.Name)
		owner, dirPinned := orchestratorConfig.StackByDir(repo, stack.Dir)

		reason := ""
		switch {
		case namePinned && (!dirPinned || owner != stack.Name):
			reason = fmt.Sprintf("stack %s is pinned to dir %s in the orchestrator config but the pull request maps it to %s", stack.Name, pinned, stack.Dir)
		case !namePinned && dirPinned:
			reason = fmt.Sprintf("dir %s belongs to stack %s in the orchestrator config and cannot be used by stack %s", stack.Dir, owner, stack.Name)
		}
		if reason != "" {
			logger.Warn(
				"Denied stack whose dir does not match the orchestrator config",
				"repo", repo,
				"pr", prNumber,
				"stack", stack.Name,
				"dir", stack.Dir,
				"reason", reason,
			)
			denied = append(denied, reason)
			continue
		}
		kept = append(kept, stack)
	}
	return kept, denied
}

// publishDeniedComment writes one PR comment listing every denied stack. Failures are logged.
func publishDeniedComment(ctx context.Context, logger log.Logger, publisher scm.Publisher, repo string, prNumber int, action authz.Action, reasons []string) {
	comment := feedback.DeniedComment(string(action), reasons)
	if _, err := publisher.WriteComment(ctx, repo, prNumber, comment); err != nil {
		logger.Error(
			"Failed to write denied comment",
			"repo", repo,
			"pr", prNumber,
			"action", action,
			"error", err,
		)
	}
}
