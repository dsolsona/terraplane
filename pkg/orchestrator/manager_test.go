package orchestrator_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xyzjace/terraplane/config"
	"github.com/xyzjace/terraplane/pkg/log"
	"github.com/xyzjace/terraplane/pkg/orchestrator"
	"github.com/xyzjace/terraplane/pkg/orchestratorconfig"
)

type stubSchema struct {
	err error
}

func (s stubSchema) RequireCurrentSchema(context.Context) error { return s.err }

type stubDispatcher struct{}

func (stubDispatcher) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (stubDispatcher) Shutdown(context.Context) error { return nil }

type stubServer struct {
	startErr    error
	shutdownErr error
	started     chan struct{}
	shutdown    chan struct{}
}

func (s *stubServer) Start(ctx context.Context) error {
	if s.started != nil {
		close(s.started)
	}
	if s.startErr != nil {
		return s.startErr
	}
	<-ctx.Done()
	return nil
}

func (s *stubServer) Shutdown(context.Context) error {
	if s.shutdown != nil {
		close(s.shutdown)
	}
	return s.shutdownErr
}

func TestStartMissingAuthToken(t *testing.T) {
	m := orchestrator.NewManager(
		&config.Config{SharedAuthToken: "", ServerShutdownTimer: time.Second},
		log.Noop(),
		&stubServer{},
		stubSchema{},
		stubDispatcher{},
		&orchestratorconfig.ConfigFile{},
	)
	err := m.Start(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "SHARED_AUTH_TOKEN")
}

func TestStartSchemaCheckFailure(t *testing.T) {
	m := orchestrator.NewManager(
		&config.Config{SharedAuthToken: "secret", ServerShutdownTimer: time.Second},
		log.Noop(),
		&stubServer{},
		stubSchema{err: errors.New("migrations pending")},
		stubDispatcher{},
		&orchestratorconfig.ConfigFile{},
	)
	err := m.Start(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "migrations pending")
}

func TestStartAndShutdownOnCancel(t *testing.T) {
	started := make(chan struct{})
	shutdown := make(chan struct{})
	srv := &stubServer{started: started, shutdown: shutdown}

	m := orchestrator.NewManager(
		&config.Config{SharedAuthToken: "secret", ServerShutdownTimer: time.Second},
		log.Noop(),
		srv,
		stubSchema{},
		stubDispatcher{},
		&orchestratorconfig.ConfigFile{},
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Start(ctx) }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server start")
	}
	cancel()

	select {
	case <-shutdown:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server shutdown")
	}

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Start to return")
	}
}

func TestStartPropagatesServerError(t *testing.T) {
	m := orchestrator.NewManager(
		&config.Config{SharedAuthToken: "secret", ServerShutdownTimer: time.Second},
		log.Noop(),
		&stubServer{startErr: errors.New("bind failed")},
		stubSchema{},
		stubDispatcher{},
		&orchestratorconfig.ConfigFile{},
	)

	err := m.Start(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "bind failed")
}

func TestStartWithAgentTokensDoesNotRequireSharedToken(t *testing.T) {
	cfgFile, err := orchestratorconfig.Parse([]byte(`
agents:
  agent-prod:
    token_env: AGENT_TOKEN_PROD
repos:
  acme/infra:
    agent: agent-prod
`), func(string) string { return "prod-token" })
	require.NoError(t, err)

	m := orchestrator.NewManager(
		&config.Config{SharedAuthToken: "", ServerShutdownTimer: time.Second},
		log.Noop(),
		&stubServer{},
		stubSchema{err: errors.New("migrations pending")},
		stubDispatcher{},
		cfgFile,
	)
	err = m.Start(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "migrations pending")
}
