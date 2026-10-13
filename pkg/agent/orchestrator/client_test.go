package orchestrator_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xyzjace/terraplane/config"
	orchestrator "github.com/xyzjace/terraplane/pkg/agent/orchestrator"
	"github.com/xyzjace/terraplane/pkg/command"
)

func newTestClient(t *testing.T, srv *httptest.Server) orchestrator.Client {
	t.Helper()
	return orchestrator.NewClient(&config.Config{
		AgentOrchestratorURL: srv.URL,
		SharedAuthToken:      "secret",
	})
}

// --- ClaimJob ---

func TestClaimJobNoWork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "/agent/jobs/claim", r.URL.Path)
		require.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cmd, err := newTestClient(t, srv).ClaimJob(context.Background(), "agent-dev")
	require.NoError(t, err)
	require.Nil(t, cmd)
}

func TestClaimJobReturnsCommand(t *testing.T) {
	want := command.Command{Kind: command.KindPlan, Plan: command.PlanCommand{}}
	want.Plan.JobID = "job-1"
	want.Plan.Repo = "acme/infra"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"command": want})
	}))
	defer srv.Close()

	cmd, err := newTestClient(t, srv).ClaimJob(context.Background(), "agent-dev")
	require.NoError(t, err)
	require.NotNil(t, cmd)
	require.Equal(t, command.KindPlan, cmd.Kind)
	require.Equal(t, "job-1", cmd.Plan.JobID)
	require.Equal(t, "acme/infra", cmd.Plan.Repo)
}

func TestClaimJobSendsAgentID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "agent-dev", body["agent_id"])
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).ClaimJob(context.Background(), "agent-dev")
	require.NoError(t, err)
}

func TestClaimJobServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cmd, err := newTestClient(t, srv).ClaimJob(context.Background(), "agent-dev")
	require.Error(t, err)
	require.Contains(t, err.Error(), "claim job failed")
	require.Nil(t, cmd)
}

func TestClaimJobInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{not json"))
	}))
	defer srv.Close()

	cmd, err := newTestClient(t, srv).ClaimJob(context.Background(), "agent-dev")
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to decode claim response")
	require.Nil(t, cmd)
}

// --- Heartbeat ---

func TestHeartbeatSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "/agent/jobs/job-1/heartbeat", r.URL.Path)
		require.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	err := newTestClient(t, srv).Heartbeat(context.Background(), "job-1", "agent-dev")
	require.NoError(t, err)
}

func TestHeartbeatSendsAgentID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "agent-dev", body["agent_id"])
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	require.NoError(t, newTestClient(t, srv).Heartbeat(context.Background(), "job-1", "agent-dev"))
}

func TestHeartbeatServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := newTestClient(t, srv).Heartbeat(context.Background(), "job-1", "agent-dev")
	require.Error(t, err)
	require.Contains(t, err.Error(), "heartbeat failed")
}

// --- Ack ---

func TestAckSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "/agent/jobs/job-1/ack", r.URL.Path)
		require.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "agent-dev", body["agent_id"])
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	require.NoError(t, newTestClient(t, srv).Ack(context.Background(), "job-1", "agent-dev"))
}

func TestAckServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := newTestClient(t, srv).Ack(context.Background(), "job-1", "agent-dev")
	require.Error(t, err)
	require.Contains(t, err.Error(), "ack job failed")
}

// --- SubmitResult ---

func TestSubmitResultSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "/agent/jobs/job-1/result", r.URL.Path)
		require.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "agent-dev", body["agent_id"])
		require.Equal(t, "success", body["result"])
		require.Equal(t, "plan output", body["output"])
		require.Empty(t, body["error"])
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	require.NoError(t, newTestClient(t, srv).SubmitResult(context.Background(), "job-1", "agent-dev", true, "plan output", ""))
}

func TestSubmitResultFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "failed", body["result"])
		require.Equal(t, "something broke", body["error"])
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	require.NoError(t, newTestClient(t, srv).SubmitResult(context.Background(), "job-1", "agent-dev", false, "", "something broke"))
}

func TestSubmitResultServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := newTestClient(t, srv).SubmitResult(context.Background(), "job-1", "agent-dev", true, "", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "submit result failed")
}

// --- CloneToken ---

func TestCloneTokenSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "/agent/jobs/job-1/clone_token", r.URL.Path)
		require.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "agent-dev", body["agent_id"])
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "ghs_abc", "expires_at": "2026-01-01T00:00:00Z"})
	}))
	defer srv.Close()

	token, err := newTestClient(t, srv).CloneToken(context.Background(), "job-1", "agent-dev")
	require.NoError(t, err)
	require.Equal(t, "ghs_abc", token)
}

func TestCloneTokenEmptyToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"token": ""})
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).CloneToken(context.Background(), "job-1", "agent-dev")
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty clone token")
}

func TestCloneTokenInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).CloneToken(context.Background(), "job-1", "agent-dev")
	require.Error(t, err)
	require.Contains(t, err.Error(), "decode clone token response")
}

func TestCloneTokenNotImplemented(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).CloneToken(context.Background(), "job-1", "agent-dev")
	require.Error(t, err)
	require.Contains(t, err.Error(), "GitHub App")
	require.Contains(t, err.Error(), "AGENT_SCM_AUTH=ssh")
}

func TestCloneTokenServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).CloneToken(context.Background(), "job-1", "agent-dev")
	require.Error(t, err)
	require.Contains(t, err.Error(), "clone token request failed: 403")
}

func TestCloneTokenConnectionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()

	_, err := newTestClient(t, srv).CloneToken(context.Background(), "job-1", "agent-dev")
	require.Error(t, err)
}
