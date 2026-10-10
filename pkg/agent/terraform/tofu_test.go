package terraform

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xyzjace/terraplane/pkg/log"
)

const tofuTestVersion = "1.11.5"

func tofuArchiveName() string {
	return fmt.Sprintf("tofu_%s_%s_%s.zip", tofuTestVersion, runtime.GOOS, runtime.GOARCH)
}

func zipWith(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	if name != "" {
		f, err := w.Create(name)
		require.NoError(t, err)
		_, err = f.Write(content)
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// tofuReleases serves a fake OpenTofu release: the archive and a SHA256SUMS
// file. sums may be overridden to test verification failures.
type tofuReleases struct {
	archive []byte
	sums    string
	hits    map[string]int
}

func newTofuReleases(t *testing.T, archive []byte) *tofuReleases {
	t.Helper()
	return &tofuReleases{
		archive: archive,
		sums:    fmt.Sprintf("%s  %s\n%s  tofu_%s_other_arch.zip\n", sha256Hex(archive), tofuArchiveName(), sha256Hex([]byte("x")), tofuTestVersion),
		hits:    map[string]int{},
	}
}

func (r *tofuReleases) server(t *testing.T) *httptest.Server {
	t.Helper()
	prefix := "/v" + tofuTestVersion + "/"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.hits[req.URL.Path]++
		switch req.URL.Path {
		case prefix + "tofu_" + tofuTestVersion + "_SHA256SUMS":
			_, _ = w.Write([]byte(r.sums))
		case prefix + tofuArchiveName():
			_, _ = w.Write(r.archive)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestInstallTofuSuccess(t *testing.T) {
	binary := []byte("#!/bin/sh\necho tofu\n")
	releases := newTofuReleases(t, zipWith(t, "tofu", binary))
	srv := releases.server(t)
	dir := t.TempDir()

	path, err := installTofu(context.Background(), srv.Client(), srv.URL, tofuTestVersion, dir)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "tofu"), path)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, binary, got)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())

	// No temp files left behind.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestInstallTofuChecksumMismatch(t *testing.T) {
	releases := newTofuReleases(t, zipWith(t, "tofu", []byte("real")))
	releases.sums = fmt.Sprintf("%s  %s\n", sha256Hex([]byte("tampered")), tofuArchiveName())
	srv := releases.server(t)

	_, err := installTofu(context.Background(), srv.Client(), srv.URL, tofuTestVersion, t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not match its published checksum")
}

func TestInstallTofuMissingChecksumEntry(t *testing.T) {
	releases := newTofuReleases(t, zipWith(t, "tofu", []byte("real")))
	releases.sums = "abc  tofu_1.11.5_plan9_mips.zip\n"
	srv := releases.server(t)

	_, err := installTofu(context.Background(), srv.Client(), srv.URL, tofuTestVersion, t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "no checksum published for "+tofuArchiveName())
}

func TestInstallTofuChecksumsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	_, err := installTofu(context.Background(), srv.Client(), srv.URL, tofuTestVersion, t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to download OpenTofu 1.11.5 checksums")
	require.Contains(t, err.Error(), "404")
}

func TestInstallTofuArchiveNotFound(t *testing.T) {
	releases := newTofuReleases(t, zipWith(t, "tofu", []byte("real")))
	srv := releases.server(t)
	releases.archive = nil
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v"+tofuTestVersion+"/tofu_"+tofuTestVersion+"_SHA256SUMS" {
			_, _ = w.Write([]byte(releases.sums))
			return
		}
		http.NotFound(w, req)
	})

	_, err := installTofu(context.Background(), srv.Client(), srv.URL, tofuTestVersion, t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to download OpenTofu 1.11.5:")
}

func TestInstallTofuArchiveWithoutBinary(t *testing.T) {
	releases := newTofuReleases(t, zipWith(t, "README", []byte("nope")))
	srv := releases.server(t)

	_, err := installTofu(context.Background(), srv.Client(), srv.URL, tofuTestVersion, t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not contain tofu")
}

func TestInstallTofuNotAZip(t *testing.T) {
	releases := newTofuReleases(t, []byte("not a zip"))
	srv := releases.server(t)

	_, err := installTofu(context.Background(), srv.Client(), srv.URL, tofuTestVersion, t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a zip archive")
}

func TestInstallTofuUnwritableDir(t *testing.T) {
	releases := newTofuReleases(t, zipWith(t, "tofu", []byte("real")))
	srv := releases.server(t)

	_, err := installTofu(context.Background(), srv.Client(), srv.URL, tofuTestVersion, filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to create OpenTofu binary")
}

func TestInstallTofuTransportError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()

	_, err := installTofu(context.Background(), srv.Client(), srv.URL, tofuTestVersion, t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to fetch")
}

func TestChecksumForAcceptsBinaryMarker(t *testing.T) {
	got, err := checksumFor([]byte("ABCDEF *tofu.zip\n"), "tofu.zip")
	require.NoError(t, err)
	require.Equal(t, "abcdef", got)
}

func TestEnsureTofuDownloadsOnceAndReuses(t *testing.T) {
	binary := []byte("tofu-binary")
	releases := newTofuReleases(t, zipWith(t, "tofu", binary))
	srv := releases.server(t)
	binDir := t.TempDir()
	vm := &versionManager{
		logger:          log.Noop(),
		binDir:          binDir,
		tofuReleasesURL: srv.URL,
		httpClient:      srv.Client(),
	}

	path, err := vm.Ensure(context.Background(), ToolTofu, "v"+tofuTestVersion)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(binDir, "tofu", tofuTestVersion, "tofu"), path)

	again, err := vm.Ensure(context.Background(), ToolTofu, tofuTestVersion)
	require.NoError(t, err)
	require.Equal(t, path, again)
	require.Equal(t, 1, releases.hits["/v"+tofuTestVersion+"/"+tofuArchiveName()], "second call must reuse the installed binary")
}

func TestEnsureTofuDownloadFailure(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	vm := &versionManager{logger: log.Noop(), binDir: t.TempDir(), tofuReleasesURL: srv.URL, httpClient: srv.Client()}

	_, err := vm.Ensure(context.Background(), ToolTofu, tofuTestVersion)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to ensure tofu 1.11.5")
}

func TestEnsureRejectsBadInput(t *testing.T) {
	vm := NewVersionManager(log.Noop(), t.TempDir())

	_, err := vm.Ensure(context.Background(), ToolTofu, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "tool version is required")

	_, err = vm.Ensure(context.Background(), Tool("pulumi"), "1.0.0")
	require.Error(t, err)
	require.Contains(t, err.Error(), `unsupported tool "pulumi"`)
}

func TestEnsureTerraformReusesExistingBinary(t *testing.T) {
	// Terraform keeps the historical <binDir>/<version>/terraform layout.
	binDir := t.TempDir()
	versionDir := filepath.Join(binDir, "1.5.0")
	require.NoError(t, os.MkdirAll(versionDir, 0o755))
	bin := filepath.Join(versionDir, "terraform")
	require.NoError(t, os.WriteFile(bin, []byte("x"), 0o755))

	path, err := NewVersionManager(log.Noop(), binDir).Ensure(context.Background(), ToolTerraform, "1.5.0")
	require.NoError(t, err)
	require.Equal(t, bin, path)
}

func TestNewVersionManagerDefaultsBinDir(t *testing.T) {
	vm := NewVersionManager(log.Noop(), "").(*versionManager)
	require.Equal(t, filepath.Join(os.TempDir(), "terraplane-terraform"), vm.binDir)
	require.Equal(t, defaultTofuReleasesURL, vm.tofuReleasesURL)
}

func TestParseTool(t *testing.T) {
	for in, want := range map[string]Tool{"": "", "terraform": ToolTerraform, " Terraform ": ToolTerraform, "tofu": ToolTofu, "OpenTofu": ToolTofu} {
		got, err := ParseTool(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	_, err := ParseTool("pulumi")
	require.Error(t, err)
	require.Contains(t, err.Error(), `unsupported tool "pulumi"`)
}
