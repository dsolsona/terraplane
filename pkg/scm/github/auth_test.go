package github

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xyzjace/terraplane/config"
)

var (
	testKeyOnce sync.Once
	testKey     *rsa.PrivateKey
)

// testRSAKey is generated once per test binary; 2048-bit keys are slow to make.
func testRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	testKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = key
	})
	return testKey
}

func pkcs1PEM(key *rsa.PrivateKey) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func pkcs8PEM(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// fakeGitHubApp serves the three endpoints app authentication touches and
// verifies every JWT it receives against the app's public key.
type fakeGitHubApp struct {
	t             *testing.T
	key           *rsa.PrivateKey
	appID         string
	installations map[string]int64 // "orgs/acme" or "users/dave" → installation ID
	tokenCalls    int
	tokenStatus   int
	tokenBody     string
	expiresAt     time.Time
	tokenRequests []string // JSON bodies of access_tokens requests, "" when none
	revokeStatus  int
	revoked       []string // tokens presented to DELETE /installation/token
}

func newFakeGitHubApp(t *testing.T, key *rsa.PrivateKey) *fakeGitHubApp {
	t.Helper()
	return &fakeGitHubApp{
		t:             t,
		key:           key,
		appID:         "12345",
		installations: map[string]int64{"orgs/acme": 42, "users/dave": 7},
		tokenStatus:   http.StatusCreated,
		expiresAt:     time.Now().Add(time.Hour),
		revokeStatus:  http.StatusNoContent,
	}
}

func (f *fakeGitHubApp) verifyJWT(r *http.Request) {
	f.t.Helper()
	header := r.Header.Get("Authorization")
	require.True(f.t, strings.HasPrefix(header, "Bearer "), "missing bearer JWT")
	parts := strings.Split(strings.TrimPrefix(header, "Bearer "), ".")
	require.Len(f.t, parts, 3)

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(f.t, err)
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	require.NoError(f.t, rsa.VerifyPKCS1v15(&f.key.PublicKey, crypto.SHA256, digest[:], signature), "JWT signature does not verify")

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(f.t, err)
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	require.NoError(f.t, json.Unmarshal(claimsJSON, &claims))
	require.Equal(f.t, f.appID, claims.Iss)
	// The source may run on an injected clock, so check the shape, not the wall time:
	// a minute of backdating plus nine minutes of validity, under GitHub's ten-minute cap.
	require.Less(f.t, claims.Iat, claims.Exp)
	require.LessOrEqual(f.t, claims.Exp-claims.Iat, int64(600))
}

func (f *fakeGitHubApp) server() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "installation/token" {
			// Revocation is authenticated by the token itself, not the app JWT.
			require.Equal(f.t, http.MethodDelete, r.Method)
			auth := r.Header.Get("Authorization")
			require.True(f.t, strings.HasPrefix(auth, "token "), "revocation must authenticate with the token")
			f.revoked = append(f.revoked, strings.TrimPrefix(auth, "token "))
			w.WriteHeader(f.revokeStatus)
			return
		}
		f.verifyJWT(r)
		switch {
		case strings.HasSuffix(path, "/installation"):
			id, ok := f.installations[strings.TrimSuffix(path, "/installation")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = fmt.Fprintf(w, `{"id":%d}`, id)
		case strings.HasPrefix(path, "app/installations/") && strings.HasSuffix(path, "/access_tokens"):
			require.Equal(f.t, http.MethodPost, r.Method)
			f.tokenCalls++
			body, err := io.ReadAll(r.Body)
			require.NoError(f.t, err)
			f.tokenRequests = append(f.tokenRequests, string(body))
			w.WriteHeader(f.tokenStatus)
			if f.tokenBody != "" {
				_, _ = io.WriteString(w, f.tokenBody)
				return
			}
			id := strings.TrimSuffix(strings.TrimPrefix(path, "app/installations/"), "/access_tokens")
			_, _ = fmt.Fprintf(w, `{"token":"ghs_%s_%d","expires_at":%q}`, id, f.tokenCalls, f.expiresAt.Format(time.RFC3339))
		default:
			http.NotFound(w, r)
		}
	}))
	f.t.Cleanup(srv.Close)
	return srv
}

func (f *fakeGitHubApp) source(srv *httptest.Server, installationID string) *appTokenSource {
	return newAppTokenSource(f.appID, installationID, f.key, srv.URL, srv.Client())
}

func TestStaticToken(t *testing.T) {
	token, err := staticToken("tok").Token(t.Context(), "acme")
	require.NoError(t, err)
	require.Equal(t, "tok", token)

	_, err = staticToken("").Token(t.Context(), "acme")
	require.Error(t, err)
	require.Contains(t, err.Error(), "ORCHESTRATOR_GITHUB_ACCESS_TOKEN")
}

func TestAppTokenLooksUpInstallationAndCaches(t *testing.T) {
	app := newFakeGitHubApp(t, testRSAKey(t))
	src := app.source(app.server(), "")

	token, err := src.Token(t.Context(), "acme")
	require.NoError(t, err)
	require.Equal(t, "ghs_42_1", token)

	again, err := src.Token(t.Context(), "acme")
	require.NoError(t, err)
	require.Equal(t, token, again)
	require.Equal(t, 1, app.tokenCalls, "cached token must be reused")
}

func TestAppTokenFallsBackToUserInstallation(t *testing.T) {
	app := newFakeGitHubApp(t, testRSAKey(t))
	src := app.source(app.server(), "")

	token, err := src.Token(t.Context(), "dave")
	require.NoError(t, err)
	require.Equal(t, "ghs_7_1", token)
}

func TestAppTokenNotInstalled(t *testing.T) {
	app := newFakeGitHubApp(t, testRSAKey(t))
	src := app.source(app.server(), "")

	_, err := src.Token(t.Context(), "nobody")
	require.Error(t, err)
	require.Contains(t, err.Error(), "GitHub App 12345 is not installed for account nobody")
}

func TestAppTokenFixedInstallationSkipsLookup(t *testing.T) {
	app := newFakeGitHubApp(t, testRSAKey(t))
	app.installations = nil // any lookup would 404
	src := app.source(app.server(), "99")

	token, err := src.Token(t.Context(), "")
	require.NoError(t, err)
	require.Equal(t, "ghs_99_1", token)
}

func TestAppTokenEmptyOwnerWithoutFixedInstallation(t *testing.T) {
	app := newFakeGitHubApp(t, testRSAKey(t))
	src := app.source(app.server(), "")

	_, err := src.Token(t.Context(), "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "ORCHESTRATOR_GITHUB_APP_INSTALLATION_ID")
}

func TestAppTokenRefreshesBeforeExpiry(t *testing.T) {
	app := newFakeGitHubApp(t, testRSAKey(t))
	app.expiresAt = time.Now().Add(time.Hour)
	src := app.source(app.server(), "42")

	now := time.Now()
	src.now = func() time.Time { return now }

	first, err := src.Token(t.Context(), "acme")
	require.NoError(t, err)

	// Still well within the hour: reuse.
	now = now.Add(30 * time.Minute)
	same, err := src.Token(t.Context(), "acme")
	require.NoError(t, err)
	require.Equal(t, first, same)

	// Inside the refresh window: a new token is minted.
	now = now.Add(26 * time.Minute)
	renewed, err := src.Token(t.Context(), "acme")
	require.NoError(t, err)
	require.NotEqual(t, first, renewed)
	require.Equal(t, 2, app.tokenCalls)
}

func TestAppTokenCreateFailures(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		want   string
	}{
		"unexpected status": {http.StatusForbidden, `{"message":"forbidden"}`, "unexpected status 403"},
		"bad json":          {http.StatusCreated, `{not-json`, "failed to decode"},
		"empty token":       {http.StatusCreated, `{"token":""}`, "empty installation token"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			app := newFakeGitHubApp(t, testRSAKey(t))
			app.tokenStatus = tc.status
			app.tokenBody = tc.body
			src := app.source(app.server(), "42")

			_, err := src.Token(t.Context(), "acme")
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestAppTokenInstallationLookupFailures(t *testing.T) {
	t.Run("unexpected status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)
		src := newAppTokenSource("1", "", testRSAKey(t), srv.URL, srv.Client())

		_, err := src.Token(t.Context(), "acme")
		require.Error(t, err)
		require.Contains(t, err.Error(), "unexpected status 500")
	})

	t.Run("bad json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{not-json`)
		}))
		t.Cleanup(srv.Close)
		src := newAppTokenSource("1", "", testRSAKey(t), srv.URL, srv.Client())

		_, err := src.Token(t.Context(), "acme")
		require.Error(t, err)
		require.Contains(t, err.Error(), "failed to decode GitHub App installation response")
	})

	t.Run("transport error", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		srv.Close()
		src := newAppTokenSource("1", "", testRSAKey(t), srv.URL, srv.Client())

		_, err := src.Token(t.Context(), "acme")
		require.Error(t, err)
		require.Contains(t, err.Error(), "failed to execute")

		_, err = newAppTokenSource("1", "42", testRSAKey(t), srv.URL, srv.Client()).Token(t.Context(), "acme")
		require.Error(t, err)
		require.Contains(t, err.Error(), "failed to execute")
	})

	t.Run("bad api url", func(t *testing.T) {
		src := newAppTokenSource("1", "", testRSAKey(t), "http://example.com/\x00", http.DefaultClient)
		_, err := src.Token(t.Context(), "acme")
		require.Error(t, err)
		require.Contains(t, err.Error(), "failed to create GitHub API request")
	})
}

func TestClientAuthenticatesWithAppToken(t *testing.T) {
	// End to end: the client asks the app source for a token and sends it as the bearer.
	app := newFakeGitHubApp(t, testRSAKey(t))
	appSrv := app.server()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer ghs_42_1", r.Header.Get("Authorization"))
		require.Equal(t, "/repos/acme/infra/pulls/7", r.URL.Path)
		_, _ = io.WriteString(w, `{"head":{"sha":"deadbeef"}}`)
	}))
	t.Cleanup(api.Close)

	c := &client{tokens: app.source(appSrv, ""), httpClient: api.Client(), apiURL: api.URL}
	sha, err := c.GetCommitSHA(t.Context(), "acme/infra", 7)
	require.NoError(t, err)
	require.Equal(t, "deadbeef", sha)
}

func TestParsePrivateKey(t *testing.T) {
	key := testRSAKey(t)

	parsed, err := parsePrivateKey([]byte(pkcs1PEM(key)))
	require.NoError(t, err)
	require.True(t, parsed.Equal(key))

	parsed, err = parsePrivateKey([]byte(pkcs8PEM(t, key)))
	require.NoError(t, err)
	require.True(t, parsed.Equal(key))

	_, err = parsePrivateKey([]byte("not a key"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "not PEM encoded")

	_, err = parsePrivateKey([]byte(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("garbage")})))
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to parse")

	_, err = parsePrivateKey([]byte(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("garbage")})))
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to parse")

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	_, err = parsePrivateKey([]byte(pkcs8PEM(t, ecKey)))
	require.Error(t, err)
	require.Contains(t, err.Error(), "not an RSA key")

	_, err = parsePrivateKey([]byte(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")})))
	require.Error(t, err)
	require.Contains(t, err.Error(), `unsupported PEM type "CERTIFICATE"`)
}

func TestNewTokenSource(t *testing.T) {
	key := testRSAKey(t)
	keyPath := filepath.Join(t.TempDir(), "app.pem")
	require.NoError(t, os.WriteFile(keyPath, []byte(pkcs1PEM(key)), 0o600))

	t.Run("nothing configured yields a source that fails on use", func(t *testing.T) {
		src, err := newTokenSource(&config.Config{}, githubAPIURL, http.DefaultClient)
		require.NoError(t, err)
		_, err = src.Token(context.Background(), "acme")
		require.Error(t, err)
	})

	t.Run("personal token", func(t *testing.T) {
		src, err := newTokenSource(&config.Config{OrchestratorGithubAccessToken: "tok"}, githubAPIURL, http.DefaultClient)
		require.NoError(t, err)
		token, err := src.Token(t.Context(), "acme")
		require.NoError(t, err)
		require.Equal(t, "tok", token)
	})

	t.Run("app with inline key", func(t *testing.T) {
		src, err := newTokenSource(&config.Config{
			OrchestratorGithubAppID:             " 12345 ",
			OrchestratorGithubAppInstallationID: "42",
			OrchestratorGithubAppPrivateKey:     pkcs1PEM(key),
		}, githubAPIURL, http.DefaultClient)
		require.NoError(t, err)
		app, ok := src.(*appTokenSource)
		require.True(t, ok)
		require.Equal(t, "12345", app.appID)
		require.Equal(t, "42", app.installationID)
	})

	t.Run("app with key file", func(t *testing.T) {
		src, err := newTokenSource(&config.Config{
			OrchestratorGithubAppID:             "12345",
			OrchestratorGithubAppPrivateKeyPath: keyPath,
		}, githubAPIURL, http.DefaultClient)
		require.NoError(t, err)
		require.IsType(t, &appTokenSource{}, src)
	})

	failures := map[string]struct {
		cfg  config.Config
		want string
	}{
		"token and app":   {config.Config{OrchestratorGithubAccessToken: "tok", OrchestratorGithubAppID: "1"}, "not both"},
		"missing app id":  {config.Config{OrchestratorGithubAppPrivateKey: pkcs1PEM(key)}, "ORCHESTRATOR_GITHUB_APP_ID is required"},
		"missing key":     {config.Config{OrchestratorGithubAppID: "1"}, "ORCHESTRATOR_GITHUB_APP_PRIVATE_KEY or ORCHESTRATOR_GITHUB_APP_PRIVATE_KEY_PATH is required"},
		"unreadable path": {config.Config{OrchestratorGithubAppID: "1", OrchestratorGithubAppPrivateKeyPath: filepath.Join(t.TempDir(), "missing.pem")}, "failed to read ORCHESTRATOR_GITHUB_APP_PRIVATE_KEY_PATH"},
		"bad key":         {config.Config{OrchestratorGithubAppID: "1", OrchestratorGithubAppPrivateKey: "nope"}, "not PEM encoded"},
	}
	for name, tc := range failures {
		t.Run(name, func(t *testing.T) {
			_, err := newTokenSource(&tc.cfg, githubAPIURL, http.DefaultClient)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}
