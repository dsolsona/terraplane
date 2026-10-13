package github_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xyzjace/terraplane/config"
	"github.com/xyzjace/terraplane/pkg/log"
	"github.com/xyzjace/terraplane/pkg/scm/github"
)

func TestHTTPSCloneURL(t *testing.T) {
	access := github.NewRepositoryAccess(log.Noop(), &config.Config{}, nil)
	require.Equal(t, "https://github.com/acme/infra.git", access.HTTPSCloneURL("acme/infra"))
	require.Equal(t, "git@github.com:acme/infra.git", access.CloneURL("acme/infra"))
}
