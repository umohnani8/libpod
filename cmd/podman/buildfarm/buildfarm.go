package buildfarm

import (
	"github.com/containers/podman/v4/cmd/podman/registry"
	"github.com/containers/podman/v4/cmd/podman/validate"
	"github.com/containers/podman/v4/pkg/util"
	"github.com/spf13/cobra"
)

var (
	// Pull in configured json library
	json = registry.JSONLibrary()

	// Command: podman _pod_
	buildfarmCmd = &cobra.Command{
		Use:   "buildfarm",
		Short: "Farm out builds to remote machines",
		Long:  "Farm out builds to remote machines",
		RunE:  validate.SubCommandExists,
	}
	containerConfig = util.DefaultContainerConfig()
)

func init() {
	registry.Commands = append(registry.Commands, registry.CliCommand{
		Command: buildfarmCmd,
	})
}
