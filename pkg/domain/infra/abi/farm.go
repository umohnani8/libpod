package abi

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/containers/buildah/pkg/parse"
	"github.com/containers/common/libimage"
	istorage "github.com/containers/image/v5/storage"
	"github.com/containers/podman/v4/pkg/domain/entities"
	"github.com/containers/podman/v4/pkg/emulation"
)

const (
	LocalFarmImageBuilderName   = "(local)"
	localFarmImageBuilderDriver = "local"
)

func (ir *ImageEngine) FarmName(ctx context.Context) string {
	return LocalFarmImageBuilderName
}

func (ir *ImageEngine) FarmDriver(ctx context.Context) string {
	return localFarmImageBuilderDriver
}

func (ir *ImageEngine) fetchInfo(ctx context.Context) (os, arch, variant string, nativePlatforms []string, emulatedPlatforms []string, err error) {
	nativePlatform := parse.DefaultPlatform()
	platform := strings.SplitN(nativePlatform, "/", 3)
	switch len(platform) {
	case 0, 1:
		return "", "", "", nil, nil, fmt.Errorf("unparseable default platform %q", nativePlatform)
	case 2:
		os, arch = platform[0], platform[1]
	case 3:
		os, arch, variant = platform[0], platform[1], platform[2]
	}
	os, arch, variant = libimage.NormalizePlatform(os, arch, variant)
	nativePlatform = os + "/" + arch
	if variant != "" {
		nativePlatform += ("/" + variant)
	}
	emulatedPlatforms = emulation.Registered()
	return os, arch, variant, append([]string{}, nativePlatform), emulatedPlatforms, nil
}

func (ir *ImageEngine) FarmInspect(ctx context.Context) (*entities.FarmInfo, error) {
	var (
		platforms          sync.Once
		platformsErr       error
		os, arch, variant  string
		nativeP, emulatedP []string
	)
	platforms.Do(func() {
		os, arch, variant, nativeP, emulatedP, platformsErr = ir.fetchInfo(ctx)
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
	destination := options.Destination

	// already present at destination?
	var br *entities.BoolReport
	if destination == nil {
		br, err = ir.Exists(ctx, options.ImageID)
	} else {
		br, err = destination.Exists(ctx, options.ImageID)
	}
	if err != nil {
		return "", err
	}
	if br.Value {
		return istorage.Transport.Name() + ":" + options.ImageID, nil
	}

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
		return "", fmt.Errorf("saving image %q: %w", options.ImageID, err)
	}

	loadOptions := entities.ImageLoadOptions{
		Input: tempFile.Name(),
	}
	if destination == nil {
		_, err = ir.Load(ctx, loadOptions)
	} else {
		_, err = destination.Load(ctx, loadOptions)
	}
	if err != nil {
		return "", err
	}

	return istorage.Transport.Name() + ":" + options.ImageID, nil
}
