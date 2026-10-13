package scm

// RepositoryAccess describes how agents clone repositories, over SSH with a
// deploy key or over HTTPS with a token. Orchestrator-facing Provider handles
// webhooks and API calls; agents only need this subset.
type RepositoryAccess interface {
	Name() string
	SSHHost() string
	CloneURL(slug string) string
	HTTPSCloneURL(slug string) string
}
