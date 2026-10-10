package services

import (
	"context"
	"fmt"

	"github.com/xyzjace/terraplane/pkg/authz"
	"github.com/xyzjace/terraplane/pkg/feedback"
	"github.com/xyzjace/terraplane/pkg/log"
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
