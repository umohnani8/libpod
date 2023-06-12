package libpod

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/containers/common/pkg/config"
	"github.com/containers/podman/v4/pkg/domain/entities"
	"github.com/containers/podman/v4/pkg/util"
	"github.com/containers/storage"
	"github.com/hashicorp/go-multierror"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
)

type Farm struct {
	name      string
	storeOpts *storage.StoreOptions
	builders  map[string]entities.ImageEngine
}

func (f *Farm) Name() string {
	return f.name
}

func (f *Farm) Builders() map[string]entities.ImageEngine {
	return f.builders
}

var allBuilders = []string{}

// NewDefaultFarm returns a Farm that uses all known system connections and
// which has no name.  If storeOptions is not nil, the local system will be
// included as an unnamed connection.
func newFarmWithBuilders(ctx context.Context, builders *[]string, storeOptions *storage.StoreOptions, flags *pflag.FlagSet) (*Farm, error) {
	wantBuilder := func(name string) bool {
		if builders == &allBuilders {
			return true
		}
		return util.StringInSlice(name, *builders)
	}
	logrus.Info("initializing farm")
	custom, err := config.ReadCustomConfig()
	if err != nil {
		return nil, fmt.Errorf("reading custom config: %w", err)
	}
	farm := &Farm{
		builders: make(map[string]entities.ImageEngine),
	}
	if flags == nil {
		flags = pflag.NewFlagSet("buildfarm", pflag.ExitOnError)
	}
	farm.flagSet = flags
	farm.storeOpts = storeOptions
	var builderMutex sync.Mutex
	var builderGroup multierror.Group
	for dest := range custom.Engine.ServiceDestinations {
		if !wantBuilder(dest) {
			continue
		}
		dest := dest
		builderGroup.Go(func() error {
			logrus.Infof("connecting to %q", dest)
			ib, err := NewPodmanRemoteImageBuilder(ctx, flags, dest)
			if err != nil {
				return err
			}
			defer logrus.Infof("builder %q ready", dest)
			builderMutex.Lock()
			defer builderMutex.Unlock()
			farm.builders[dest] = ib
			return nil
		})
	}
	if farm.storeOpts != nil && wantBuilder(LocalImageBuilderName) { // make a shallow copy - could/should be a deep copy?
		builderGroup.Go(func() error {
			logrus.Infof("setting up local builder")
			ib, err := NewPodmanLocalImageBuilder(ctx, flags, farm.storeOpts)
			if err != nil {
				return err
			}
			defer logrus.Infof("local builder ready")
			builderMutex.Lock()
			defer builderMutex.Unlock()
			farm.builders[""] = ib
			return nil
		})
	}
	if builderError := builderGroup.Wait(); builderError != nil {
		if err := builderError.ErrorOrNil(); err != nil {
			return nil, err
		}
	}
	if len(farm.builders) > 0 {
		defer logrus.Info("farm ready")
		return farm, nil
	}
	return nil, errors.New("no builders configured")
}

// NewDefaultFarm returns a Farm that uses all known system connections and
// which has no name.  If storeOptions is not nil, the local system will be
// included as an unnamed connection.
func NewDefaultFarm(ctx context.Context, storeOptions *storage.StoreOptions, flags *pflag.FlagSet) (*Farm, error) {
	return newFarmWithBuilders(ctx, &allBuilders, storeOptions, flags)
}

// NewFarm returns a Farm that has a preconfigured set of system connections.
func NewFarm(ctx context.Context, name string, storeOptions *storage.StoreOptions, flags *pflag.FlagSet) (*Farm, error) {
	if name == "" {
		return NewDefaultFarm(ctx, storeOptions, flags)
	}
	return nil, errors.New("not implemented")
}

// NewAdHocFarm returns a Farm that uses the specified system connections and
// which has no name.  If storeOptions is not nil AND the list of destinations
// includes the empty string, the local system will be included as an unnamed
// connection.
func NewAdHocFarm(ctx context.Context, destinations []string, storeOptions *storage.StoreOptions, flags *pflag.FlagSet) (*Farm, error) {
	return newFarmWithBuilders(ctx, &destinations, storeOptions, flags)
}
