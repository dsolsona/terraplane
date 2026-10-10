package terraform

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	hcversion "github.com/hashicorp/go-version"
	install "github.com/hashicorp/hc-install"
	"github.com/hashicorp/hc-install/fs"
	"github.com/hashicorp/hc-install/product"
	"github.com/hashicorp/hc-install/releases"
	"github.com/hashicorp/hc-install/src"
	"github.com/xyzjace/terraplane/pkg/log"
	"golang.org/x/sync/singleflight"
)

var toolInstallGroup singleflight.Group

type VersionManager interface {
	// Ensure returns the path of the tool binary at version, downloading it if needed.
	Ensure(ctx context.Context, tool Tool, version string) (string, error)
}

//go:generate mockgen -source=version_manager.go -destination=mock_terraform/mock_version_manager.go -package=mock_terraform

type versionManager struct {
	logger          log.Logger
	binDir          string
	tofuReleasesURL string
	httpClient      *http.Client
}

func NewVersionManager(logger log.Logger, binDir string) VersionManager {
	if binDir == "" {
		binDir = filepath.Join(os.TempDir(), "terraplane-terraform")
	}
	return &versionManager{
		logger:          logger,
		binDir:          binDir,
		tofuReleasesURL: defaultTofuReleasesURL,
		httpClient:      &http.Client{},
	}
}

// Terraform binaries keep their historical layout (<binDir>/<version>/terraform)
// so existing agent work directories stay valid; OpenTofu lives under
// <binDir>/tofu/<version>/tofu.
func (m *versionManager) Ensure(ctx context.Context, tool Tool, version string) (string, error) {
	v, err := parseToolVersion(version)
	if err != nil {
		return "", err
	}

	var versionDir, execPath string
	switch tool {
	case ToolTerraform:
		versionDir = filepath.Join(m.binDir, v.String())
		execPath = filepath.Join(versionDir, product.Terraform.BinaryName())
	case ToolTofu:
		versionDir = filepath.Join(m.binDir, "tofu", v.String())
		execPath = filepath.Join(versionDir, "tofu")
	default:
		return "", fmt.Errorf("unsupported tool %q", tool)
	}

	if path, err := existingBinary(execPath); err == nil {
		m.logger.Info("Using tool binary", "tool", tool, "version", v.String(), "path", path)
		return path, nil
	}

	result, err, _ := toolInstallGroup.Do(versionDir, func() (any, error) {
		if path, err := existingBinary(execPath); err == nil {
			return path, nil
		}

		if err := os.MkdirAll(versionDir, 0o755); err != nil {
			return "", fmt.Errorf("failed to create tool version directory %q: %w", versionDir, err)
		}

		if tool == ToolTofu {
			return installTofu(ctx, m.httpClient, m.tofuReleasesURL, v.String(), versionDir)
		}

		installer := install.NewInstaller()
		path, err := installer.Ensure(ctx, []src.Source{
			&fs.ExactVersion{
				Product:    product.Terraform,
				Version:    v,
				ExtraPaths: []string{versionDir},
			},
			&releases.ExactVersion{
				Product:    product.Terraform,
				Version:    v,
				InstallDir: versionDir,
			},
		})
		if err != nil {
			return "", err
		}
		return path, nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to ensure %s %s: %w", tool, v, err)
	}

	execPath = result.(string)
	m.logger.Info("Using tool binary", "tool", tool, "version", v.String(), "path", execPath)
	return execPath, nil
}

func existingBinary(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("tool binary path %q is a directory", path)
	}
	return path, nil
}

func parseToolVersion(version string) (*hcversion.Version, error) {
	version = strings.TrimSpace(version)
	version = strings.TrimPrefix(version, "v")
	if version == "" {
		return nil, fmt.Errorf("tool version is required")
	}

	v, err := hcversion.NewVersion(version)
	if err != nil {
		return nil, fmt.Errorf("invalid tool version %q: %w", version, err)
	}
	return v, nil
}
