package buildfarm

import (
	"fmt"

	"github.com/containers/common/pkg/completion"
	"github.com/containers/common/pkg/config"
	"github.com/containers/podman/v4/cmd/podman/registry"
	"github.com/containers/podman/v4/cmd/podman/utils"
	"github.com/containers/podman/v4/pkg/domain/entities"
	"github.com/containers/podman/v4/pkg/util"
	"github.com/spf13/cobra"
)

var (
	buildfarmCreateDescription = `Create a new buildfarm with connections added via podman system connection add`

	createCommand = &cobra.Command{
		Use:               "create [options] [NAME]",
		Args:              cobra.MinimumNArgs(1),
		Short:             "Create a new farm",
		Long:              buildfarmCreateDescription,
		RunE:              create,
		ValidArgsFunction: completion.AutocompleteNone,
		Example: `podman buildfarm create myfarm connection1
  podman buildfarm create myfarm`,
	}
)

var (
	createOptions entities.BuildfarmCreateOptions
)

func init() {
	registry.Commands = append(registry.Commands, registry.CliCommand{
		Command: createCommand,
		Parent:  buildfarmCmd,
	})
	flags := createCommand.Flags()

	ignoreFlagName := "ignore"
	flags.BoolVar(&createOptions.Ignore, ignoreFlagName, false, "Don't fail if volume already exists")

	flags.SetNormalizeFunc(utils.AliasFlags)
}

func create(cmd *cobra.Command, args []string) error {
	farmName := args[0]
	connections := args[1:]

	cfg, err := config.ReadCustomConfig()
	if err != nil {
		return err
	}

	if cfg.Engine.Farms == nil {
		cfg.Engine.Farms = make(map[string][]string)
	}

	if len(connections) == 0 {
		cfg.Engine.Farms[farmName] = []string{}
	}

	for _, c := range connections {
		if _, ok := cfg.Engine.ServiceDestinations[c]; ok {
			if util.StringInSlice(c, cfg.Engine.Farms[farmName]) {
				// Don't add duplicate connections to a farm
				continue
			}
			cfg.Engine.Farms[farmName] = append(cfg.Engine.Farms[farmName], c)
		} else {
			return fmt.Errorf("cannot create farm, %q is not a system connection", c)
		}
	}

	// If this is the first farm being created, set it as the default farm
	if len(cfg.Engine.Farms) == 1 {
		cfg.Engine.DefaultFarm = farmName
	}

	err = cfg.Write()
	if err != nil {
		return err
	}

	fmt.Printf("Farm %q created\n", farmName)
	return nil
}
