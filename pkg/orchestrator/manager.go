package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/xyzjace/terraplane/config"
	"github.com/xyzjace/terraplane/pkg/log"
	"github.com/xyzjace/terraplane/pkg/orchestratorconfig"
	"github.com/xyzjace/terraplane/pkg/webserver"
	"golang.org/x/sync/errgroup"
)

type Manager interface {
	Start(ctx context.Context) error
}

// SchemaChecker verifies the database schema is migrated before serving traffic.
type SchemaChecker interface {
	RequireCurrentSchema(ctx context.Context) error
}

type manager struct {
	serverShutdownTimer time.Duration
	logger              log.Logger
	server              webserver.Server
	schema              SchemaChecker
	sharedAuthToken     string
	orchestratorConfig  *orchestratorconfig.ConfigFile
	dispatcher          Dispatcher
}

func (o *manager) Start(ctx context.Context) error {
	// With an orchestrator config each agent has its own token, so the shared one is not needed.
	if o.sharedAuthToken == "" && !o.orchestratorConfig.Enabled() {
		return fmt.Errorf("SHARED_AUTH_TOKEN is not configured")
	}

	if err := o.schema.RequireCurrentSchema(ctx); err != nil {
		return err
	}

	group, ctx := errgroup.WithContext(ctx)

	group.Go(func() error {
		o.logger.Debug("Starting web server")

		err := o.server.Start(ctx)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return err
	})

	group.Go(func() error {
		o.logger.Debug("Starting dispatcher")

		err := o.dispatcher.Start(ctx)
		return err
	})

	group.Go(func() error {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(
			context.Background(),
			o.serverShutdownTimer,
		)
		defer cancel()

		_ = o.server.Shutdown(shutdownCtx)
		return o.dispatcher.Shutdown(shutdownCtx)
	})

	if err := group.Wait(); err != nil {
		o.logger.Error(fmt.Sprintf("something went wrong, stopping orchestrator processes: %v", err))
		return err
	}

	return nil
}

func NewManager(
	config *config.Config,
	logger log.Logger,
	server webserver.Server,
	schema SchemaChecker,
	dispatcher Dispatcher,
	orchestratorConfig *orchestratorconfig.ConfigFile,
) Manager {
	return &manager{
		serverShutdownTimer: config.ServerShutdownTimer,
		logger:              logger,
		server:              server,
		schema:              schema,
		sharedAuthToken:     config.SharedAuthToken,
		orchestratorConfig:  orchestratorConfig,
		dispatcher:          dispatcher,
	}
}
