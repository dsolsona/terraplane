package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xyzjace/terraplane/config"
	"github.com/xyzjace/terraplane/pkg/scm"
)

// tokenSource supplies the bearer token for GitHub API requests about the
// repositories of one account (an organisation or a user).
type tokenSource interface {
	Token(ctx context.Context, owner string) (string, error)
	// CloneToken mints a token limited to reading the contents of one
	// repository. Only a GitHub App can narrow a token this way.
	CloneToken(ctx context.Context, repo string) (installationToken, error)
}

// staticToken is a personal access token: one token for every account.
type staticToken string

func (t staticToken) Token(context.Context, string) (string, error) {
	if t == "" {
		return "", fmt.Errorf("GitHub credentials are not configured. Please set ORCHESTRATOR_GITHUB_ACCESS_TOKEN or the ORCHESTRATOR_GITHUB_APP_* settings in the environment")
	}
	return string(t), nil
}

func (t staticToken) CloneToken(context.Context, string) (installationToken, error) {
	return installationToken{}, fmt.Errorf("%w: a personal access token cannot be narrowed to one repository; authenticate as a GitHub App", scm.ErrCloneTokenUnavailable)
}

const (
	appJWTLifetime      = 9 * time.Minute // GitHub rejects app JWTs valid for more than ten
	appTokenRefreshSlop = 5 * time.Minute // installation tokens are renewed this long before they expire
)

// appTokenSource authenticates as a GitHub App. A JWT signed with the app's
// private key is exchanged for an installation token, which lasts an hour
// and is scoped to one account's installation. Tokens are cached per
// installation and installations per account.
type appTokenSource struct {
	appID          string
	installationID string // optional; when set, every account uses this installation
	key            *rsa.PrivateKey
	apiURL         string
	httpClient     *http.Client
	now            func() time.Time

	mu            sync.Mutex
	installations map[string]string            // owner → installation ID
	tokens        map[string]installationToken // installation ID → token
}

type installationToken struct {
	value   string
	expires time.Time
}

func newAppTokenSource(appID, installationID string, key *rsa.PrivateKey, apiURL string, httpClient *http.Client) *appTokenSource {
	return &appTokenSource{
		appID:          appID,
		installationID: installationID,
		key:            key,
		apiURL:         apiURL,
		httpClient:     httpClient,
		now:            time.Now,
		installations:  make(map[string]string),
		tokens:         make(map[string]installationToken),
	}
}

func (a *appTokenSource) Token(ctx context.Context, owner string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	installationID, err := a.installation(ctx, owner)
	if err != nil {
		return "", err
	}

	if token, ok := a.tokens[installationID]; ok && a.now().Add(appTokenRefreshSlop).Before(token.expires) {
		return token.value, nil
	}

	token, err := a.createInstallationToken(ctx, installationID, nil)
	if err != nil {
		return "", err
	}
	a.tokens[installationID] = token
	return token.value, nil
}

// CloneToken asks GitHub for a token that can only read repo. Every call
// mints a fresh token: they are handed to agents one job at a time and
// revoked when the job ends, so caching would only widen their exposure.
func (a *appTokenSource) CloneToken(ctx context.Context, repo string) (installationToken, error) {
	owner, name, found := strings.Cut(repo, "/")
	if !found || owner == "" || name == "" {
		return installationToken{}, fmt.Errorf("repository %q is not an owner/name slug", repo)
	}

	a.mu.Lock()
	installationID, err := a.installation(ctx, owner)
	a.mu.Unlock()
	if err != nil {
		return installationToken{}, err
	}

	body, err := json.Marshal(map[string]any{
		"repositories": []string{name},
		"permissions":  map[string]string{"contents": "read", "metadata": "read"},
	})
	if err != nil {
		return installationToken{}, fmt.Errorf("failed to encode clone token request for repository %s: %w", repo, err)
	}
	return a.createInstallationToken(ctx, installationID, body)
}

func (a *appTokenSource) installation(ctx context.Context, owner string) (string, error) {
	if a.installationID != "" {
		return a.installationID, nil
	}
	if owner == "" {
		return "", fmt.Errorf("GitHub App installation lookup needs an account name; set ORCHESTRATOR_GITHUB_APP_INSTALLATION_ID to use a fixed installation")
	}
	if id, ok := a.installations[owner]; ok {
		return id, nil
	}

	id, err := a.lookupInstallation(ctx, owner)
	if err != nil {
		return "", err
	}
	a.installations[owner] = id
	return id, nil
}

// lookupInstallation asks for the account's installation of the app, trying
// the organisation endpoint first and the user endpoint second.
func (a *appTokenSource) lookupInstallation(ctx context.Context, owner string) (string, error) {
	for _, path := range []string{"/orgs/%s/installation", "/users/%s/installation"} {
		id, found, err := a.getInstallation(ctx, a.apiURL+fmt.Sprintf(path, url.PathEscape(owner)))
		if err != nil {
			return "", err
		}
		if found {
			return id, nil
		}
	}
	return "", fmt.Errorf("GitHub App %s is not installed for account %s", a.appID, owner)
}

func (a *appTokenSource) getInstallation(ctx context.Context, u string) (string, bool, error) {
	req, err := a.newAppRequest(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", false, err
	}

	res, err := a.httpClient.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("failed to execute GitHub API request to %s: %w", u, err)
	}
	defer func() { _ = res.Body.Close() }()

	switch res.StatusCode {
	case http.StatusOK:
		var resp struct {
			ID int64 `json:"id"`
		}
		if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
			return "", false, fmt.Errorf("failed to decode GitHub App installation response from %s: %w", u, err)
		}
		return strconv.FormatInt(resp.ID, 10), true, nil
	case http.StatusNotFound:
		_, _ = io.Copy(io.Discard, res.Body)
		return "", false, nil
	default:
		body, _ := io.ReadAll(res.Body)
		return "", false, fmt.Errorf("GitHub API request to %s returned unexpected status %s: %s", u, res.Status, strings.TrimSpace(string(body)))
	}
}

// createInstallationToken exchanges the app JWT for an installation token.
// A nil body yields a token with the installation's full access; a JSON
// body with repositories and permissions narrows it.
func (a *appTokenSource) createInstallationToken(ctx context.Context, installationID string, body []byte) (installationToken, error) {
	u := fmt.Sprintf("%s/app/installations/%s/access_tokens", a.apiURL, url.PathEscape(installationID))
	req, err := a.newAppRequest(ctx, http.MethodPost, u, body)
	if err != nil {
		return installationToken{}, err
	}

	res, err := a.httpClient.Do(req)
	if err != nil {
		return installationToken{}, fmt.Errorf("failed to execute GitHub API request to create an installation token for installation %s: %w", installationID, err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(res.Body)
		return installationToken{}, fmt.Errorf("GitHub API request to create an installation token for installation %s returned unexpected status %s: %s", installationID, res.Status, strings.TrimSpace(string(body)))
	}

	var resp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		return installationToken{}, fmt.Errorf("failed to decode GitHub installation token response for installation %s: %w", installationID, err)
	}
	if resp.Token == "" {
		return installationToken{}, fmt.Errorf("GitHub returned an empty installation token for installation %s", installationID)
	}
	return installationToken{value: resp.Token, expires: resp.ExpiresAt}, nil
}

// newAppRequest builds a request authenticated as the app itself, with a
// freshly signed JWT. body, when not nil, is sent as JSON.
func (a *appTokenSource) newAppRequest(ctx context.Context, method, u string, body []byte) (*http.Request, error) {
	jwt, err := a.jwt()
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return nil, fmt.Errorf("failed to create GitHub API request to %s: %w", u, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// revokeInstallationToken invalidates an installation token. The token
// authenticates its own revocation, so no app credentials are involved. A
// 401 means GitHub no longer recognises the token, which is the outcome
// wanted, so it counts as success.
func (c *client) revokeInstallationToken(ctx context.Context, token string) error {
	u := c.apiURL + "/installation/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return fmt.Errorf("failed to create GitHub API request to revoke an installation token: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	res, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute GitHub API request to revoke an installation token: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	switch res.StatusCode {
	case http.StatusNoContent, http.StatusUnauthorized:
		_, _ = io.Copy(io.Discard, res.Body)
		return nil
	default:
		body, _ := io.ReadAll(res.Body)
		return fmt.Errorf("GitHub API request to revoke an installation token returned unexpected status %s: %s", res.Status, strings.TrimSpace(string(body)))
	}
}

// jwt signs the token GitHub expects from an app: RS256 over a fixed header
// and claims naming the app as issuer. iat is backdated a minute to absorb
// clock skew between this host and GitHub.
func (a *appTokenSource) jwt() (string, error) {
	now := a.now()
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(appJWTLifetime).Unix(),
		"iss": a.appID,
	})
	if err != nil {
		return "", fmt.Errorf("failed to encode GitHub App JWT claims: %w", err)
	}

	signingInput := base64URL([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + base64URL(claims)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("failed to sign GitHub App JWT: %w", err)
	}
	return signingInput + "." + base64URL(signature), nil
}

func base64URL(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// parsePrivateKey accepts the PKCS#1 key GitHub issues and PKCS#8 for keys
// that have been converted.
func parsePrivateKey(pemData []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, fmt.Errorf("GitHub App private key is not PEM encoded")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse GitHub App private key: %w", err)
		}
		return key, nil
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse GitHub App private key: %w", err)
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("GitHub App private key is not an RSA key")
		}
		return rsaKey, nil
	default:
		return nil, fmt.Errorf("GitHub App private key has unsupported PEM type %q", block.Type)
	}
}

// newTokenSource picks personal token or GitHub App authentication from the
// configuration. Neither configured yields a source that fails on first use
// with a message naming the settings, so an orchestrator without GitHub
// credentials still starts, as it does today.
func newTokenSource(cfg *config.Config, apiURL string, httpClient *http.Client) (tokenSource, error) {
	hasToken := cfg.OrchestratorGithubAccessToken != ""
	hasApp := cfg.OrchestratorGithubAppID != "" || cfg.OrchestratorGithubAppPrivateKey != "" || cfg.OrchestratorGithubAppPrivateKeyPath != ""

	if hasToken && hasApp {
		return nil, fmt.Errorf("configure either ORCHESTRATOR_GITHUB_ACCESS_TOKEN or the ORCHESTRATOR_GITHUB_APP_* settings, not both")
	}
	if !hasApp {
		return staticToken(cfg.OrchestratorGithubAccessToken), nil
	}

	if strings.TrimSpace(cfg.OrchestratorGithubAppID) == "" {
		return nil, fmt.Errorf("ORCHESTRATOR_GITHUB_APP_ID is required to authenticate as a GitHub App")
	}

	pemData := cfg.OrchestratorGithubAppPrivateKey
	if pemData == "" && cfg.OrchestratorGithubAppPrivateKeyPath != "" {
		data, err := os.ReadFile(cfg.OrchestratorGithubAppPrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read ORCHESTRATOR_GITHUB_APP_PRIVATE_KEY_PATH: %w", err)
		}
		pemData = string(data)
	}
	if strings.TrimSpace(pemData) == "" {
		return nil, fmt.Errorf("ORCHESTRATOR_GITHUB_APP_PRIVATE_KEY or ORCHESTRATOR_GITHUB_APP_PRIVATE_KEY_PATH is required to authenticate as a GitHub App")
	}

	key, err := parsePrivateKey([]byte(pemData))
	if err != nil {
		return nil, err
	}
	return newAppTokenSource(strings.TrimSpace(cfg.OrchestratorGithubAppID), strings.TrimSpace(cfg.OrchestratorGithubAppInstallationID), key, apiURL, httpClient), nil
}
