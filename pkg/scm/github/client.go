package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/xyzjace/terraplane/config"
	"github.com/xyzjace/terraplane/pkg/scm"
)

//go:generate mockgen -source=client.go -destination=mock_github/mock_client.go -package=mock_github

// ErrNotFound is returned when GitHub answers 404 for a resource lookup.
var ErrNotFound = errors.New("not found")

type Client interface {
	GetCommitSHA(ctx context.Context, repo string, prNumber int) (string, error)
	GetFile(ctx context.Context, repo string, path string, revision string) (string, error)
	WriteComment(ctx context.Context, repo string, prNumber int, body string) (int, error)
	UpdateComment(ctx context.Context, repo string, prNumber int, body string, commentID int) error
	ReactToComment(ctx context.Context, repo string, commentID int, reaction string) error
	TeamMember(ctx context.Context, org string, team string, user string) (bool, error)
	GetIssueComment(ctx context.Context, repo string, commentID int) (scm.IssueComment, error)
	PullRequestApproved(ctx context.Context, repo string, prNumber int) (bool, error)
	ChecksGreen(ctx context.Context, repo string, sha string) (bool, error)
	GetPullRequest(ctx context.Context, repo string, prNumber int) (scm.PullRequest, error)
	CommitsBehind(ctx context.Context, repo string, base string, head string) (int, error)
	CloneToken(ctx context.Context, repo string) (scm.CloneToken, error)
	RevokeCloneToken(ctx context.Context, token string) error
}

type client struct {
	tokens     tokenSource
	httpClient *http.Client
	apiURL     string
}

const githubAPIURL = "https://api.github.com"

// repoOwner returns the account part of an owner/name slug.
func repoOwner(repo string) string {
	owner, _, _ := strings.Cut(repo, "/")
	return owner
}

func (c *client) ReactToComment(ctx context.Context, repo string, commentID int, reaction string) error {
	u := fmt.Sprintf("%s/repos/%s/issues/comments/%d/reactions", c.apiURL, repo, commentID)
	payloadBytes, err := json.Marshal(map[string]string{"content": reaction})
	if err != nil {
		return fmt.Errorf("failed to marshal reaction payload for repository %s comment #%d: %w", repo, commentID, err)
	}

	req, err := c.newRequest(ctx, repoOwner(repo), http.MethodPost, u, bytes.NewReader(payloadBytes))
	if err != nil {
		return err
	}

	res, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute GitHub API request to react to comment in repository %s comment #%d: %w", repo, commentID, err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(res.Body)
		return fmt.Errorf("GitHub API request to react to comment in repository %s comment #%d returned unexpected status %s: %s", repo, commentID, res.Status, strings.TrimSpace(string(respBody)))
	}
	_, _ = io.Copy(io.Discard, res.Body)
	return nil
}

func (c *client) GetCommitSHA(ctx context.Context, repo string, prNumber int) (string, error) {
	u := fmt.Sprintf("%s/repos/%s/pulls/%d", c.apiURL, repo, prNumber)
	var resp struct {
		Head struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := c.getJSON(ctx, repoOwner(repo), u, &resp); err != nil {
		return "", fmt.Errorf("failed to fetch pull request head commit for repository %s PR #%d: %w", repo, prNumber, err)
	}
	if resp.Head.SHA == "" {
		return "", fmt.Errorf("GitHub returned an empty head commit SHA for repository %s PR #%d", repo, prNumber)
	}
	return resp.Head.SHA, nil
}

func (c *client) GetFile(ctx context.Context, repo string, path string, revision string) (string, error) {
	u := fmt.Sprintf("%s/repos/%s/contents/%s", c.apiURL, repo, path)
	if revision != "" {
		var err error
		u, err = appendRefQuery(u, revision)
		if err != nil {
			return "", fmt.Errorf("failed to build GitHub contents URL for repository %s file %q at revision %q: %w", repo, path, revision, err)
		}
	}

	var resp struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := c.getJSON(ctx, repoOwner(repo), u, &resp); err != nil {
		return "", fmt.Errorf("failed to fetch file %q at revision %q from repository %s: %w", path, revision, repo, err)
	}
	if resp.Encoding != "base64" {
		return "", fmt.Errorf("GitHub returned unsupported file encoding %q for repository %s file %q at revision %q", resp.Encoding, repo, path, revision)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(resp.Content, "\n", ""))
	if err != nil {
		return "", fmt.Errorf("failed to decode base64 file content for repository %s file %q at revision %q: %w", repo, path, revision, err)
	}
	return string(decoded), nil
}

func (c *client) getJSON(ctx context.Context, owner, u string, out any) error {
	req, err := c.newRequest(ctx, owner, http.MethodGet, u, nil)
	if err != nil {
		return err
	}

	res, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute GitHub API request to %s: %w", u, err)
	}
	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("failed to read GitHub API response body from %s: %w", u, err)
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub API request to %s returned unexpected status %s: %s", u, res.Status, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("failed to decode GitHub API response from %s: %w", u, err)
	}
	return nil
}

// newRequest builds an authenticated request. owner is the account whose
// repositories the request concerns, which selects the installation when
// authenticating as a GitHub App.
func (c *client) newRequest(ctx context.Context, owner, method, u string, body io.Reader) (*http.Request, error) {
	token, err := c.tokens.Token(ctx, owner)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create GitHub API request to %s: %w", u, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *client) WriteComment(ctx context.Context, repo string, prNumber int, body string) (int, error) {
	u := fmt.Sprintf("%s/repos/%s/issues/%d/comments", c.apiURL, repo, prNumber)
	payloadBytes, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return 0, fmt.Errorf("failed to marshal comment payload for repository %s PR #%d: %w", repo, prNumber, err)
	}

	req, err := c.newRequest(ctx, repoOwner(repo), http.MethodPost, u, bytes.NewReader(payloadBytes))
	if err != nil {
		return 0, err
	}

	res, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to execute GitHub API request to write comment to repository %s PR #%d: %w", repo, prNumber, err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(res.Body)
		return 0, fmt.Errorf("GitHub API request to write comment to repository %s PR #%d returned unexpected status %s: %s", repo, prNumber, res.Status, strings.TrimSpace(string(respBody)))
	}
	respBody, _ := io.ReadAll(res.Body)
	var respData commentCreatedResponse
	if err := json.Unmarshal(respBody, &respData); err != nil {
		return 0, fmt.Errorf("failed to decode GitHub API response for write comment: %w", err)
	}
	return respData.ID, nil
}

func (c *client) UpdateComment(ctx context.Context, repo string, prNumber int, body string, commentID int) error {
	u := fmt.Sprintf("%s/repos/%s/issues/comments/%d", c.apiURL, repo, commentID)
	payloadBytes, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return fmt.Errorf("failed to marshal comment payload for repository %s PR #%d: %w", repo, prNumber, err)
	}

	req, err := c.newRequest(ctx, repoOwner(repo), http.MethodPatch, u, bytes.NewReader(payloadBytes))
	if err != nil {
		return err
	}

	res, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute GitHub API request to update comment in repository %s PR #%d: %w", repo, prNumber, err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(res.Body)
		return fmt.Errorf("GitHub API request to update comment in repository %s PR #%d returned unexpected status %s: %s", repo, prNumber, res.Status, strings.TrimSpace(string(respBody)))
	}
	_, _ = io.Copy(io.Discard, res.Body)
	return nil
}

// GetIssueComment fetches a comment so its author and body come from GitHub
// rather than from a webhook body anyone with the secret could forge.
func (c *client) GetIssueComment(ctx context.Context, repo string, commentID int) (scm.IssueComment, error) {
	u := fmt.Sprintf("%s/repos/%s/issues/comments/%d", c.apiURL, repo, commentID)
	req, err := c.newRequest(ctx, repoOwner(repo), http.MethodGet, u, nil)
	if err != nil {
		return scm.IssueComment{}, err
	}

	res, err := c.httpClient.Do(req)
	if err != nil {
		return scm.IssueComment{}, fmt.Errorf("failed to execute GitHub API request to fetch comment #%d in repository %s: %w", commentID, repo, err)
	}
	defer func() { _ = res.Body.Close() }()

	switch res.StatusCode {
	case http.StatusOK:
		var resp struct {
			ID   int    `json:"id"`
			Body string `json:"body"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
			IssueURL string `json:"issue_url"`
		}
		if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
			return scm.IssueComment{}, fmt.Errorf("failed to decode GitHub API response for comment #%d in repository %s: %w", commentID, repo, err)
		}
		return scm.IssueComment{ID: resp.ID, Body: resp.Body, UserLogin: resp.User.Login, IssueURL: resp.IssueURL}, nil
	case http.StatusNotFound:
		_, _ = io.Copy(io.Discard, res.Body)
		return scm.IssueComment{}, fmt.Errorf("comment #%d in repository %s: %w", commentID, repo, ErrNotFound)
	default:
		respBody, _ := io.ReadAll(res.Body)
		return scm.IssueComment{}, fmt.Errorf("GitHub API request to fetch comment #%d in repository %s returned unexpected status %s: %s", commentID, repo, res.Status, strings.TrimSpace(string(respBody)))
	}
}

// PullRequestApproved folds the review list down to each reviewer's latest
// verdict. COMMENTED reviews do not change a verdict; DISMISSED clears it.
// Only the first 100 reviews are considered; pagination is not implemented.
func (c *client) PullRequestApproved(ctx context.Context, repo string, prNumber int) (bool, error) {
	u := fmt.Sprintf("%s/repos/%s/pulls/%d/reviews?per_page=100", c.apiURL, repo, prNumber)
	var reviews []struct {
		State string `json:"state"`
		User  struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := c.getJSON(ctx, repoOwner(repo), u, &reviews); err != nil {
		return false, fmt.Errorf("failed to fetch reviews for repository %s PR #%d: %w", repo, prNumber, err)
	}

	latest := make(map[string]string) // reviewer → latest verdict
	for _, review := range reviews {
		switch review.State {
		case "APPROVED", "CHANGES_REQUESTED":
			latest[review.User.Login] = review.State
		case "DISMISSED":
			delete(latest, review.User.Login)
		}
	}

	approved := false
	for _, state := range latest {
		if state == "CHANGES_REQUESTED" {
			return false, nil
		}
		approved = true
	}
	return approved, nil
}

// ChecksGreen requires the combined commit status to be success (or absent)
// and every check run to have completed without failing. Only the first 100
// check runs are considered; pagination is not implemented.
func (c *client) ChecksGreen(ctx context.Context, repo string, sha string) (bool, error) {
	statusURL := fmt.Sprintf("%s/repos/%s/commits/%s/status", c.apiURL, repo, sha)
	var status struct {
		State      string `json:"state"`
		TotalCount int    `json:"total_count"`
	}
	if err := c.getJSON(ctx, repoOwner(repo), statusURL, &status); err != nil {
		return false, fmt.Errorf("failed to fetch commit status for repository %s commit %s: %w", repo, sha, err)
	}
	if status.TotalCount > 0 && status.State != "success" {
		return false, nil
	}

	runsURL := fmt.Sprintf("%s/repos/%s/commits/%s/check-runs?per_page=100", c.apiURL, repo, sha)
	var runs struct {
		CheckRuns []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := c.getJSON(ctx, repoOwner(repo), runsURL, &runs); err != nil {
		return false, fmt.Errorf("failed to fetch check runs for repository %s commit %s: %w", repo, sha, err)
	}
	for _, run := range runs.CheckRuns {
		if run.Status != "completed" {
			return false, nil
		}
		switch run.Conclusion {
		case "success", "neutral", "skipped":
		default:
			return false, nil
		}
	}
	return true, nil
}

func (c *client) GetPullRequest(ctx context.Context, repo string, prNumber int) (scm.PullRequest, error) {
	u := fmt.Sprintf("%s/repos/%s/pulls/%d", c.apiURL, repo, prNumber)
	var resp struct {
		State          string `json:"state"`
		Draft          bool   `json:"draft"`
		MergeableState string `json:"mergeable_state"`
		Head           struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := c.getJSON(ctx, repoOwner(repo), u, &resp); err != nil {
		return scm.PullRequest{}, fmt.Errorf("failed to fetch pull request for repository %s PR #%d: %w", repo, prNumber, err)
	}
	return scm.PullRequest{
		Open:           resp.State == "open",
		Draft:          resp.Draft,
		MergeableState: resp.MergeableState,
		HeadSHA:        resp.Head.SHA,
		BaseRef:        resp.Base.Ref,
	}, nil
}

// CommitsBehind uses the compare endpoint: behind_by counts commits on base
// that head does not contain.
func (c *client) CommitsBehind(ctx context.Context, repo string, base string, head string) (int, error) {
	u := fmt.Sprintf("%s/repos/%s/compare/%s...%s", c.apiURL, repo, base, head)
	var resp struct {
		BehindBy int `json:"behind_by"`
	}
	if err := c.getJSON(ctx, repoOwner(repo), u, &resp); err != nil {
		return 0, fmt.Errorf("failed to compare %s with %s in repository %s: %w", head, base, repo, err)
	}
	return resp.BehindBy, nil
}

// TeamMember checks org team membership. GitHub answers 404 for non-members
// and for callers whose token lacks read:org, so both read as "not a member".
func (c *client) TeamMember(ctx context.Context, org string, team string, user string) (bool, error) {
	u := fmt.Sprintf("%s/orgs/%s/teams/%s/memberships/%s", c.apiURL, url.PathEscape(org), url.PathEscape(team), url.PathEscape(user))
	req, err := c.newRequest(ctx, org, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}

	res, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("failed to execute GitHub API request to check membership of %s in team %s/%s: %w", user, org, team, err)
	}
	defer func() { _ = res.Body.Close() }()

	switch res.StatusCode {
	case http.StatusOK:
		var resp struct {
			State string `json:"state"`
		}
		if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
			return false, fmt.Errorf("failed to decode GitHub API response for membership of %s in team %s/%s: %w", user, org, team, err)
		}
		return resp.State == "active", nil
	case http.StatusNotFound:
		_, _ = io.Copy(io.Discard, res.Body)
		return false, nil
	default:
		respBody, _ := io.ReadAll(res.Body)
		return false, fmt.Errorf("GitHub API request to check membership of %s in team %s/%s returned unexpected status %s: %s", user, org, team, res.Status, strings.TrimSpace(string(respBody)))
	}
}

func appendRefQuery(u, ref string) (string, error) {
	parsed, err := url.Parse(u)
	if err != nil {
		return "", fmt.Errorf("failed to parse GitHub contents URL %q: %w", u, err)
	}
	q := parsed.Query()
	q.Set("ref", ref)
	parsed.RawQuery = q.Encode()
	return parsed.String(), nil
}

// CloneToken mints a token that reads only repo, for one agent clone.
func (c *client) CloneToken(ctx context.Context, repo string) (scm.CloneToken, error) {
	token, err := c.tokens.CloneToken(ctx, repo)
	if err != nil {
		return scm.CloneToken{}, fmt.Errorf("failed to mint clone token for repository %s: %w", repo, err)
	}
	return scm.CloneToken{Token: token.value, ExpiresAt: token.expires}, nil
}

func (c *client) RevokeCloneToken(ctx context.Context, token string) error {
	return c.revokeInstallationToken(ctx, token)
}

func NewClient(config *config.Config) (Client, error) {
	httpClient := &http.Client{}
	tokens, err := newTokenSource(config, githubAPIURL, httpClient)
	if err != nil {
		return nil, err
	}
	return &client{
		tokens:     tokens,
		httpClient: httpClient,
		apiURL:     githubAPIURL,
	}, nil
}
