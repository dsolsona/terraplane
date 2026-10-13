package github

import "fmt"

// HTTPSCloneURL is the clone URL agents use with an orchestrator-issued
// token; credentials travel in a header, never in the URL.
func (p *provider) HTTPSCloneURL(slug string) string {
	return fmt.Sprintf("https://github.com/%s.git", slug)
}
