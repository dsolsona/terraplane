package scm

import (
	"context"
	"net/http"
)

//go:generate mockgen -source=provider.go -destination=mock_scm/mock_provider.go -package=mock_scm

type Provider interface {
	Name() string
	ParseWebhook(r *http.Request) ([]Webhook, error)
	GetFile(fileName string, revision string, repo string) (string, error)
	// TeamMember reports whether user is an active member of org/team.
	TeamMember(ctx context.Context, org string, team string, user string) (bool, error)
	// PullRequestApproved reports whether the latest review from at least one
	// reviewer approves and no reviewer's latest review requests changes.
	PullRequestApproved(ctx context.Context, repo string, prNumber int) (bool, error)
	// ChecksGreen reports whether every status and check run on the commit passed.
	ChecksGreen(ctx context.Context, repo string, sha string) (bool, error)
	GetPullRequest(ctx context.Context, repo string, prNumber int) (PullRequest, error)
	// CommitsBehind reports how many commits on base are missing from head.
	CommitsBehind(ctx context.Context, repo string, base string, head string) (int, error)
}
