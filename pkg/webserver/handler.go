package webserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/xyzjace/terraplane/config"
	"github.com/xyzjace/terraplane/internal/auth"
	"github.com/xyzjace/terraplane/pkg/command"
	"github.com/xyzjace/terraplane/pkg/log"
	"github.com/xyzjace/terraplane/pkg/orchestrator/services"
	"github.com/xyzjace/terraplane/pkg/orchestratorconfig"
	"github.com/xyzjace/terraplane/pkg/scm"
)

type handler struct {
	logger             log.Logger
	scmProvider        scm.Provider
	scmPublisher       scm.Publisher
	mux                *http.ServeMux
	sharedAuthToken    string
	orchestratorConfig *orchestratorconfig.ConfigFile
	jobService         services.JobService
	unlockService      services.UnlockService
}

// agentIDContextKey carries the agent identity derived from a per-agent token.
type agentIDContextKey struct{}

func NewHandler(
	logger log.Logger,
	scmProvider scm.Provider,
	scmPublisher scm.Publisher,
	jobService services.JobService,
	unlockService services.UnlockService,
	config *config.Config,
	orchestratorConfig *orchestratorconfig.ConfigFile,
) http.Handler {
	h := &handler{
		logger:             logger,
		mux:                http.NewServeMux(),
		scmProvider:        scmProvider,
		scmPublisher:       scmPublisher,
		sharedAuthToken:    config.SharedAuthToken,
		orchestratorConfig: orchestratorConfig,
		jobService:         jobService,
		unlockService:      unlockService,
	}

	h.mux.HandleFunc("GET /health", h.healthCheck)
	h.mux.HandleFunc("POST /scm/webhook", h.scmWebhookHandler)
	h.mux.Handle("POST /agent/jobs/claim", h.requireBearer(http.HandlerFunc(h.agentJobClaimHandler)))
	h.mux.Handle("POST /agent/jobs/{id}/heartbeat", h.requireBearer(http.HandlerFunc(h.agentHeartbeatHandler)))
	h.mux.Handle("POST /agent/jobs/{id}/ack", h.requireBearer(http.HandlerFunc(h.agentJobAckHandler)))
	h.mux.Handle("POST /agent/jobs/{id}/result", h.requireBearer(http.HandlerFunc(h.agentJobResultHandler)))
	h.mux.Handle("POST /agent/jobs/{id}/periodic_result", h.requireBearer(http.HandlerFunc(h.agentPeriodicResultHandler)))

	return h
}

// requireBearer authenticates agent routes. With an orchestrator config the
// token identifies the agent and that identity is stored in the request
// context; otherwise the shared token is accepted from any agent.
func (h *handler) requireBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")

		if !h.orchestratorConfig.Enabled() {
			if !auth.BearerTokenMatches(header, h.sharedAuthToken) {
				h.rejectUnauthorized(w, r)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		token, ok := auth.ParseBearerToken(header)
		if !ok {
			h.rejectUnauthorized(w, r)
			return
		}
		agentID, ok := h.orchestratorConfig.AgentByToken(token)
		if !ok {
			h.rejectUnauthorized(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), agentIDContextKey{}, agentID)))
	})
}

func (h *handler) rejectUnauthorized(w http.ResponseWriter, r *http.Request) {
	h.logger.Warn("Rejected request with invalid auth token", "path", r.URL.Path, "remote_addr", r.RemoteAddr)
	writeResponse(w, http.StatusUnauthorized, "Unauthorized")
}

// resolveAgentID returns the agent making the request. When the token
// identified the agent, a conflicting agent_id in the body is rejected and
// false is returned after writing the response.
func (h *handler) resolveAgentID(w http.ResponseWriter, r *http.Request, bodyAgentID string) (string, bool) {
	tokenAgentID, ok := r.Context().Value(agentIDContextKey{}).(string)
	if !ok {
		return bodyAgentID, true
	}
	if bodyAgentID != "" && bodyAgentID != tokenAgentID {
		h.logger.Warn(
			"Rejected request whose agent_id does not match its token",
			"path", r.URL.Path,
			"agent_id", bodyAgentID,
			"token_agent_id", tokenAgentID,
		)
		writeResponse(w, http.StatusForbidden, "agent_id does not match token")
		return "", false
	}
	return tokenAgentID, true
}

func (h *handler) scmWebhookHandler(w http.ResponseWriter, r *http.Request) {
	h.logger.Debug("SCM webhook handler called")

	webhooks, err := h.scmProvider.ParseWebhook(r)
	if err != nil {
		h.logger.Error("Failed to parse SCM webhook", "error", err)
		writeResponse(w, http.StatusInternalServerError, "Failed to parse SCM webhook")
		return
	}
	if len(webhooks) == 0 {
		h.logger.Debug("No actionable SCM webhook events found")
		writeResponse(w, http.StatusOK, "No actionable event found")
		return
	}

	for _, webhook := range webhooks {
		if h.orchestratorConfig.Enabled() && !h.orchestratorConfig.HasRepo(webhook.RepositorySlug) {
			h.logger.Warn(
				"Ignoring webhook from repository not listed in the orchestrator config",
				"repo", webhook.RepositorySlug,
				"pr", webhook.PRNumber,
				"user", webhook.TriggeringUser,
			)
			continue
		}

		cmd := command.ParseWebhook(&webhook)
		if cmd.Kind == command.KindUnknown {
			h.logger.Debug(
				"Ignoring pull request comment that is not a terraplane command",
				"repo", webhook.RepositorySlug,
				"pr", webhook.PRNumber,
				"user", webhook.TriggeringUser,
				"comment", webhook.FullCommand,
			)
			continue
		}

		if cmd.Kind == command.KindUnlock {
			if err := h.unlockService.RunUnlock(r.Context(), cmd.Unlock); err != nil {
				h.logger.Error(
					"Failed to unlock stacks from webhook",
					"repo", webhook.RepositorySlug,
					"pr", webhook.PRNumber,
					"error", err,
				)
			}
		} else if err := h.jobService.CreatePendingJobs(r.Context(), &webhook); err != nil {
			h.logger.Error(
				"Failed to create pending jobs from webhook",
				"repo", webhook.RepositorySlug,
				"pr", webhook.PRNumber,
				"error", err,
			)
		}

		if err := h.scmPublisher.AcknowledgeComment(
			r.Context(),
			webhook.RepositorySlug,
			webhook.PRNumber,
			webhook.CommentID,
		); err != nil {
			h.logger.Error(
				"Failed to acknowledge pull request comment",
				"repo", webhook.RepositorySlug,
				"pr", webhook.PRNumber,
				"comment_id", webhook.CommentID,
				"error", err,
			)
		}
	}

	writeResponse(w, http.StatusOK, "Webhook parsed successfully")
}

func (h *handler) agentJobClaimHandler(w http.ResponseWriter, r *http.Request) {
	var payload agentJobClaimPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		h.logger.Error("Failed to unmarshal agent job claim request body", "error", err)
		writeResponse(w, http.StatusInternalServerError, "Failed to unmarshal agent job claim request body")
		return
	}
	agentID, ok := h.resolveAgentID(w, r, payload.AgentID)
	if !ok {
		return
	}

	cmd, err := h.jobService.ClaimPendingJob(r.Context(), agentID)
	if err != nil {
		if errors.Is(err, services.ErrEmptyAgentID) {
			writeResponse(w, http.StatusBadRequest, "agent_id is required")
			return
		}
		h.logger.Error("Failed to claim pending job", "error", err)
		writeResponse(w, http.StatusInternalServerError, "Failed to claim pending job")
		return
	}
	if cmd == nil {
		writeNoContent(w)
		return
	}

	writeJSON(w, http.StatusOK, agentJobClaimResponse{Command: *cmd})
}

func (h *handler) agentHeartbeatHandler(w http.ResponseWriter, r *http.Request) {
	var payload agentHeartbeatPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		h.logger.Error("Failed to unmarshal agent heartbeat request body", "error", err)
		writeResponse(w, http.StatusInternalServerError, "Failed to unmarshal agent heartbeat request body")
		return
	}
	agentID, ok := h.resolveAgentID(w, r, payload.AgentID)
	if !ok {
		return
	}

	if err := h.jobService.RefreshAgentClaims(r.Context(), agentID); err != nil {
		h.logger.Error("Failed to refresh agent claims", "error", err)
		writeResponse(w, http.StatusInternalServerError, "Failed to refresh agent claims")
		return
	}

	writeNoContent(w)
}

func (h *handler) agentJobAckHandler(w http.ResponseWriter, r *http.Request) {
	var payload agentJobAckPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		h.logger.Error("Failed to unmarshal agent job ack request body", "error", err)
		writeResponse(w, http.StatusInternalServerError, "Failed to unmarshal agent job ack request body")
		return
	}
	agentID, ok := h.resolveAgentID(w, r, payload.AgentID)
	if !ok {
		return
	}

	jobID := r.PathValue("id")
	if err := h.jobService.AckJob(r.Context(), jobID, agentID); err != nil {
		h.logger.Error("Failed to ack job", "job_id", jobID, "agent_id", agentID, "error", err)
		writeResponse(w, http.StatusInternalServerError, "Failed to ack job")
		return
	}

	writeNoContent(w)
}

func (h *handler) agentJobResultHandler(w http.ResponseWriter, r *http.Request) {
	var payload agentJobResultPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		h.logger.Error("Failed to unmarshal agent job result request body", "error", err)
		writeResponse(w, http.StatusInternalServerError, "Failed to unmarshal agent job result request body")
		return
	}
	agentID, ok := h.resolveAgentID(w, r, payload.AgentID)
	if !ok {
		return
	}

	jobID := r.PathValue("id")
	if err := h.jobService.CommitJobResult(r.Context(), jobID, agentID, payload.Result, payload.Output, payload.Error); err != nil {
		h.logger.Error("Failed to commit job result", "job_id", jobID, "agent_id", agentID, "error", err)
		writeResponse(w, http.StatusInternalServerError, "Failed to commit job result")
		return
	}

	writeNoContent(w)
}

func (h *handler) agentPeriodicResultHandler(w http.ResponseWriter, r *http.Request) {
	var payload agentPeriodicResultPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		h.logger.Error("Failed to unmarshal agent periodic result request body", "error", err)
		writeResponse(w, http.StatusInternalServerError, "Failed to unmarshal agent periodic result request body")
		return
	}
	agentID, ok := h.resolveAgentID(w, r, payload.AgentID)
	if !ok {
		return
	}

	jobID := r.PathValue("id")
	if err := h.jobService.HandlePeriodicResult(r.Context(), jobID, agentID, payload.Output); err != nil {
		h.logger.Error("Failed to handle periodic result", "job_id", jobID, "agent_id", agentID, "error", err)
		writeResponse(w, http.StatusInternalServerError, "Failed to handle periodic result")
		return
	}

	writeNoContent(w)
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *handler) healthCheck(w http.ResponseWriter, r *http.Request) {
	writeResponse(w, http.StatusOK, "OK")
}

func writeResponse(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeNoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}
