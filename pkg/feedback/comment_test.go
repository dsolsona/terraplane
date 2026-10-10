package feedback_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/xyzjace/terraplane/pkg/feedback"
	"github.com/xyzjace/terraplane/pkg/storage/models"
)

func TestPlanResultComment(t *testing.T) {
	job := &models.Job{
		StackName: "stg-foundation",
		Dir:       "terraform/environments/staging/foundation",
		CommitSHA: "abcdef0123456789",
	}

	t.Run("success with plan summary and output", func(t *testing.T) {
		output := "Terraform will perform the following actions:\n\nPlan: 1 to add, 2 to change, 3 to destroy.\n"
		got := feedback.PlanResultComment(job, models.JobStatusSucceeded, output, "")

		require.Contains(t, got, "### `stg-foundation` · plan · ✅ passed")
		require.Contains(t, got, "`terraform/environments/staging/foundation@abcdef0`")
		require.Contains(t, got, "```diff\n+ 1 add\n  2 change\n- 3 destroy\n```")
		require.Contains(t, got, "> [!CAUTION]")
		require.Contains(t, got, "destroys **3**")
		require.Contains(t, got, "<summary>Output</summary>")
		require.Contains(t, got, "terraplane apply -s stg-foundation")
		require.NotContains(t, got, "**terraplane**")
		require.NotContains(t, got, "❌")
	})

	t.Run("no changes", func(t *testing.T) {
		output := "No changes. Your infrastructure matches the configuration.\n"
		got := feedback.PlanResultComment(job, models.JobStatusSucceeded, output, "")
		require.Contains(t, got, "> [!NOTE]")
		require.Contains(t, got, "No infrastructure changes.")
		require.Contains(t, got, "terraplane apply -s stg-foundation")
		require.NotContains(t, got, "```diff")
	})

	t.Run("failure with error and no apply hint", func(t *testing.T) {
		got := feedback.PlanResultComment(job, models.JobStatusFailed, "no plan line", "boom")
		require.Contains(t, got, "### `stg-foundation` · plan · ❌ failed")
		require.Contains(t, got, "> [!CAUTION]")
		require.Contains(t, got, "boom")
		require.Contains(t, got, "<summary>Output</summary>")
		require.NotContains(t, got, "Apply when ready")
		require.NotContains(t, got, "```diff")
	})

	t.Run("empty output and error", func(t *testing.T) {
		got := feedback.PlanResultComment(&models.Job{StackName: "stg-foundation"}, models.JobStatusSucceeded, "   ", "  ")
		require.Equal(t, "### `stg-foundation` · plan · ✅ passed\n\nApply when ready:\n\n```\nterraplane apply -s stg-foundation\n```\n", got)
	})

	t.Run("extends fence for backticks in body", func(t *testing.T) {
		got := feedback.PlanResultComment(job, models.JobStatusFailed, "", "error with ```` inside")
		require.Contains(t, got, "`````\nerror with ```` inside\n`````\n")
	})

	t.Run("zero destroys skip caution", func(t *testing.T) {
		output := "Plan: 0 to add, 1 to change, 0 to destroy.\n"
		got := feedback.PlanResultComment(job, models.JobStatusSucceeded, output, "")
		require.Contains(t, got, "```diff\n+ 0 add\n  1 change\n- 0 destroy\n```")
		require.NotContains(t, got, "destroys")
	})

	t.Run("singular destroy wording", func(t *testing.T) {
		output := "Plan: 0 to add, 0 to change, 1 to destroy.\n"
		got := feedback.PlanResultComment(job, models.JobStatusSucceeded, output, "")
		require.Contains(t, got, "destroys **1** resource.")
		require.NotContains(t, got, "resources.")
	})

	t.Run("short commit sha passthrough", func(t *testing.T) {
		short := &models.Job{StackName: "s", CommitSHA: "abc"}
		got := feedback.PlanResultComment(short, models.JobStatusSucceeded, "", "")
		require.Contains(t, got, "`abc`")
	})

	t.Run("dir only meta", func(t *testing.T) {
		got := feedback.PlanResultComment(&models.Job{StackName: "s", Dir: "terraform/x"}, models.JobStatusSucceeded, "", "")
		require.Contains(t, got, "`terraform/x`")
		require.NotContains(t, got, "@")
	})
}

func TestJobResultComment(t *testing.T) {
	t.Run("apply action", func(t *testing.T) {
		job := &models.Job{
			StackName: "stg-foundation",
			Action:    models.JobActionApply,
			Dir:       "terraform/stg",
			CommitSHA: "deadbeef",
		}
		output := "Apply complete! Resources: 1 added, 0 changed, 0 destroyed.\n"
		got := feedback.JobResultComment(job, models.JobStatusSucceeded, output, "")
		require.Contains(t, got, "### `stg-foundation` · apply · ✅ passed")
	})

	t.Run("plan action by default", func(t *testing.T) {
		job := &models.Job{StackName: "stg-foundation"}
		got := feedback.JobResultComment(job, models.JobStatusSucceeded, "", "")
		require.Contains(t, got, "### `stg-foundation` · plan · ✅ passed")
	})
}

func TestApplyResultComment(t *testing.T) {
	job := &models.Job{
		StackName: "stg-foundation",
		Dir:       "terraform/stg",
		CommitSHA: "deadbeef",
	}

	t.Run("success with apply summary", func(t *testing.T) {
		output := "Apply complete! Resources: 1 added, 2 changed, 0 destroyed.\n"
		got := feedback.ApplyResultComment(job, models.JobStatusSucceeded, output, "")
		require.Contains(t, got, "### `stg-foundation` · apply · ✅ passed")
		require.Contains(t, got, "`terraform/stg@deadbee`")
		require.Contains(t, got, "```diff\n+ 1 add\n  2 change\n- 0 destroy\n```")
		require.Contains(t, got, "<summary>Output</summary>")
		require.NotContains(t, got, "destroys")
		require.NotContains(t, got, "**terraplane**")
	})

	t.Run("failure with error only", func(t *testing.T) {
		got := feedback.ApplyResultComment(job, models.JobStatusFailed, "", "apply failed")
		require.Contains(t, got, "### `stg-foundation` · apply · ❌ failed")
		require.Contains(t, got, "apply failed")
		require.NotContains(t, got, "<details>")
	})
}

func TestUnlockResultComment(t *testing.T) {
	t.Run("with stack", func(t *testing.T) {
		got := feedback.UnlockResultComment("stg-foundation", models.JobStatusSucceeded, "")
		require.Equal(t, "### `stg-foundation` · unlock · ✅ passed\n", got)
	})

	t.Run("without stack", func(t *testing.T) {
		got := feedback.UnlockResultComment("", models.JobStatusFailed, "unlock failed")
		require.True(t, strings.HasPrefix(got, "### unlock · ❌ failed\n"))
		require.Contains(t, got, "unlock failed")
	})
}

func TestRunningStatusLabel(t *testing.T) {
	job := &models.Job{StackName: "stg-foundation"}
	got := feedback.PlanResultComment(job, models.JobStatusRunning, "still going", "")
	require.Contains(t, got, "### `stg-foundation` · plan · ⌛ running")
	require.Contains(t, got, "<details open>")
	require.Contains(t, got, "<summary>Output (live)</summary>")
	require.Contains(t, got, "still going")
	require.NotContains(t, got, "Apply when ready")
}

func TestDeniedComment(t *testing.T) {
	got := feedback.DeniedComment("apply", []string{
		"bob is not allowed to apply stack reporting in acme/infra",
		"  bob is not allowed to apply stack billing in acme/infra  ",
	})
	require.Equal(t, "### apply · ⛔ denied\n\n> [!CAUTION]\n> - bob is not allowed to apply stack reporting in acme/infra\n> - bob is not allowed to apply stack billing in acme/infra\n", got)
}
