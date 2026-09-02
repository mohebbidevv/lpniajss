package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"golaunch/internal/application"
	"golaunch/internal/domain/entities"
	"golaunch/internal/domain/repository"
	"golaunch/internal/infrastructure/config"
	"golaunch/internal/infrastructure/dockerbuild"
	"golaunch/internal/infrastructure/dockerrun"
	"golaunch/internal/infrastructure/dockerx"
	"golaunch/internal/infrastructure/hostexec"
)

// deployStack is the set of swappable pieces the driver decides. Everything
// above it depends only on the interfaces, so this function is the single
// place that knows a concrete runtime exists.
type deployStack struct {
	Runtime repository.Runtime
	Builder repository.ImageBuilder
	Closer  io.Closer
}

func buildDeployStack(ctx context.Context, cfg config.RuntimeConfig) (deployStack, error) {
	switch cfg.Driver {
	case config.DriverHostExec:
		return deployStack{
			Runtime: hostexec.NewHostExecRuntime(),
			Builder: hostexec.NewHostExecImageBuilder(),
		}, nil

	case config.DriverDocker:
		cli, err := dockerx.NewClient(ctx)
		if err != nil {
			return deployStack{}, err
		}
		// the edge network must exist before the first container create
		if err := dockerrun.EnsureNetwork(ctx, cli, cfg.Network); err != nil {
			cli.Close()
			return deployStack{}, err
		}
		return deployStack{
			Runtime: dockerrun.New(cli, dockerrun.Config{
				Network:       cfg.Network,
				ContainerPort: cfg.ContainerPort,
				EndpointMode:  dockerrun.EndpointMode(cfg.EndpointMode),
				DefaultLimits: appLimits(cfg),
			}),
			Builder: dockerbuild.NewDockerImageBuilder(cli),
			Closer:  cli,
		}, nil

	default:
		return deployStack{}, fmt.Errorf("unsupported runtime driver %q", cfg.Driver)
	}
}

func appLimits(cfg config.RuntimeConfig) entities.ResourceLimits {
	return entities.ResourceLimits{
		MemoryMB:   cfg.MemoryMB,
		CPUCores:   cfg.CPUCores,
		PidsLimit:  cfg.PidsLimit,
		ReadOnlyFS: true,
	}
}

func deployConfig(cfg config.RuntimeConfig) application.DeployConfig {
	return application.DeployConfig{
		BuildTimeout: time.Duration(cfg.BuildTimeoutSeconds) * time.Second,
		BuildLimits: entities.ResourceLimits{
			MemoryMB: cfg.BuildMemoryMB,
			CPUCores: cfg.BuildCPUCores,
		},
		AppLimits:    appLimits(cfg),
		ReadyTimeout: time.Duration(cfg.ReadyTimeoutSeconds) * time.Second,
	}
}
