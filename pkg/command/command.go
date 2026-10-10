package command

type Kind string

const (
	KindUnknown Kind = "unknown"
	KindPlan    Kind = "plan"
	KindApply   Kind = "apply"
	KindUnlock  Kind = "unlock"
)

type Command struct {
	Kind   Kind
	Plan   PlanCommand
	Apply  ApplyCommand
	Unlock UnlockCommand
}

func (c *Command) JobID() string {
	switch c.Kind {
	case KindPlan:
		return c.Plan.JobID
	case KindApply:
		return c.Apply.JobID
	case KindUnlock:
		return c.Unlock.JobID
	default:
		return ""
	}
}

type base struct {
	Repo, TriggerUser, RawComment, CommitSHA, Agent, JobID, Dir, Tool, ToolVersion string
	PRNumber                                                                       int
}

type PlanCommand struct {
	base
	Stacks       []string
	Environments []string
	PlanFlags    string
}

type ApplyCommand struct {
	base
	Stacks       []string
	Environments []string
}

type UnlockCommand struct {
	base
	Stacks       []string
	Environments []string
}
