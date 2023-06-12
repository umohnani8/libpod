package entities

import (
	"io"

	"github.com/docker/distribution/reference"
)

type BuildfarmCreateOptions struct {
	Ignore bool
}

type BuildfarmRmOptions struct {
	All bool
}

type BuildfarmListOptions struct {
	Filter map[string][]string
}

type BuildfarmListReport struct {
}

type BuildfarmUpdateOptions struct {
	Add    []string
	Remove []string
}

type BuildfarmBuildOptions struct {
	OutputFormat                      string
	Out                               io.Writer
	Err                               io.Writer
	ForceRemoveIntermediateContainers bool
	RemoveIntermediateContainers      bool
	RemoveIntermediateImages          bool
	PruneImagesOnSuccess              bool
	Platforms                         []struct{ OS, Arch, Variant string }
	Pull                              bool
	IIDFile                           string
	ContextDirectory                  string
	Labels                            []string
	Args                              map[string]string
	ShmSize                           string
	Ulimit                            []string
	Memory                            int64
	MemorySwap                        int64
	CPUShares                         uint64
	CPUQuota                          int64
	CPUPeriod                         uint64
	CPUSetCPUs                        string
	CPUSetMems                        string
	CgroupParent                      string
	NoCache                           bool
	Quiet                             bool
	CacheFrom                         []reference.Named
	CacheTo                           []reference.Named
	AddHost                           []string
	Target                            string
	ConfigureNetwork                  *bool
}

type PullToFileOptions struct {
	ImageID    string
	SaveFormat string
	SaveFile   string
}

type PullToLocalOptions struct {
	ImageID     string
	SaveFormat  string
	Destination ImageEngine
}

type RemoveImageOptions struct {
	ImageID string
}

type PruneImageOptions struct {
	All bool
}

type PruneImageReport struct {
	ImageIDs   []string
	ImageNames []string
}

type ListBuilderOptions struct {
	ForceRemoveIntermediateContainers bool
	RemoveIntermediateContainers      bool
	RemoveIntermediateImages          bool
	PruneImagesOnSuccess              bool
	IIDFile                           string
}

type InfoOptions struct {
}

type Info struct {
	NativePlatforms   []string
	EmulatedPlatforms []string
}
