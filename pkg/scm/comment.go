package scm

// IssueComment is the part of a pull request comment the orchestrator trusts
// over the webhook body: what was said, by whom, and on which issue.
type IssueComment struct {
	ID        int
	Body      string
	UserLogin string
	IssueURL  string
}
