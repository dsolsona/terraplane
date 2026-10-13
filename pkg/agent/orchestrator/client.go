package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/xyzjace/terraplane/config"
	"github.com/xyzjace/terraplane/pkg/command"
)

//go:generate mockgen -destination=mock_orchestrator/mock_client.go -package=mock_orchestrator github.com/xyzjace/terraplane/pkg/agent/orchestrator Client
type Client interface {
	ClaimJob(ctx context.Context, agentID string) (*command.Command, error)
	Heartbeat(ctx context.Context, jobID string, agentID string) error
	Ack(ctx context.Context, jobID string, agentID string) error
	SubmitResult(ctx context.Context, jobID string, agentID string, success bool, output string, errMsg string) error
	SubmitPeriodicResult(ctx context.Context, jobID string, agentID string, output string) error
	// CloneToken asks the orchestrator for a short-lived token that can read
	// the job's repository. Only available when the orchestrator is a GitHub App.
	CloneToken(ctx context.Context, jobID string, agentID string) (string, error)
}

type claimResponse struct {
	Command command.Command `json:"command"`
}

type cloneTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

type client struct {
	httpClient      *http.Client
	baseURL         string
	sharedAuthToken string
}

func (c *client) post(ctx context.Context, path string, payload any) (*http.Response, error) {
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+path, bytes.NewBuffer(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.sharedAuthToken)
	req.Header.Set("Content-Type", "application/json")
	return c.httpClient.Do(req)
}

func (c *client) ClaimJob(ctx context.Context, agentID string) (*command.Command, error) {
	resp, err := c.post(ctx, "/agent/jobs/claim", map[string]string{"agent_id": agentID})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
		var response claimResponse
		if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
			return nil, fmt.Errorf("failed to decode claim response: %w", err)
		}
		return &response.Command, nil
	default:
		return nil, fmt.Errorf("claim job failed: %s", resp.Status)
	}
}

func (c *client) Heartbeat(ctx context.Context, jobID string, agentID string) error {
	resp, err := c.post(ctx, fmt.Sprintf("/agent/jobs/%s/heartbeat", jobID), map[string]string{"agent_id": agentID})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("heartbeat failed: %s", resp.Status)
	}
	return nil
}

func (c *client) Ack(ctx context.Context, jobID string, agentID string) error {
	resp, err := c.post(ctx, fmt.Sprintf("/agent/jobs/%s/ack", jobID), map[string]string{"agent_id": agentID})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("ack job failed: %s", resp.Status)
	}
	return nil
}

func (c *client) SubmitResult(ctx context.Context, jobID string, agentID string, success bool, output string, errMsg string) error {
	result := "failed"
	if success {
		result = "success"
	}
	resp, err := c.post(ctx, fmt.Sprintf("/agent/jobs/%s/result", jobID), map[string]string{
		"agent_id": agentID,
		"result":   result,
		"output":   output,
		"error":    errMsg,
	})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("submit result failed: %s", resp.Status)
	}
	return nil
}

func (c *client) SubmitPeriodicResult(ctx context.Context, jobID string, agentID string, output string) error {
	resp, err := c.post(ctx, fmt.Sprintf("/agent/jobs/%s/periodic_result", jobID), map[string]string{
		"agent_id": agentID,
		"output":   output,
	})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("submit periodic result failed: %s", resp.Status)
	}
	return nil
}

func (c *client) CloneToken(ctx context.Context, jobID string, agentID string) (string, error) {
	resp, err := c.post(ctx, fmt.Sprintf("/agent/jobs/%s/clone_token", jobID), map[string]string{"agent_id": agentID})
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var response cloneTokenResponse
		if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
			return "", fmt.Errorf("failed to decode clone token response: %w", err)
		}
		if response.Token == "" {
			return "", fmt.Errorf("orchestrator returned an empty clone token for job %s", jobID)
		}
		return response.Token, nil
	case http.StatusNotImplemented:
		return "", fmt.Errorf("orchestrator cannot issue clone tokens (%s); it must authenticate to GitHub as a GitHub App, or set AGENT_SCM_AUTH=ssh on this agent", resp.Status)
	default:
		return "", fmt.Errorf("clone token request failed: %s", resp.Status)
	}
}

func NewClient(config *config.Config) Client {
	return &client{
		httpClient:      &http.Client{},
		baseURL:         config.AgentOrchestratorURL,
		sharedAuthToken: config.SharedAuthToken,
	}
}
