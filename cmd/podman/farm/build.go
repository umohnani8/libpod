package farm

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/containers/buildah/define"
	"github.com/containers/buildah/pkg/util"
	"github.com/containers/common/pkg/config"
	"github.com/containers/podman/v4/cmd/podman/registry"
	"github.com/containers/podman/v4/cmd/podman/utils"
	"github.com/containers/podman/v4/pkg/domain/entities"
	"github.com/containers/podman/v4/pkg/farm"
	"github.com/containers/storage"
	"github.com/docker/distribution/reference"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

type buildOptions struct {
	output       string
	dockerfiles  []string
	buildOptions entities.BuildOptions
	platforms    []string
	local        bool
	cli          struct {
		BuildArg  []string
		CacheFrom []string
		CacheTo   []string
		Isolation string
		Network   string
	}
}

var (
	farmBuildDescription = `Builds a container image using podman system connections, then bundles them into a manifest list.`
	buildCommand         = &cobra.Command{
		Use:     "build",
		Short:   "Build a container image for multiple architectures",
		Long:    farmBuildDescription,
		RunE:    build,
		Example: "podman farm build [flags] buildContextDirectory",
		Args:    cobra.ExactArgs(1),
	}
	buildOpts = buildOptions{
		output:       "localhost/test",
		buildOptions: entities.BuildOptions{},
	}
)

func init() {
	registry.Commands = append(registry.Commands, registry.CliCommand{
		Command: buildCommand,
		Parent:  farmCmd,
	})
	flags := buildCommand.Flags()
	flags.SetNormalizeFunc(utils.AliasFlags)

	localFlagName := "local"
	// Default for local is true and hide this flag for the remote use case
	if !registry.IsRemote() {
		flags.BoolVarP(&buildOpts.local, localFlagName, "l", true, "Build image on local machine including on farm nodes")
	}

	// We intentionally avoid reusing parsing logic in buildah to not
	// advertise options which aren't supported for remote builds.
	buildCommand.PersistentFlags().StringVarP(&buildOpts.output, "tag", "t", "", "output location")
	buildCommand.PersistentFlags().StringSliceVarP(&buildOpts.dockerfiles, "file", "f", nil, "dockerfile")
	buildCommand.PersistentFlags().StringSliceVar(&buildOpts.buildOptions.Labels, "label", nil, "set `label=value` in output images")
	buildCommand.PersistentFlags().BoolVar(&buildOpts.buildOptions.RemoveIntermediateCtrs, "rm", true, "remove intermediate containers on success")
	buildCommand.PersistentFlags().BoolVar(&buildOpts.buildOptions.ForceRmIntermediateCtrs, "force-rm", true, "remove intermediate containers, even if the build fails")
	buildCommand.PersistentFlags().BoolVar(&buildOpts.buildOptions.RemoveIntermediateImages, "rm-images", false, "remove images from builders on success")
	buildCommand.PersistentFlags().BoolVar(&buildOpts.buildOptions.PruneImagesOnSuccess, "prune", false, "prune images from builders on success")
	buildCommand.PersistentFlags().StringSliceVar(&buildOpts.buildOptions.AddHost, "add-host", nil, "add custom host-to-IP mappings")
	buildCommand.PersistentFlags().StringSliceVar(&buildOpts.cli.BuildArg, "build-arg", nil, "build-time variable")
	buildCommand.PersistentFlags().StringSliceVar(&buildOpts.cli.CacheFrom, "cache-from", nil, "cache source repositories")
	buildCommand.PersistentFlags().Uint64Var(&buildOpts.buildOptions.CPUPeriod, "cpu-period", 0, "CPU CFS period")
	buildCommand.PersistentFlags().Int64Var(&buildOpts.buildOptions.CPUQuota, "cpu-quota", 0, "CPU CFS quota")
	buildCommand.PersistentFlags().Uint64VarP(&buildOpts.buildOptions.CPUShares, "cpu-shares", "c", 0, "CPU CFS quota")
	buildCommand.PersistentFlags().StringVar(&buildOpts.buildOptions.CPUSetCPUs, "cpuset-cpus", "", "CPUs to allow execution")
	buildCommand.PersistentFlags().StringVar(&buildOpts.buildOptions.CPUSetMems, "cpuset-mems", "", "MEMs to allow execution")
	buildCommand.PersistentFlags().StringVar(&buildOpts.buildOptions.IIDFile, "iidfile", "", "write manifest list ID to file")
	buildCommand.PersistentFlags().StringVar(&buildOpts.cli.Isolation, "isolation", "", "RUN isolation")
	buildCommand.PersistentFlags().Int64Var(&buildOpts.buildOptions.Memory, "memory", 0, "memory limit")
	buildCommand.PersistentFlags().Int64Var(&buildOpts.buildOptions.MemorySwap, "memory-swap", 0, "memory+swap limit")
	buildCommand.PersistentFlags().StringVar(&buildOpts.cli.Network, "network", "", "RUN network")
	buildCommand.PersistentFlags().BoolVar(&buildOpts.buildOptions.NoCache, "no-cache", false, "disable build cache")
	buildCommand.PersistentFlags().BoolVar(&buildOpts.buildOptions.Pull, "pull", false, "always try to pull newer versions of images")
	buildCommand.PersistentFlags().StringSliceVar(&buildOpts.platforms, "platform", nil, "target platforms")
	buildCommand.PersistentFlags().BoolVar(&buildOpts.buildOptions.Quiet, "quiet", false, "suppress build output")
	buildCommand.PersistentFlags().StringVar(&buildOpts.buildOptions.ShmSize, "shm-size", "", "size of /dev/shm")
	buildCommand.PersistentFlags().StringVar(&buildOpts.buildOptions.Target, "target", "", "final stage to build")
	buildCommand.PersistentFlags().StringSliceVar(&buildOpts.buildOptions.Ulimit, "ulimit", nil, "resource limits")
}

func build(cmd *cobra.Command, args []string) error {
	if buildOpts.output == "" {
		return fmt.Errorf(`no output specified: expected -t "dir:/directory/path" or -t "registry.tld/repository/name:tag"`)
	}
	buildOpts.buildOptions.ContextDirectory = args[0]
	if buildOpts.buildOptions.ContextDirectory == "" {
		return fmt.Errorf("expected location of build context")
	}
	if !filepath.IsAbs(buildOpts.buildOptions.ContextDirectory) {
		contextDir, err := filepath.Abs(buildOpts.buildOptions.ContextDirectory)
		if err != nil {
			return err
		}
		buildOpts.buildOptions.ContextDirectory = contextDir
	}
	for _, arg := range buildOpts.cli.BuildArg {
		if buildOpts.buildOptions.Args == nil {
			buildOpts.buildOptions.Args = make(map[string]string)
		}
		kv := strings.SplitN(arg, "=", 2)
		if len(kv) > 1 {
			buildOpts.buildOptions.Args[kv[0]] = kv[1]
		} else {
			delete(buildOpts.buildOptions.Args, kv[0])
		}
	}
	for _, cacheFrom := range buildOpts.cli.CacheFrom {
		name, err := reference.ParseNamed(cacheFrom)
		if err != nil {
			return fmt.Errorf("parsing reference %q: %w", cacheFrom, err)
		}
		buildOpts.buildOptions.CacheFrom = append(buildOpts.buildOptions.CacheFrom, name)
	}
	for _, cacheTo := range buildOpts.cli.CacheTo {
		name, err := reference.ParseNamed(cacheTo)
		if err != nil {
			return fmt.Errorf("parsing reference %q: %w", cacheTo, err)
		}
		buildOpts.buildOptions.CacheTo = append(buildOpts.buildOptions.CacheTo, name)
	}
	switch buildOpts.cli.Network {
	case "", "default":
		// nothing
	case "none":
		buildOpts.buildOptions.ConfigureNetwork = define.NetworkDisabled
	}
	if len(buildOpts.dockerfiles) == 0 {
		dockerfile, err := util.DiscoverContainerfile(buildOpts.buildOptions.ContextDirectory)
		if err != nil {
			return err
		}
		absDockerfile := dockerfile
		if !filepath.IsAbs(dockerfile) {
			absDockerfile, err = filepath.Abs(dockerfile)
			if err != nil {
				return err
			}
		}
		buildOpts.dockerfiles = append(buildOpts.dockerfiles, absDockerfile)
	}
	buildOpts.buildOptions.FarmBuild = true

	cfg, err := config.ReadCustomConfig()
	if err != nil {
		return err
	}

	defaultFarm := cfg.Farms.Default
	if farmCmd.Flags().Changed("farm") {
		f, err := farmCmd.Flags().GetString("farm")
		if err != nil {
			return err
		}
		defaultFarm = f
	}

	var storeOptions *storage.StoreOptions
	if buildOpts.local {
		opts, err := storage.DefaultStoreOptionsAutoDetectUID()
		if err != nil {
			return err
		}
		storeOptions = &opts
	}

	ctx := context.TODO()
	farm, err := farm.NewFarm(ctx, defaultFarm, storeOptions, buildOpts.local)
	if err != nil {
		return fmt.Errorf("initializing: %w", err)
	}

	schedule, err := farm.Schedule(ctx, buildOpts.platforms)
	if err != nil {
		return fmt.Errorf("scheduling builds: %w", err)
	}
	logrus.Infof("schedule: %v", schedule)

	if err = farm.Build(ctx, buildOpts.output, schedule, buildOpts.dockerfiles, buildOpts.buildOptions); err != nil {
		return fmt.Errorf("build: %w", err)
	}
	logrus.Infof("build: ok")
	return nil
}
