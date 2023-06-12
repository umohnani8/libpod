package buildfarm

import (
	"fmt"
	"os"
	"sort"

	"github.com/containers/common/pkg/completion"
	"github.com/containers/common/pkg/config"
	"github.com/containers/common/pkg/report"
	"github.com/containers/podman/v4/cmd/podman/common"
	"github.com/containers/podman/v4/cmd/podman/registry"
	"github.com/containers/podman/v4/cmd/podman/validate"
	"github.com/containers/podman/v4/pkg/domain/entities"
	"github.com/spf13/cobra"
)

var (
	buildfarmLsDescription = `
podman buildfarm ls

List all available farms. The output of the farms can be filtered
and the output format can be changed to JSON or a user specified Go template.`
	lsCommand = &cobra.Command{
		Use:               "ls [options]",
		Aliases:           []string{"list"},
		Args:              validate.NoArgs,
		Short:             "List buildfarms",
		Long:              buildfarmLsDescription,
		RunE:              list,
		ValidArgsFunction: completion.AutocompleteNone,
	}
)

var (
	// Temporary struct to hold cli values.
	cliOpts = struct {
		Filter []string
		Format string
		Quiet  bool
	}{}
	lsOpts = entities.BuildfarmListOptions{}
)

func init() {
	registry.Commands = append(registry.Commands, registry.CliCommand{
		Command: lsCommand,
		Parent:  buildfarmCmd,
	})
	flags := lsCommand.Flags()

	filterFlagName := "filter"
	flags.StringArrayVarP(&cliOpts.Filter, filterFlagName, "f", []string{}, "Filter buildfarm output")
	_ = lsCommand.RegisterFlagCompletionFunc(filterFlagName, common.AutocompleteBuildfarmFilters)

	formatFlagName := "format"
	flags.StringVar(&cliOpts.Format, formatFlagName, "", "Format buildfarm output using Go template")
	_ = lsCommand.RegisterFlagCompletionFunc(formatFlagName, common.AutocompleteFormat(&entities.BuildfarmListReport{}))

	flags.BoolP("noheading", "n", false, "Do not print headers")
	flags.BoolVarP(&cliOpts.Quiet, "quiet", "q", false, "Print buildfarm output in quiet mode")
}

type farmOut struct {
	Name        string
	Connections []string
	Default     bool
}

func list(cmd *cobra.Command, args []string) error {
	cfg, err := config.ReadCustomConfig()
	if err != nil {
		return err
	}

	format := cmd.Flag("format").Value.String()
	if format == "" && len(args) > 0 {
		format = "json"
	}

	quiet, err := cmd.Flags().GetBool("quiet")
	if err != nil {
		return err
	}

	noHeading, err := cmd.Flags().GetBool("noheading")
	if err != nil {
		return err
	}

	rows := make([]farmOut, 0)
	for k, v := range cfg.Engine.Farms {
		if quiet {
			fmt.Println(k)
			continue
		}

		defaultFarm := false
		if k == cfg.Engine.DefaultFarm {
			defaultFarm = true
		}

		r := farmOut{
			Name:        k,
			Connections: v,
			Default:     defaultFarm,
		}
		rows = append(rows, r)
	}

	if quiet {
		return nil
	}

	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Name < rows[j].Name
	})

	rpt := report.New(os.Stdout, cmd.Name())
	defer rpt.Flush()

	if report.IsJSON(format) {
		buf, err := registry.JSONLibrary().MarshalIndent(rows, "", "    ")
		if err == nil {
			fmt.Println(string(buf))
		}
		return err
	}

	if format != "" {
		rpt, err = rpt.Parse(report.OriginUser, format)
	} else {
		rpt, err = rpt.Parse(report.OriginPodman,
			"{{range .}}{{.Name}}\t{{.Connections}}\t{{.Default}}\n{{end -}}")
	}
	if err != nil {
		return err
	}

	if rpt.RenderHeaders && !noHeading {
		err = rpt.Execute([]map[string]string{{
			"Default":     "Default",
			"Connections": "Connections",
			"Name":        "Name",
		}})
		if err != nil {
			return err
		}
	}

	return rpt.Execute(rows)

}
