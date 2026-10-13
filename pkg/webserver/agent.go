package webserver

import (
	"time"

	"github.com/xyzjace/terraplane/pkg/command"
)

type agentJobClaimPayload struct {
	AgentID string `json:"agent_id"`
}

type agentJobClaimResponse struct {
	Command command.Command `json:"command"`
}

type agentHeartbeatPayload struct {
	AgentID string `json:"agent_id"`
}

type agentJobAckPayload struct {
	AgentID string `json:"agent_id"`
}

type agentJobResultPayload struct {
	AgentID string `json:"agent_id"`
	Result  string `json:"result"`
	Output  string `json:"output"`
	Error   string `json:"error"`
}

type agentPeriodicResultPayload struct {
	AgentID string `json:"agent_id"`
	Output  string `json:"output"`
}

type agentCloneTokenPayload struct {
	AgentID string `json:"agent_id"`
}

type agentCloneTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}
