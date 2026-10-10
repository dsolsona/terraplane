package feedback

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/xyzjace/terraplane/pkg/storage/models"
)

var (
	planSummaryRE = regexp.MustCompile(
		`(?m)^Plan:\s+(\d+)\s+to\s+add,\s+(\d+)\s+to\s+change,\s+(\d+)\s+to\s+destroy\.?$`,
	)
	applySummaryRE = regexp.MustCompile(
		`(?m)^Apply complete! Resources:\s+(\d+)\s+added,\s+(\d+)\s+changed,\s+(\d+)\s+destroyed\.?$`,
	)
	noChangesRE = regexp.MustCompile(`(?i)No changes\.\s+Your infrastructure matches the configuration\.`)
)

type resourceDelta struct {
	add, change, destroy int
}

// JobResultComment formats a plan or apply job result as a GitHub PR comment body.
func JobResultComment(job *models.Job, status models.JobStatus, output, errMsg string) string {
	switch job.Action {
	case models.JobActionApply:
		return ApplyResultComment(job, status, output, errMsg)
	default:
		return PlanResultComment(job, status, output, errMsg)
	}
}

// PlanResultComment formats a plan job result as a GitHub PR comment body.
func PlanResultComment(job *models.Job, status models.JobStatus, output, errMsg string) string {
	var b strings.Builder
	var success bool
	if status == models.JobStatusSucceeded {
		success = true
	} else {
		success = false
	}

	writeHeader(&b, "plan", job.StackName, status)
	writeMeta(&b, job)

	if delta, fromSummary := parsePlanDelta(output); fromSummary {
		writeDelta(&b, delta)
	} else if success && noChangesRE.MatchString(output) {
		b.WriteString("\n> [!NOTE]\n")
		b.WriteString("> No infrastructure changes.\n")
	}

	writeError(&b, errMsg)
	writeOutput(&b, status, output)

	if success {
		fmt.Fprintf(&b, "\nApply when ready:\n\n```\nterraplane apply -s %s\n```\n", job.StackName)
	}

	return b.String()
}

// ApplyResultComment formats an apply job result as a GitHub PR comment body.
func ApplyResultComment(job *models.Job, status models.JobStatus, output, errMsg string) string {
	var b strings.Builder

	writeHeader(&b, "apply", job.StackName, status)
	writeMeta(&b, job)

	if delta, ok := parseApplyDelta(output); ok {
		writeDelta(&b, delta)
	}

	writeError(&b, errMsg)
	writeOutput(&b, status, output)
	return b.String()
}

// UnlockResultComment formats an unlock result as a GitHub PR comment body.
func UnlockResultComment(stackName string, status models.JobStatus, errMsg string) string {
	var b strings.Builder
	writeHeader(&b, "unlock", stackName, status)
	writeError(&b, errMsg)
	return b.String()
}

// DeniedComment formats an authorization denial as a GitHub PR comment body.
// One comment covers every denied stack of a command.
func DeniedComment(action string, reasons []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### %s · ⛔ denied\n", action)
	b.WriteString("\n> [!CAUTION]\n")
	for _, reason := range reasons {
		fmt.Fprintf(&b, "> - %s\n", strings.TrimSpace(reason))
	}
	return b.String()
}

func writeHeader(b *strings.Builder, action, stack string, status models.JobStatus) {
	statusLabel := statusLabel(status)
	switch {
	case stack != "":
		fmt.Fprintf(b, "### `%s` · %s · %s\n", stack, action, statusLabel)
	default:
		fmt.Fprintf(b, "### %s · %s\n", action, statusLabel)
	}
}

func writeMeta(b *strings.Builder, job *models.Job) {
	dir := strings.TrimSpace(job.Dir)
	sha := shortSHA(job.CommitSHA)
	switch {
	case dir != "" && sha != "":
		fmt.Fprintf(b, "\n`%s@%s`\n", dir, sha)
	case dir != "":
		fmt.Fprintf(b, "\n`%s`\n", dir)
	case sha != "":
		fmt.Fprintf(b, "\n`%s`\n", sha)
	}
}

// writeDelta renders counts in a ```diff fence so GitHub paints adds green and
// destroys red. A caution callout appears when anything would be destroyed.
func writeDelta(b *strings.Builder, d resourceDelta) {
	b.WriteString("\n```diff\n")
	fmt.Fprintf(b, "+ %d add\n", d.add)
	fmt.Fprintf(b, "  %d change\n", d.change)
	fmt.Fprintf(b, "- %d destroy\n", d.destroy)
	b.WriteString("```\n")

	if d.destroy > 0 {
		b.WriteString("\n> [!CAUTION]\n")
		fmt.Fprintf(b, "> This run destroys **%d** resource", d.destroy)
		if d.destroy != 1 {
			b.WriteString("s")
		}
		b.WriteString(".\n")
	}
}

func writeError(b *strings.Builder, errMsg string) {
	errMsg = strings.TrimSpace(errMsg)
	if errMsg == "" {
		return
	}
	b.WriteString("\n> [!CAUTION]\n")
	b.WriteString("> Something went wrong:\n")
	b.WriteString("\n")
	writeFencedBlock(b, errMsg)
}

func writeOutput(b *strings.Builder, status models.JobStatus, output string) {
	output = strings.TrimSpace(output)
	if output == "" {
		return
	}
	if status == models.JobStatusRunning {
		b.WriteString("\n<details open>\n")
		b.WriteString("<summary>Output (live)</summary>\n\n")
	} else {
		b.WriteString("\n<details>\n")
		b.WriteString("<summary>Output</summary>\n\n")
	}
	writeFencedBlock(b, output)
	b.WriteString("</details>\n")
}

func statusLabel(status models.JobStatus) string {
	switch status {
	case models.JobStatusSucceeded:
		return "✅ passed"
	case models.JobStatusRunning:
		return "⌛ running"
	default:
		return "❌ failed"
	}
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) >= 7 {
		return sha[:7]
	}
	return sha
}

func parsePlanDelta(output string) (resourceDelta, bool) {
	m := planSummaryRE.FindStringSubmatch(output)
	if m == nil {
		return resourceDelta{}, false
	}
	return resourceDelta{
		add:     atoi(m[1]),
		change:  atoi(m[2]),
		destroy: atoi(m[3]),
	}, true
}

func parseApplyDelta(output string) (resourceDelta, bool) {
	m := applySummaryRE.FindStringSubmatch(output)
	if m == nil {
		return resourceDelta{}, false
	}
	return resourceDelta{
		add:     atoi(m[1]),
		change:  atoi(m[2]),
		destroy: atoi(m[3]),
	}, true
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// writeFencedBlock writes a fenced code block using a fence long enough to
// avoid colliding with fences inside the body.
func writeFencedBlock(b *strings.Builder, body string) {
	fence := codeFence(body)
	fmt.Fprintf(b, "%s\n%s\n%s\n", fence, body, fence)
}

func codeFence(body string) string {
	max := 3
	inFence := false
	count := 0
	for _, r := range body {
		if r == '`' {
			if !inFence {
				inFence = true
				count = 1
			} else {
				count++
			}
			if count > max {
				max = count
			}
		} else if inFence {
			inFence = false
			count = 0
		}
	}
	return strings.Repeat("`", max+1)
}
