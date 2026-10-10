package terraform

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/xyzjace/terraplane/internal/process"
)

func TestCommandOutput(t *testing.T) {
	require.Equal(t, "out", commandOutput(process.Result{Stdout: "out"}))
	require.Equal(t, "err", commandOutput(process.Result{Stderr: "err"}))
	require.Equal(t, "out\nerr", commandOutput(process.Result{Stdout: "out", Stderr: "err"}))
}

func TestRemoveStalePlanFiles(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "plan.tfplan")
	stale := filepath.Join(dir, "plan-old.tfplan")
	require.NoError(t, os.WriteFile(keep, []byte("keep"), 0o644))
	require.NoError(t, os.WriteFile(stale, []byte("stale"), 0o644))

	require.NoError(t, removeStalePlanFiles(dir, "plan.tfplan"))
	require.FileExists(t, keep)
	require.NoFileExists(t, stale)
}

func TestParseTerraformVersion(t *testing.T) {
	v, err := parseToolVersion("v1.5.7")
	require.NoError(t, err)
	require.Equal(t, "1.5.7", v.String())

	_, err = parseToolVersion("")
	require.Error(t, err)

	_, err = parseToolVersion("not-a-version")
	require.Error(t, err)
}

func TestExistingTerraformBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "terraform")
	require.NoError(t, os.WriteFile(bin, []byte("x"), 0o755))

	got, err := existingBinary(bin)
	require.NoError(t, err)
	require.Equal(t, bin, got)

	_, err = existingBinary(dir)
	require.Error(t, err)

	_, err = existingBinary(filepath.Join(dir, "missing"))
	require.Error(t, err)
}
