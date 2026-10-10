package terraform

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// defaultTofuReleasesURL is where OpenTofu publishes release assets.
const defaultTofuReleasesURL = "https://github.com/opentofu/opentofu/releases/download"

// installTofu downloads the OpenTofu release zip for this platform into dir,
// verifies it against the published SHA256SUMS, and extracts the binary.
// It returns the binary path.
func installTofu(ctx context.Context, client *http.Client, releasesURL, version, dir string) (string, error) {
	archive := fmt.Sprintf("tofu_%s_%s_%s.zip", version, runtime.GOOS, runtime.GOARCH)
	base := fmt.Sprintf("%s/v%s", strings.TrimRight(releasesURL, "/"), version)

	sums, err := fetch(ctx, client, base+"/tofu_"+version+"_SHA256SUMS")
	if err != nil {
		return "", fmt.Errorf("failed to download OpenTofu %s checksums: %w", version, err)
	}
	want, err := checksumFor(sums, archive)
	if err != nil {
		return "", fmt.Errorf("OpenTofu %s: %w", version, err)
	}

	body, err := fetch(ctx, client, base+"/"+archive)
	if err != nil {
		return "", fmt.Errorf("failed to download OpenTofu %s: %w", version, err)
	}
	got := sha256.Sum256(body)
	if hex.EncodeToString(got[:]) != want {
		return "", fmt.Errorf("OpenTofu %s archive %s does not match its published checksum", version, archive)
	}

	binary, err := zipEntry(body, "tofu")
	if err != nil {
		return "", fmt.Errorf("OpenTofu %s archive %s: %w", version, archive, err)
	}

	path := filepath.Join(dir, "tofu")
	tmp, err := os.CreateTemp(dir, "tofu-*")
	if err != nil {
		return "", fmt.Errorf("failed to create OpenTofu binary in %q: %w", dir, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(binary); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("failed to write OpenTofu binary: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("failed to make OpenTofu binary executable: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("failed to write OpenTofu binary: %w", err)
	}
	// Rename so a concurrent reader never sees a partial binary.
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("failed to install OpenTofu binary at %q: %w", path, err)
	}
	return path, nil
}

func fetch(ctx context.Context, client *http.Client, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build request for %s: %w", u, err)
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch %s: %w", u, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s returned unexpected status %s", u, res.Status)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", u, err)
	}
	return body, nil
}

// checksumFor finds the sha256 for name in a SHA256SUMS file ("<hash>  <name>" per line).
func checksumFor(sums []byte, name string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(sums)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("no checksum published for %s", name)
}

func zipEntry(archive []byte, name string) ([]byte, error) {
	reader, err := zip.NewReader(strings.NewReader(string(archive)), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("not a zip archive: %w", err)
	}
	for _, file := range reader.File {
		if file.Name != name {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("failed to open %s: %w", name, err)
		}
		defer func() { _ = rc.Close() }()
		data, err := io.ReadAll(rc)
		if err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", name, err)
		}
		return data, nil
	}
	return nil, fmt.Errorf("does not contain %s", name)
}
