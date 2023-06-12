package buildfarm

import (
	"errors"
	"fmt"

	"github.com/containers/common/pkg/completion"
	"github.com/containers/common/pkg/config"
	"github.com/containers/podman/v4/cmd/podman/common"
	"github.com/containers/podman/v4/cmd/podman/registry"
	"github.com/containers/podman/v4/pkg/domain/entities"
	"github.com/spf13/cobra"
)

var (
	buildfarmUpdateDescription = `Update an existing buildfarm`
	updateCommand              = &cobra.Command{
		Use:               "update [options] FARM",
		Short:             "Update an existing buildfarm",
		Long:              buildfarmUpdateDescription,
		RunE:              buildfarmUpdate,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: common.AutocompleteFarms,
		Example:           `podman buildfarm update farm1`,
	}
)

var (
	buildfarmUpdateOptions entities.BuildfarmUpdateOptions
)

func init() {
	registry.Commands = append(registry.Commands, registry.CliCommand{
		Command: updateCommand,
		Parent:  buildfarmCmd,
	})
	flags := updateCommand.Flags()

	addFlagName := "add"
	flags.StringSliceVar(&buildfarmUpdateOptions.Add, addFlagName, nil, "add system connection(s) to farm")
	_ = updateCommand.RegisterFlagCompletionFunc(addFlagName, completion.AutocompleteNone)
	removeFlagName := "remove"
	flags.StringSliceVar(&buildfarmUpdateOptions.Remove, removeFlagName, nil, "remove system connection(s) from farm")
	_ = updateCommand.RegisterFlagCompletionFunc(removeFlagName, completion.AutocompleteNone)
	defaultFlagName := "default"
	flags.BoolP(defaultFlagName, "", false, "set the given farm as the default farm")

}

func buildfarmUpdate(cmd *cobra.Command, args []string) error {
	farmName := args[0]

	def, err := cmd.Flags().GetBool("default")
	if err != nil {
		return err
	}

	if len(buildfarmUpdateOptions.Add) == 0 && len(buildfarmUpdateOptions.Remove) == 0 && !def {
		return fmt.Errorf("nothing to update for farm %q, please use the --add, --remove, or --default flags to update a farm", farmName)
	}

	cfg, err := config.ReadCustomConfig()
	if err != nil {
		return err
	}

	if cfg.Engine.Farms == nil {
		return errors.New("no farms are created at this time, there is nothing to update")
	}

	if val, ok := cfg.Engine.Farms[farmName]; ok {
		cMap := make(map[string]int)
		for _, c := range val {
			cMap[c] = 0
		}

		for _, cRemove := range buildfarmUpdateOptions.Remove {
			delete(cMap, cRemove)
		}

		for _, cAdd := range buildfarmUpdateOptions.Add {
			if _, ok := cMap[cAdd]; !ok {
				cMap[cAdd] = 0
			}
		}

		updatedConnections := []string{}
		for k := range cMap {
			updatedConnections = append(updatedConnections, k)
		}
		cfg.Engine.Farms[farmName] = updatedConnections
	}

	// Change the default to the given farm if --default=true
	if def {
		cfg.Engine.DefaultFarm = farmName
	}

	if err := cfg.Write(); err != nil {
		return err
	}
	fmt.Printf("Farm %q updated\n", farmName)
	return nil
}
