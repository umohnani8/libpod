package farm

import (
	"errors"
	"fmt"
	"os"

	"github.com/containers/common/pkg/auth"
	"github.com/containers/common/pkg/completion"
	"github.com/containers/common/pkg/config"
	"github.com/containers/podman/v4/cmd/podman/common"
	"github.com/containers/podman/v4/cmd/podman/registry"
	"github.com/containers/podman/v4/cmd/podman/utils"
	"github.com/containers/podman/v4/pkg/farm"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

type buildOptions struct {
	buildOptions common.BuildFlagsWrapper
	local        bool
	platforms    []string
	farm         string
}

var (
	farmBuildDescription = `Build images on farm nodes, then bundle them into a manifest list`
	buildCommand         = &cobra.Command{
		Use:     "build [options] [CONTEXT]",
		Short:   "Build a container image for multiple architectures",
		Long:    farmBuildDescription,
		RunE:    build,
		Example: "podman farm build [flags] buildContextDirectory",
		Args:    cobra.ExactArgs(1),
	}
	buildOpts = buildOptions{
		buildOptions: common.BuildFlagsWrapper{},
	}
)

func init() {
	registry.Commands = append(registry.Commands, registry.CliCommand{
		Command: buildCommand,
		Parent:  farmCmd,
	})
	flags := buildCommand.Flags()
	flags.SetNormalizeFunc(utils.AliasFlags)

	authfileFlagName := "authfile"
	flags.StringVar(&buildOpts.buildOptions.Authfile, authfileFlagName, auth.GetDefaultAuthFile(), "Path of the authentication file. Use REGISTRY_AUTH_FILE environment variable to override")
	_ = buildCommand.RegisterFlagCompletionFunc(authfileFlagName, completion.AutocompleteDefault)

	cleanupFlag := "cleanup"
	flags.BoolVar(&buildOpts.buildOptions.Cleanup, cleanupFlag, false, "Remove built images from farm nodes on success")

	podmanConfig := registry.PodmanConfig()
	farmFlagName := "farm"
	// If remote, don't read the client's containers.conf file
	defaultFarm := ""
	if !registry.IsRemote() {
		defaultFarm = podmanConfig.ContainersConfDefaultsRO.Farms.Default
	}
	flags.StringVar(&buildOpts.farm, farmFlagName, defaultFarm, "Farm to use for builds")

	localFlagName := "local"
	// Default for local is true and hide this flag for the remote use case
	if !registry.IsRemote() {
		flags.BoolVarP(&buildOpts.local, localFlagName, "l", true, "Build image on local machine as well as on farm nodes")
	}

	platformsFlag := "platforms"
	buildCommand.PersistentFlags().StringSliceVar(&buildOpts.platforms, platformsFlag, nil, "Build only on farm nodes that match the given platforms")

	common.DefineBuildFlags(buildCommand, &buildOpts.buildOptions, true)
}

func build(cmd *cobra.Command, args []string) error {
	// Return error if any of the hidden flags are used
	for _, f := range common.FarmBuildHiddenFlags {
		if cmd.Flags().Changed(f) {
			return fmt.Errorf("%q is an unsupported flag for podman farm build", f)
		}
	}

	if cmd.Flags().Changed("authfile") {
		if err := auth.CheckAuthFile(buildOpts.buildOptions.Authfile); err != nil {
			return err
		}
	}
	if !cmd.Flags().Changed("tag") {
		return errors.New("cannot create manifest list without a name, value for --tag is required")
	}
	opts, err := common.ParseBuildOpts(cmd, args, &buildOpts.buildOptions)
	if err != nil {
		return err
	}
	// Close the logFile if one was created based on the flag
	if opts.LogFileToClose != nil {
		defer opts.LogFileToClose.Close()
	}
	if opts.TmpDirToClose != "" {
		// We had to download the context directory.
		// Delete it later.
		defer func() {
			if err = os.RemoveAll(opts.TmpDirToClose); err != nil {
				logrus.Errorf("Removing temporary directory %q: %v", opts.TmpDirToClose, err)
			}
		}()
	}
	opts.Cleanup = buildOpts.buildOptions.Cleanup
	iidFile, err := cmd.Flags().GetString("iidfile")
	if err != nil {
		return err
	}
	opts.IIDFile = iidFile
	opts.Authfile = buildOpts.buildOptions.Authfile

	cfg, err := config.ReadCustomConfig()
	if err != nil {
		return err
	}

	defaultFarm := cfg.Farms.Default
	if cmd.Flags().Changed("farm") {
		f, err := cmd.Flags().GetString("farm")
		if err != nil {
			return err
		}
		defaultFarm = f
	}

	buildLocal := false
	localEngine := registry.ImageEngine()
	// var localEngine entities.ImageEngine
	if buildOpts.local {
		localEngine = registry.ImageEngine()
		buildLocal = true
	}

	ctx := registry.Context()
	farm, err := farm.NewFarm(ctx, defaultFarm, localEngine, buildLocal)
	if err != nil {
		return fmt.Errorf("initializing: %w", err)
	}

	schedule, err := farm.Schedule(ctx, buildOpts.platforms)
	if err != nil {
		return fmt.Errorf("scheduling builds: %w", err)
	}
	logrus.Infof("schedule: %v", schedule)

	manifestName := opts.Output
	// Set Output to "" so that the images built on the farm nodes have no name
	opts.Output = ""
	if err = farm.Build(ctx, schedule, *opts, manifestName, localEngine); err != nil {
		return fmt.Errorf("build: %w", err)
	}
	logrus.Infof("build: ok")

	return nil
}
