package tunnel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	istorage "github.com/containers/image/v5/storage"
	"github.com/containers/podman/v4/pkg/bindings/system"
	"github.com/containers/podman/v4/pkg/domain/entities"
)

const (
	remoteFarmImageBuilderDriver = "podman-remote"
)

func (ir *ImageEngine) FarmName(ctx context.Context) string {
	return ir.Farm
}

func (ir *ImageEngine) FarmDriver(ctx context.Context) string {
	return remoteFarmImageBuilderDriver
}

func (ir *ImageEngine) fetchInfo(ctx context.Context) (os, arch string, nativePlatforms []string, err error) {
	engineInfo, err := system.Info(ir.ClientCtx, &system.InfoOptions{})
	if err != nil {
		return "", "", nil, fmt.Errorf("retrieving host info from %q: %w", ir.Farm, err)
	}
	os = engineInfo.Host.OS
	arch = engineInfo.Host.Arch
	nativePlatform := os + "/" + arch // TODO: pester someone about returning variant info
	return os, arch, []string{nativePlatform}, nil
}

func (ir *ImageEngine) FarmInspect(ctx context.Context) (*entities.FarmInfo, error) {
	var (
		platforms          sync.Once
		platformsErr       error
		os, arch, variant  string
		nativeP, emulatedP []string
	)
	platforms.Do(func() {
		os, arch, nativeP, platformsErr = ir.fetchInfo(ctx)
	})
	return &entities.FarmInfo{NativePlatforms: nativeP, EmulatedPlatforms: emulatedP, OS: os, Arch: arch, Variant: variant}, platformsErr
}

func (ir *ImageEngine) FarmStatus(ctx context.Context) error {
	_, err := ir.Config(ctx)
	return err
}

// PullToFile pulls the image from the remote engine and saves it to a file,
// returning a string-format reference which can be parsed by containers/image.
func (ir *ImageEngine) PullToFile(ctx context.Context, options entities.PullToFileOptions) (reference string, err error) {
	saveOptions := entities.ImageSaveOptions{
		Format: options.SaveFormat,
		Output: options.SaveFile,
	}
	if err := ir.Save(ctx, options.ImageID, nil, saveOptions); err != nil {
		return "", fmt.Errorf("saving image %q: %w", options.ImageID, err)
	}
	return options.SaveFormat + ":" + options.SaveFile, nil
}

func (ir *ImageEngine) PullToLocal(ctx context.Context, options entities.PullToLocalOptions) (reference string, err error) {
	tempFile, err := os.CreateTemp("", "")
	if err != nil {
		return "", err
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()
	saveOptions := entities.ImageSaveOptions{
		Format: options.SaveFormat,
		Output: tempFile.Name(),
	}
	if err := ir.Save(ctx, options.ImageID, nil, saveOptions); err != nil {
		return "", fmt.Errorf("saving image %q to temporary file: %w", options.ImageID, err)
	}
	loadOptions := entities.ImageLoadOptions{
		Input: tempFile.Name(),
	}
	if options.Destination == nil {
		return "", errors.New("internal error: options.Destination not set")
	} else {
		if _, err = options.Destination.Load(ctx, loadOptions); err != nil {
			return "", fmt.Errorf("loading image %q: %w", options.ImageID, err)
		}
	}
	name := fmt.Sprintf("%s:%s", istorage.Transport.Name(), options.ImageID)
	return name, err
}
