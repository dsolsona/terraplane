package scm

// IssueComment is the part of a pull request comment the orchestrator trusts
// over the webhook body: what was said, by whom, and on which issue.
type IssueComment struct {
	ID        int
	Body      string
	UserLogin string
	IssueURL  string
}

// PullRequest is the pull request state the apply gates look at.
type PullRequest struct {
	Open           bool
	Draft          bool
	MergeableState string // GitHub: clean, unstable, has_hooks, dirty, blocked, behind, draft, unknown
	HeadSHA        string
	BaseRef        string
}
