package terraform

import (
	"fmt"
	"strings"
)

// Tool is the binary a stack is run with. Both speak the same CLI for the
// init, plan, and apply invocations terraplane makes.
type Tool string

const (
	ToolTerraform Tool = "terraform"
	ToolTofu      Tool = "tofu"
)

// ParseTool accepts "terraform" or "tofu" (case-insensitive). Empty means
// "not specified" and is returned as "" so callers can fall back.
func ParseTool(name string) (Tool, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "":
		return "", nil
	case string(ToolTerraform):
		return ToolTerraform, nil
	case string(ToolTofu), "opentofu":
		return ToolTofu, nil
	default:
		return "", fmt.Errorf("unsupported tool %q (expected terraform or tofu)", name)
	}
}
