package buildfarm

import (
	"errors"
	"fmt"

	"github.com/containers/common/pkg/config"
	"github.com/containers/podman/v4/cmd/podman/common"
	"github.com/containers/podman/v4/cmd/podman/registry"
	"github.com/containers/podman/v4/pkg/domain/entities"
	"github.com/spf13/cobra"
)

var (
	buildfarmRmDescription = `Remove one or more existing farms.`
	rmCommand              = &cobra.Command{
		Use:               "rm [options] FARM [FARM...]",
		Aliases:           []string{"remove"},
		Short:             "Remove one or more farms",
		Long:              buildfarmRmDescription,
		RunE:              rm,
		ValidArgsFunction: common.AutocompleteFarms,
		Example: `podman buildfarm rm myfarm1 myfarm2
  podman buildfarm rm --all`,
	}
)

var (
	rmOptions = entities.BuildfarmRmOptions{}
)

func init() {
	registry.Commands = append(registry.Commands, registry.CliCommand{
		Command: rmCommand,
		Parent:  buildfarmCmd,
	})
	flags := rmCommand.Flags()
	flags.BoolVarP(&rmOptions.All, "all", "a", false, "Remove all farms")
}

func rm(cmd *cobra.Command, args []string) error {
	cfg, err := config.ReadCustomConfig()
	if err != nil {
		return err
	}

	if rmOptions.All {
		if cfg.Engine.Farms != nil {
			for k := range cfg.Engine.Farms {
				delete(cfg.Engine.Farms, k)
				fmt.Printf("Farm %q deleted\n", k)
			}
		}
		cfg.Engine.DefaultFarm = ""
		return cfg.Write()
	}

	if len(args) == 0 {
		return errors.New("requires at lease 1 arg(s), received 0")
	}

	if cfg.Engine.Farms != nil {
		for _, k := range args {
			delete(cfg.Engine.Farms, k)
			if k == cfg.Engine.DefaultFarm {
				cfg.Engine.DefaultFarm = ""
			}
		}
	}
	// Set a new default farm if the current default farm has been removed
	if cfg.Engine.DefaultFarm == "" && cfg.Engine.Farms != nil {
		for k := range cfg.Engine.Farms {
			cfg.Engine.DefaultFarm = k
			break
		}
	}
	if err := cfg.Write(); err != nil {
		return err
	}

	for _, k := range args {
		fmt.Printf("Farm %q deleted\n", k)
	}
	return nil
}
