package github

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xyzjace/terraplane/pkg/scm"
)

func TestStaticTokenCannotMintCloneTokens(t *testing.T) {
	_, err := staticToken("tok").CloneToken(t.Context(), "acme/infra")
	require.ErrorIs(t, err, scm.ErrCloneTokenUnavailable)
	require.Contains(t, err.Error(), "GitHub App")
}

func TestAppCloneTokenIsScopedToTheRepository(t *testing.T) {
	app := newFakeGitHubApp(t, testRSAKey(t))
	src := app.source(app.server(), "")

	token, err := src.CloneToken(t.Context(), "acme/infra")
	require.NoError(t, err)
	require.Equal(t, "ghs_42_1", token.value)
	require.WithinDuration(t, app.expiresAt, token.expires, time.Second)

	require.Len(t, app.tokenRequests, 1)
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(app.tokenRequests[0]), &body))
	require.Equal(t, map[string]any{
		"repositories": []any{"infra"},
		"permissions":  map[string]any{"contents": "read", "metadata": "read"},
	}, body)
}

func TestAppCloneTokenIsNeverCached(t *testing.T) {
	app := newFakeGitHubApp(t, testRSAKey(t))
	src := app.source(app.server(), "42")

	first, err := src.CloneToken(t.Context(), "acme/infra")
	require.NoError(t, err)
	second, err := src.CloneToken(t.Context(), "acme/infra")
	require.NoError(t, err)
	require.NotEqual(t, first.value, second.value)
	require.Equal(t, 2, app.tokenCalls)

	// The account-wide token is unaffected by clone tokens: it is minted
	// without a body and cached as before.
	wide, err := src.Token(t.Context(), "acme")
	require.NoError(t, err)
	require.NotEqual(t, second.value, wide)
	require.Equal(t, "", app.tokenRequests[2])
}

func TestAppCloneTokenRejectsMalformedSlug(t *testing.T) {
	app := newFakeGitHubApp(t, testRSAKey(t))
	src := app.source(app.server(), "42")

	for _, repo := range []string{"", "infra", "acme/", "/infra"} {
		_, err := src.CloneToken(t.Context(), repo)
		require.Error(t, err, repo)
		require.Contains(t, err.Error(), "not an owner/name slug", repo)
	}
	require.Zero(t, app.tokenCalls)
}

func TestAppCloneTokenInstallationLookupFailure(t *testing.T) {
	app := newFakeGitHubApp(t, testRSAKey(t))
	src := app.source(app.server(), "")

	_, err := src.CloneToken(t.Context(), "nobody/infra")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not installed for account nobody")
}

func TestAppCloneTokenCreateFailure(t *testing.T) {
	app := newFakeGitHubApp(t, testRSAKey(t))
	app.tokenStatus = http.StatusForbidden
	app.tokenBody = `{"message":"forbidden"}`
	src := app.source(app.server(), "42")

	_, err := src.CloneToken(t.Context(), "acme/infra")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unexpected status 403")
}

func TestClientCloneToken(t *testing.T) {
	app := newFakeGitHubApp(t, testRSAKey(t))
	srv := app.server()
	c := &client{tokens: app.source(srv, "42"), httpClient: srv.Client(), apiURL: srv.URL}

	token, err := c.CloneToken(t.Context(), "acme/infra")
	require.NoError(t, err)
	require.Equal(t, scm.CloneToken{Token: "ghs_42_1", ExpiresAt: token.ExpiresAt}, token)
	require.WithinDuration(t, app.expiresAt, token.ExpiresAt, time.Second)
}

func TestClientCloneTokenWrapsErrors(t *testing.T) {
	c := &client{tokens: staticToken("tok"), httpClient: http.DefaultClient, apiURL: "http://example.invalid"}

	_, err := c.CloneToken(t.Context(), "acme/infra")
	require.ErrorIs(t, err, scm.ErrCloneTokenUnavailable)
	require.Contains(t, err.Error(), "failed to mint clone token for repository acme/infra")
}

func TestClientRevokeCloneToken(t *testing.T) {
	cases := map[string]struct {
		status  int
		wantErr string
	}{
		"revoked":         {http.StatusNoContent, ""},
		"already invalid": {http.StatusUnauthorized, ""},
		"server error":    {http.StatusInternalServerError, "unexpected status 500"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			app := newFakeGitHubApp(t, testRSAKey(t))
			app.revokeStatus = tc.status
			srv := app.server()
			c := &client{tokens: staticToken("tok"), httpClient: srv.Client(), apiURL: srv.URL}

			err := c.RevokeCloneToken(t.Context(), "ghs_gone")
			require.Equal(t, []string{"ghs_gone"}, app.revoked)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestClientRevokeCloneTokenRequestFailures(t *testing.T) {
	c := &client{tokens: staticToken("tok"), httpClient: http.DefaultClient, apiURL: "http://example.com/\x00"}
	err := c.RevokeCloneToken(context.Background(), "ghs_x")
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to create GitHub API request to revoke")

	app := newFakeGitHubApp(t, testRSAKey(t))
	srv := app.server()
	srv.Close()
	c = &client{tokens: staticToken("tok"), httpClient: http.DefaultClient, apiURL: srv.URL}
	err = c.RevokeCloneToken(context.Background(), "ghs_x")
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to execute GitHub API request to revoke")
}
