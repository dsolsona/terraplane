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
}
