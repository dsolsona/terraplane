//go:generate go run -mod=mod github.com/goforj/wire/cmd/wire

//go:build wireinject
// +build wireinject

package cmd

import (
	"github.com/goforj/wire"
	"github.com/xyzjace/terraplane/config"
	"github.com/xyzjace/terraplane/internal/logging"
	"github.com/xyzjace/terraplane/pkg/agent"
	agentorchestrator "github.com/xyzjace/terraplane/pkg/agent/orchestrator"
	"github.com/xyzjace/terraplane/pkg/agent/terraform"
	"github.com/xyzjace/terraplane/pkg/agent/workspace"
	"github.com/xyzjace/terraplane/pkg/orchestrator"
	"github.com/xyzjace/terraplane/pkg/orchestrator/services"
	"github.com/xyzjace/terraplane/pkg/orchestratorconfig"
	scmfactory "github.com/xyzjace/terraplane/pkg/scm/factory"
	"github.com/xyzjace/terraplane/pkg/storage"
	"github.com/xyzjace/terraplane/pkg/webserver"
)

var observableSet = wire.NewSet(logging.NewLogger)

func InitializeOrchestrator() (orchestrator.Manager, error) {
	wire.Build(
		observableSet,
		config.NewConfig,
		orchestratorconfig.New,
		storage.New,
		storage.NewJobRepository,
		storage.NewLockRepository,
		scmfactory.NewProvider,
		scmfactory.NewPublisher,
		webserver.NewHandler,
		webserver.NewServer,
		orchestrator.NewManager,
		services.NewJobService,
		services.NewUnlockService,
		orchestrator.NewDispatcher,
		wire.Bind(new(orchestrator.SchemaChecker), new(*storage.DB)),
	)
	return nil, nil
}

func InitializeAgent() (agent.Manager, error) {
	wire.Build(
		observableSet,
		config.NewConfig,
		scmfactory.NewRepositoryAccess,
		workspace.NewManager,
		terraform.NewManager,
		agentorchestrator.NewClient,
		agent.NewManager,
	)
	return nil, nil
}
