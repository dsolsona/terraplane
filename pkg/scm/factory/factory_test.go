package factory_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xyzjace/terraplane/config"
	"github.com/xyzjace/terraplane/pkg/log"
	"github.com/xyzjace/terraplane/pkg/scm/factory"
)

func TestNewRepositoryAccessGitHub(t *testing.T) {
	repoAccess, err := factory.NewRepositoryAccess(log.Noop(), &config.Config{SCMProvider: "github"})
	require.NoError(t, err)
	require.Equal(t, "github", repoAccess.Name())
	require.Equal(t, "github.com", repoAccess.SSHHost())
	require.Equal(t, "git@github.com:acme/infra.git", repoAccess.CloneURL("acme/infra"))
	require.Equal(t, "https://github.com/acme/infra.git", repoAccess.HTTPSCloneURL("acme/infra"))
}

func TestNewProviderGitHub(t *testing.T) {
	provider, err := factory.NewProvider(log.Noop(), &config.Config{SCMProvider: "github"})
	require.NoError(t, err)
	require.Equal(t, "github", provider.Name())
}

func TestNewProviderDefault(t *testing.T) {
	provider, err := factory.NewProvider(log.Noop(), &config.Config{})
	require.NoError(t, err)
	require.Equal(t, "github", provider.Name())
}

func TestNewProviderUnknown(t *testing.T) {
	_, err := factory.NewProvider(log.Noop(), &config.Config{SCMProvider: "bitbucket"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown SCM provider")
}

func TestNewPublisherGitHub(t *testing.T) {
	publisher, err := factory.NewPublisher(log.Noop(), &config.Config{SCMProvider: "github"})
	require.NoError(t, err)
	require.Equal(t, "github", publisher.Name())
}

func TestNewPublisherUnknown(t *testing.T) {
	_, err := factory.NewPublisher(log.Noop(), &config.Config{SCMProvider: "gitlab"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown SCM provider")
}
