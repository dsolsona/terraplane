package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/xyzjace/terraplane/config"
)

type ClientSuite struct {
	suite.Suite
}

func TestClientSuite(t *testing.T) {
	suite.Run(t, new(ClientSuite))
}

func (s *ClientSuite) newClient(handler http.HandlerFunc) (*client, *httptest.Server) {
	srv := httptest.NewServer(handler)
	s.T().Cleanup(srv.Close)
	return &client{
		accessToken: "token",
		httpClient:  srv.Client(),
		apiURL:      srv.URL,
	}, srv
}

func (s *ClientSuite) TestNewClient() {
	c := NewClient(&config.Config{OrchestratorGithubAccessToken: "tok"})
	require.NotNil(s.T(), c)
}

func (s *ClientSuite) TestGetCommitSHASuccess() {
	c, _ := s.newClient(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(s.T(), "/repos/acme/infra/pulls/7", r.URL.Path)
		require.Equal(s.T(), "Bearer token", r.Header.Get("Authorization"))
		require.Equal(s.T(), "application/vnd.github+json", r.Header.Get("Accept"))
		require.Equal(s.T(), "2022-11-28", r.Header.Get("X-GitHub-Api-Version"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"head": map[string]any{"sha": "deadbeef", "ref": "feature"},
		})
	})

	sha, err := c.GetCommitSHA(context.Background(), "acme/infra", 7)
	require.NoError(s.T(), err)
	require.Equal(s.T(), "deadbeef", sha)
}

func (s *ClientSuite) TestGetCommitSHAEmpty() {
	c, _ := s.newClient(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"head": map[string]any{"sha": ""}})
	})
	_, err := c.GetCommitSHA(context.Background(), "acme/infra", 7)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "empty head commit SHA")
}

func (s *ClientSuite) TestGetCommitSHANonOK() {
	c, _ := s.newClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	})
	_, err := c.GetCommitSHA(context.Background(), "acme/infra", 7)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "unexpected status")
}

func (s *ClientSuite) TestGetCommitSHABadJSON() {
	c, _ := s.newClient(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{not-json`)
	})
	_, err := c.GetCommitSHA(context.Background(), "acme/infra", 7)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to decode")
}

func (s *ClientSuite) TestGetFileSuccessWithRef() {
	content := base64.StdEncoding.EncodeToString([]byte("hello\nworld"))
	// GitHub often wraps base64 with newlines.
	wrapped := content[:4] + "\n" + content[4:]

	c, _ := s.newClient(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(s.T(), "/repos/acme/infra/contents/terraplane.yaml", r.URL.Path)
		require.Equal(s.T(), "abc123", r.URL.Query().Get("ref"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":  wrapped,
			"encoding": "base64",
		})
	})

	got, err := c.GetFile(context.Background(), "acme/infra", "terraplane.yaml", "abc123")
	require.NoError(s.T(), err)
	require.Equal(s.T(), "hello\nworld", got)
}

func (s *ClientSuite) TestGetFileWithoutRevision() {
	content := base64.StdEncoding.EncodeToString([]byte("cfg"))
	c, _ := s.newClient(func(w http.ResponseWriter, r *http.Request) {
		require.Empty(s.T(), r.URL.RawQuery)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":  content,
			"encoding": "base64",
		})
	})
	got, err := c.GetFile(context.Background(), "acme/infra", "README.md", "")
	require.NoError(s.T(), err)
	require.Equal(s.T(), "cfg", got)
}

func (s *ClientSuite) TestGetFileUnsupportedEncoding() {
	c, _ := s.newClient(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":  "hi",
			"encoding": "utf-8",
		})
	})
	_, err := c.GetFile(context.Background(), "acme/infra", "f", "main")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "unsupported file encoding")
}

func (s *ClientSuite) TestGetFileBadBase64() {
	c, _ := s.newClient(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content":  "!!!not-base64!!!",
			"encoding": "base64",
		})
	})
	_, err := c.GetFile(context.Background(), "acme/infra", "f", "main")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to decode base64")
}

func (s *ClientSuite) TestWriteCommentSuccess() {
	c, _ := s.newClient(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(s.T(), http.MethodPost, r.Method)
		require.Equal(s.T(), "/repos/acme/infra/issues/9/comments", r.URL.Path)
		require.Equal(s.T(), "application/json", r.Header.Get("Content-Type"))
		body, err := io.ReadAll(r.Body)
		require.NoError(s.T(), err)
		require.JSONEq(s.T(), `{"body":"hello pr"}`, string(body))
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":1}`)
	})
	id, err := c.WriteComment(context.Background(), "acme/infra", 9, "hello pr")
	require.NoError(s.T(), err)
	require.Equal(s.T(), 1, id)
}

func (s *ClientSuite) TestReactToCommentSuccess() {
	c, _ := s.newClient(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(s.T(), http.MethodPost, r.Method)
		require.Equal(s.T(), "/repos/acme/infra/issues/comments/42/reactions", r.URL.Path)
		require.Equal(s.T(), "application/json", r.Header.Get("Content-Type"))
		require.Equal(s.T(), "Bearer token", r.Header.Get("Authorization"))
		body, err := io.ReadAll(r.Body)
		require.NoError(s.T(), err)
		require.JSONEq(s.T(), `{"content":"+1"}`, string(body))
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":1,"content":"+1"}`)
	})
	require.NoError(s.T(), c.ReactToComment(context.Background(), "acme/infra", 42, "+1"))
}

func (s *ClientSuite) TestReactToCommentNonCreated() {
	c, _ := s.newClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"Reactions not allowed"}`)
	})
	err := c.ReactToComment(context.Background(), "acme/infra", 42, "+1")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "unexpected status")
	require.Contains(s.T(), err.Error(), "Reactions not allowed")
}

func (s *ClientSuite) TestReactToCommentTransportError() {
	c := &client{
		accessToken: "token",
		httpClient:  http.DefaultClient,
		apiURL:      "http://127.0.0.1:1",
	}
	err := c.ReactToComment(context.Background(), "acme/infra", 42, "+1")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to execute GitHub API request to react to comment")
}

func (s *ClientSuite) TestReactToCommentRequestBuildError() {
	c := &client{accessToken: "token", httpClient: http.DefaultClient, apiURL: "http://example.com/\x00"}
	err := c.ReactToComment(context.Background(), "acme/infra", 42, "+1")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to create GitHub API request")
}

func (s *ClientSuite) TestReactToCommentMissingAccessToken() {
	c := &client{accessToken: "", httpClient: http.DefaultClient, apiURL: "http://example.invalid"}
	err := c.ReactToComment(context.Background(), "acme/infra", 42, "+1")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "ORCHESTRATOR_GITHUB_ACCESS_TOKEN")
}

func (s *ClientSuite) TestWriteCommentNonCreated() {
	c, _ := s.newClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"nope"}`)
	})
	_, err := c.WriteComment(context.Background(), "acme/infra", 9, "hello")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "unexpected status")
	require.Contains(s.T(), err.Error(), "nope")
}

func (s *ClientSuite) TestMissingAccessToken() {
	c := &client{accessToken: "", httpClient: http.DefaultClient, apiURL: "http://example.invalid"}
	_, err := c.GetCommitSHA(context.Background(), "acme/infra", 1)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "ORCHESTRATOR_GITHUB_ACCESS_TOKEN")
}

func (s *ClientSuite) TestTransportError() {
	c := &client{
		accessToken: "token",
		httpClient:  http.DefaultClient,
		apiURL:      "http://127.0.0.1:1", // nothing listening
	}
	_, err := c.GetCommitSHA(context.Background(), "acme/infra", 1)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to execute GitHub API request")
}

func (s *ClientSuite) TestAppendRefQueryInvalidURL() {
	_, err := appendRefQuery("://bad", "main")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to parse")
}

func (s *ClientSuite) TestWriteCommentTransportError() {
	c := &client{
		accessToken: "token",
		httpClient:  http.DefaultClient,
		apiURL:      "http://127.0.0.1:1",
	}
	_, err := c.WriteComment(context.Background(), "acme/infra", 1, "x")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to execute GitHub API request to write comment")
}

func (s *ClientSuite) TestGetFileNonOK() {
	c, _ := s.newClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	})
	_, err := c.GetFile(context.Background(), "acme/infra", "missing.yaml", "main")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to fetch file")
}

func (s *ClientSuite) TestGetFileAppendRefQueryError() {
	c := &client{
		accessToken: "token",
		httpClient:  http.DefaultClient,
		apiURL:      "http://example.com",
	}
	// Path with an invalid percent-encoding makes url.Parse fail inside appendRefQuery.
	_, err := c.GetFile(context.Background(), "acme/infra", "%zz", "main")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to build GitHub contents URL")
}

func (s *ClientSuite) TestWriteCommentRequestBuildError() {
	c := &client{accessToken: "token", httpClient: http.DefaultClient, apiURL: "http://example.com/\x00"}
	_, err := c.WriteComment(context.Background(), "acme/infra", 1, "x")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to create GitHub API request")
}

func (s *ClientSuite) TestGetJSONBodyReadError() {
	c := &client{
		accessToken: "token",
		apiURL:      "http://example.com",
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(errReader{}),
				Header:     make(http.Header),
			}, nil
		})},
	}
	_, err := c.GetCommitSHA(context.Background(), "acme/infra", 1)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to read GitHub API response body")
}

func (s *ClientSuite) TestNewRequestInvalidURL() {
	c := &client{accessToken: "token"}
	_, err := c.newRequest(context.Background(), http.MethodGet, "http://example.com/\x00", nil)
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to create GitHub API request")
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (s *ClientSuite) TestTeamMemberActive() {
	c, _ := s.newClient(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(s.T(), "/orgs/acme/teams/platform/memberships/alice", r.URL.Path)
		require.Equal(s.T(), http.MethodGet, r.Method)
		_, _ = io.WriteString(w, `{"state":"active","role":"member"}`)
	})
	member, err := c.TeamMember(context.Background(), "acme", "platform", "alice")
	require.NoError(s.T(), err)
	require.True(s.T(), member)
}

func (s *ClientSuite) TestTeamMemberPending() {
	c, _ := s.newClient(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"state":"pending"}`)
	})
	member, err := c.TeamMember(context.Background(), "acme", "platform", "alice")
	require.NoError(s.T(), err)
	require.False(s.T(), member)
}

func (s *ClientSuite) TestTeamMemberNotFound() {
	c, _ := s.newClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	})
	member, err := c.TeamMember(context.Background(), "acme", "platform", "alice")
	require.NoError(s.T(), err)
	require.False(s.T(), member)
}

func (s *ClientSuite) TestTeamMemberUnexpectedStatus() {
	c, _ := s.newClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"Resource not accessible"}`)
	})
	_, err := c.TeamMember(context.Background(), "acme", "platform", "alice")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "unexpected status")
	require.Contains(s.T(), err.Error(), "Resource not accessible")
}

func (s *ClientSuite) TestTeamMemberBadJSON() {
	c, _ := s.newClient(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{not-json`)
	})
	_, err := c.TeamMember(context.Background(), "acme", "platform", "alice")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to decode")
}

func (s *ClientSuite) TestTeamMemberTransportError() {
	c, srv := s.newClient(func(w http.ResponseWriter, r *http.Request) {})
	srv.Close()
	_, err := c.TeamMember(context.Background(), "acme", "platform", "alice")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "failed to execute")
}

func (s *ClientSuite) TestTeamMemberMissingAccessToken() {
	c := &client{httpClient: http.DefaultClient, apiURL: "http://example.invalid"}
	_, err := c.TeamMember(context.Background(), "acme", "platform", "alice")
	require.Error(s.T(), err)
	require.Contains(s.T(), err.Error(), "ORCHESTRATOR_GITHUB_ACCESS_TOKEN")
}

func (s *ClientSuite) TestTeamMemberEscapesPathSegments() {
	c, _ := s.newClient(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(s.T(), "/orgs/acme/teams/plat%3Fform/memberships/al%20ice%2F..", r.URL.EscapedPath())
		w.WriteHeader(http.StatusNotFound)
	})
	member, err := c.TeamMember(context.Background(), "acme", "plat?form", "al ice/..")
	require.NoError(s.T(), err)
	require.False(s.T(), member)
}
