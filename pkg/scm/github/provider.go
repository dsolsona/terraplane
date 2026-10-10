package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/xyzjace/terraplane/config"
	"github.com/xyzjace/terraplane/pkg/log"
	"github.com/xyzjace/terraplane/pkg/scm"
)

const signaturePrefix = "sha256="

type provider struct {
	logger                log.Logger
	github_webhook_secret string
	client                Client
}

func (p *provider) Name() string {
	return "github"
}

func (p *provider) SSHHost() string {
	return "github.com"
}

func (p *provider) CloneURL(slug string) string {
	return fmt.Sprintf("git@github.com:%s.git", slug)
}

func (p *provider) ParseWebhook(r *http.Request) ([]scm.Webhook, error) {
	if _, err := p.verifyWebhookSignature(r); err != nil {
		return nil, err
	}

	githubEvent := r.Header.Get("X-GitHub-Event")
	switch githubEvent {
	case "":
		p.logger.Debug("Ignoring GitHub webhook with missing X-GitHub-Event header")
		return nil, nil
	case "ping":
		p.logger.Debug("Ignoring GitHub ping webhook")
		return nil, nil
	case "issue_comment":
		return p.parseIssueCommentWebhook(r)
	default:
		p.logger.Warn("Ignoring unhandled GitHub webhook event", "event", githubEvent)
		return nil, nil
	}
}

func (p *provider) GetFile(fileName string, revision string, repo string) (string, error) {
	return p.client.GetFile(context.Background(), repo, fileName, revision)
}

func (p *provider) TeamMember(ctx context.Context, org string, team string, user string) (bool, error) {
	return p.client.TeamMember(ctx, org, team, user)
}

func (p *provider) PullRequestApproved(ctx context.Context, repo string, prNumber int) (bool, error) {
	return p.client.PullRequestApproved(ctx, repo, prNumber)
}

func (p *provider) ChecksGreen(ctx context.Context, repo string, sha string) (bool, error) {
	return p.client.ChecksGreen(ctx, repo, sha)
}

func (p *provider) parseIssueCommentWebhook(r *http.Request) ([]scm.Webhook, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read GitHub issue comment webhook request body: %w", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	var payload issueCommentWebhook
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("failed to unmarshal GitHub issue comment webhook payload: %w", err)
	}

	if payload.Comment.Body == "" {
		return nil, fmt.Errorf("GitHub issue comment webhook contained an empty comment body for repository %s issue #%d", payload.Repository.FullName, payload.Issue.Number)
	}

	if payload.Action != "created" {
		p.logger.Debug(
			"Ignoring GitHub issue comment webhook because the action is not created",
			"repo", payload.Repository.FullName,
			"issue", payload.Issue.Number,
			"action", payload.Action,
		)
		return nil, nil
	}

	if payload.Issue.PullRequest == nil {
		p.logger.Debug(
			"Ignoring GitHub issue comment webhook because the comment was not left on a pull request",
			"repo", payload.Repository.FullName,
			"issue", payload.Issue.Number,
		)
		return nil, nil
	}

	repo := payload.Repository.FullName
	prNumber := payload.Issue.Number

	if !looksLikeCommand(payload.Comment.Body) {
		p.logger.Debug(
			"Ignoring GitHub issue comment webhook because the comment is not a terraplane command",
			"repo", repo,
			"issue", prNumber,
		)
		return nil, nil
	}

	p.logger.Debug(
		"Processing GitHub pull request comment webhook",
		"repo", repo,
		"pr", prNumber,
		"user", payload.Comment.User.Login,
	)

	// The webhook body is only trusted to tell us where to look. The author,
	// body, and issue come from the API so a forged or stale delivery cannot
	// put words in someone's mouth.
	comment, err := p.client.GetIssueComment(context.Background(), repo, int(payload.Comment.ID))
	if errors.Is(err, ErrNotFound) {
		p.logger.Warn(
			"Ignoring GitHub issue comment webhook because the comment no longer exists",
			"repo", repo,
			"pr", prNumber,
			"comment_id", payload.Comment.ID,
		)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to fetch comment #%d for repository %s PR #%d: %w", payload.Comment.ID, repo, prNumber, err)
	}

	issueRepo, issueNumber, err := parseIssueURL(comment.IssueURL)
	if err != nil {
		return nil, fmt.Errorf("failed to verify comment #%d for repository %s PR #%d: %w", comment.ID, repo, prNumber, err)
	}
	if !strings.EqualFold(issueRepo, repo) || issueNumber != prNumber {
		p.logger.Warn(
			"Ignoring GitHub issue comment webhook because the comment belongs to a different issue",
			"repo", repo,
			"pr", prNumber,
			"comment_id", comment.ID,
			"comment_repo", issueRepo,
			"comment_issue", issueNumber,
		)
		return nil, nil
	}

	commitSHA, err := p.client.GetCommitSHA(context.Background(), issueRepo, issueNumber)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve pull request head commit for repository %s PR #%d: %w", issueRepo, issueNumber, err)
	}

	webhook := scm.Webhook{
		RepositorySlug: issueRepo,
		PRNumber:       issueNumber,
		FullCommand:    comment.Body,
		TriggeringUser: comment.UserLogin,
		CommitSHA:      commitSHA,
		CommentID:      comment.ID,
	}

	p.logger.Info(
		"Received pull request comment webhook",
		"repo", webhook.RepositorySlug,
		"pr", webhook.PRNumber,
		"user", webhook.TriggeringUser,
		"commit", webhook.CommitSHA,
		"comment", webhook.FullCommand,
	)

	return []scm.Webhook{webhook}, nil
}

// looksLikeCommand is a cheap filter so ordinary PR chatter costs no API
// calls. The command parser makes the real decision on the body fetched
// from the API.
func looksLikeCommand(body string) bool {
	line := strings.TrimSpace(body)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	fields := strings.Fields(line)
	return len(fields) > 0 && strings.EqualFold(fields[0], "terraplane")
}

// parseIssueURL extracts owner/repo and the issue number from an API issue
// URL such as https://api.github.com/repos/acme/infra/issues/42. GitHub
// Enterprise prefixes the path with /api/v3, so the repos segment is located
// rather than assumed.
func parseIssueURL(raw string) (string, int, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", 0, fmt.Errorf("parse issue URL %q: %w", raw, err)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	i := slices.Index(parts, "repos")
	if i < 0 || len(parts) != i+5 || parts[i+3] != "issues" {
		return "", 0, fmt.Errorf("unexpected issue URL %q", raw)
	}
	number, err := strconv.Atoi(parts[i+4])
	if err != nil {
		return "", 0, fmt.Errorf("unexpected issue number in URL %q: %w", raw, err)
	}
	return parts[i+1] + "/" + parts[i+2], number, nil
}

func (p *provider) verifyWebhookSignature(r *http.Request) ([]byte, error) {
	if p.github_webhook_secret == "" {
		return nil, fmt.Errorf("GitHub webhook secret is not configured. Please set ORCHESTRATOR_GITHUB_WEBHOOK_SECRET in the environment")
	}

	signature := r.Header.Get("X-Hub-Signature-256")
	if signature == "" {
		return nil, fmt.Errorf("GitHub webhook request is missing the X-Hub-Signature-256 header")
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read GitHub webhook request body during signature verification: %w", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	mac := hmac.New(sha256.New, []byte(p.github_webhook_secret))
	mac.Write(body)
	expected := signaturePrefix + hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return nil, fmt.Errorf("GitHub webhook signature verification failed")
	}

	return body, nil
}

var _ scm.RepositoryAccess = (*provider)(nil)

func newProvider(logger log.Logger, cfg *config.Config, client Client) *provider {
	return &provider{logger: logger, github_webhook_secret: cfg.OrchestratorGithubWebhookSecret, client: client}
}

func NewProvider(logger log.Logger, config *config.Config, client Client) scm.Provider {
	return newProvider(logger, config, client)
}

func NewRepositoryAccess(logger log.Logger, cfg *config.Config, client Client) scm.RepositoryAccess {
	return newProvider(logger, cfg, client)
}
