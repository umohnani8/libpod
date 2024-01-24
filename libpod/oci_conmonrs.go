//go:build linux || freebsd
// +build linux freebsd

package libpod

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containers/common/pkg/config"
	"github.com/containers/common/pkg/resize"
	cversion "github.com/containers/common/pkg/version"
	"github.com/containers/conmon-rs/pkg/client"
	"github.com/containers/podman/v5/libpod/define"
	"github.com/containers/podman/v5/pkg/errorhandling"
	"github.com/containers/podman/v5/pkg/rootless"
	"github.com/containers/podman/v5/pkg/specgenutil"
	"github.com/containers/podman/v5/pkg/util"
	"github.com/containers/podman/v5/utils"
	"github.com/docker/docker/pkg/homedir"
	"github.com/moby/term"
	spec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// Note: To get started with testing this, replace the conmon binary used in your podman
// run command with conmonrs.
// e.g podman --conmon /usr/bin/conmonrs run -d alpine sleep 100
// then run "pidof conmonrs" to get the pids of conmonrs
// when you do "pstree [pidofconmonrs]" you will see the process under it
// Multiple container processes can be added to one conmonrs process, work needs to be done
// in conmonrs so that podman can retrieve how many container processes a conmonrs process
// currently has. The limit of how many containers can be a part of one conmonrs is not known yet.
// Link to issue in conmonrs to expose this: https://github.com/containers/conmon-rs/issues/2020
// With the code currently, containers are able to come up and run but work still needs to be done
// around logging and other complex functionalities.
// There are also some differences in the flags that conmon uses vs the options that conmonrs has.
// Some options seem to have been dropped, so need to look into those to see whether they were importnant
// or replaced by some other less known option in comonrs

type ConmonRSOCIRuntime struct {
	name              string
	path              string
	conmonPath        string
	conmonEnv         []string
	tmpDir            string
	exitsDir          string
	logSizeMax        int64
	noPivot           bool
	reservePorts      bool
	runtimeFlags      []string
	supportsJSON      bool
	supportsKVM       bool
	supportsNoCgroups bool
	enableKeyring     bool

	cachedClient *client.ConmonClient
}

func newConmonRSOCIRuntime(name string, paths []string, conmonPath string, runtimeFlags []string, runtimeCfg *config.Config, rt *Runtime) (OCIRuntime, error) {
	if name == "" {
		return nil, fmt.Errorf("the OCI runtime must be provided a non-empty name: %w", define.ErrInvalidArg)
	}

	// Make lookup tables for runtime support
	supportsJSON := make(map[string]bool, len(runtimeCfg.Engine.RuntimeSupportsJSON.Get()))
	supportsNoCgroups := make(map[string]bool, len(runtimeCfg.Engine.RuntimeSupportsNoCgroups.Get()))
	supportsKVM := make(map[string]bool, len(runtimeCfg.Engine.RuntimeSupportsKVM.Get()))
	for _, r := range runtimeCfg.Engine.RuntimeSupportsJSON.Get() {
		supportsJSON[r] = true
	}
	for _, r := range runtimeCfg.Engine.RuntimeSupportsNoCgroups.Get() {
		supportsNoCgroups[r] = true
	}
	for _, r := range runtimeCfg.Engine.RuntimeSupportsKVM.Get() {
		supportsKVM[r] = true
	}

	runtime := new(ConmonRSOCIRuntime)
	runtime.name = name
	runtime.conmonPath = conmonPath
	runtime.runtimeFlags = runtimeFlags

	runtime.conmonEnv = runtimeCfg.Engine.ConmonEnvVars.Get()
	runtime.tmpDir = runtimeCfg.Engine.TmpDir
	runtime.logSizeMax = runtimeCfg.Containers.LogSizeMax
	runtime.noPivot = runtimeCfg.Engine.NoPivotRoot
	runtime.reservePorts = runtimeCfg.Engine.EnablePortReservation
	runtime.enableKeyring = runtimeCfg.Containers.EnableKeyring

	// TODO: probe OCI runtime for feature and enable automatically if
	// available.
	base := filepath.Base(name)
	runtime.supportsJSON = supportsJSON[base]
	runtime.supportsNoCgroups = supportsNoCgroups[base]
	runtime.supportsKVM = supportsKVM[base]

	foundPath := false
	for _, path := range paths {
		stat, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("cannot stat OCI runtime %s path: %w", name, err)
		}
		if !stat.Mode().IsRegular() {
			continue
		}
		foundPath = true
		logrus.Tracef("found runtime %q", path)
		runtime.path = path
		break
	}

	// Search the $PATH as last fallback
	if !foundPath {
		if foundRuntime, err := exec.LookPath(name); err == nil {
			foundPath = true
			runtime.path = foundRuntime
			logrus.Debugf("using runtime %q from $PATH: %q", name, foundRuntime)
		}
	}

	if !foundPath {
		return nil, fmt.Errorf("no valid executable found for OCI runtime %s: %w", name, define.ErrInvalidArg)
	}

	runtime.exitsDir = filepath.Join(runtime.tmpDir, "exits")

	// Create the exit files and attach sockets directories
	if err := os.MkdirAll(runtime.exitsDir, 0750); err != nil {
		return nil, fmt.Errorf("creating OCI runtime exit files directory: %w", err)
	}

	runRoot := rt.RunRoot()
	fmt.Println("----runRoot----:", runRoot)

	fmt.Println("===========in here================")
	return runtime, nil
}

func (r *ConmonRSOCIRuntime) cgroupManager(ctr *Container) (client.CgroupManager, error) {
	switch ctr.CgroupManager() {
	case config.SystemdCgroupsManager:
		return client.CgroupManagerSystemd, nil
	case config.CgroupfsCgroupsManager:
		return client.CgroupManagerCgroupfs, nil
	default:
		return client.CgroupManagerSystemd, fmt.Errorf("unsupported conmon-rs cgroup manager: %q", ctr.CgroupManager())
	}
}

func (r *ConmonRSOCIRuntime) client(numCtrs int) (*client.ConmonClient, error) {
	// TODO
	// get list of clients and check how many ctrs are attached to each conmon-rs
	// if any conmon-rs is less then numCtrs then get that client and add new ctr to it
	if numCtrs != 1 {
		return nil, errors.New("only 1:1 ctr to conmon-rs is supported right now")
	}

	if r.cachedClient != nil {
		fmt.Println("---cached client---:", r.cachedClient)
		return r.cachedClient, nil
	}
	conmonrsDir := filepath.Join(r.tmpDir, "conmon-rs")
	if err := os.MkdirAll(conmonrsDir, 0750); err != nil {
		return nil, err
	}
	// For the case of 1:1, always create a new client
	serverRunDir, err := os.MkdirTemp(conmonrsDir, "rs")
	if err != nil {
		fmt.Println("--error creating temp dir----")
		return nil, err
	}
	serverConfig := client.NewConmonServerConfig(r.path, "", serverRunDir)
	// TODO: change this to a file
	// Note: to see logs in stdout for debugging, change this to LogDriverStdout
	serverConfig.LogDriver = client.LogDriverSystemd
	serverConfig.CgroupManager = client.CgroupManagerPerCommand

	client, err := client.New(serverConfig)
	if err != nil {
		fmt.Println("----found new client creation error---")
		return nil, err
	}
	r.cachedClient = client
	fmt.Println("---newly created client----:", client)
	return client, nil
}

// Name returns the name of the runtime.
func (r *ConmonRSOCIRuntime) Name() string {
	return fmt.Sprintf("conmon-rs:%s", r.name)
}

// Path returns the path to the runtime executable.
func (r *ConmonRSOCIRuntime) Path() string {
	return r.path
}

// CreateContainer creates the container in the OCI runtime.
// The returned int64 contains the microseconds needed to restore
// the given container if it is a restore and if restoreOptions.PrintStats
// is true. In all other cases the returned int64 is 0.
func (r *ConmonRSOCIRuntime) CreateContainer(ctr *Container, restoreOptions *ContainerCheckpointOptions) (int64, error) {
	fmt.Println("====IN CONMONRS CREATE======")
	if !hasCurrentUserMapped(ctr) {
		// if we are running a non privileged container, be sure to umount some kernel paths so they are not
		// bind mounted inside the container at all.
		if !ctr.config.Privileged && !rootless.IsRootless() {
			hideFiles := !ctr.config.Privileged && !rootless.IsRootless()
			fmt.Println("--hidefiles--:", hideFiles)
			return 0, r.printError("createRootlessContainer")
			// return r.createRootlessContainer(ctr, restoreOptions, hideFiles)
		}
	}

	return r.createOCIContainer(ctr, restoreOptions)
}

func (r *ConmonRSOCIRuntime) createOCIContainer(ctr *Container, restoreOptions *ContainerCheckpointOptions) (int64, error) {
	var err error

	runtimeDir, err := homedir.GetRuntimeDir()
	if err != nil {
		return 0, err
	}

	var ociLogPath string
	if logrus.GetLevel() != logrus.DebugLevel && r.supportsJSON {
		ociLogPath = filepath.Join(ctr.state.RunDir, "oci-log")
	}

	if logTag := ctr.LogTag(); logTag != "" {
		return 0, fmt.Errorf("log tag specified %q, but not supported with conmon-rs", logTag)
	}

	fmt.Println("---CgroupsMode---:", ctr.config.CgroupsMode)
	// TODO: talk to Dan about cgroups mode
	switch ctr.config.CgroupsMode {
	case cgroupSplit:
		// return 0, fmt.Errorf("cgroups mode %q not supported with conmon-rs", ctr.config.CgroupsMode)
		moveToRuntimeCgroup()
	case "enabled":
		logrus.Warnf("cgroups mode %q used with conmon-rs, handled as %q", ctr.config.CgroupsMode, "no-conmon")
	}

	// ignore pid file config option if it matches the default location used by conmon-rs
	if ctr.config.PidFile != "" && ctr.config.PidFile != filepath.Join(ctr.state.RunDir, "pidfile") {
		return 0, fmt.Errorf("pid file specified %q, but not supported with conmon-rs", ctr.config.PidFile)
	}

	config := new(client.CreateContainerConfig)
	config.ID = ctr.ID()
	config.BundlePath = ctr.bundlePath()
	config.Terminal = ctr.Terminal()
	config.Stdin = ctr.Stdin()
	config.ExitPaths = []string{filepath.Join(r.exitsDir, ctr.ID())}
	config.OOMExitPaths = []string{filepath.Join(r.exitsDir, "oom-"+ctr.ID())}

	maxSize := r.logSizeMax
	fmt.Println("----log size max 1---:", maxSize)
	if ctr.config.LogSize > 0 {
		maxSize = ctr.config.LogSize
	}
	fmt.Println("----log size max 2---:", maxSize)

	logDriver := define.KubernetesLogging
	fmt.Println("---log driver---:", ctr.LogDriver())
	fmt.Println("----log path----:", ctr.LogPath())
	// switch ctr.LogDriver() {
	// TODO: fix to use ctr.LogDriver() instead
	switch logDriver {
	case define.JournaldLogging:
		// TODO: Add support
		return 0, fmt.Errorf("%s log driver not supported with conmon-rs", define.JournaldLogging)
	case define.NoLogging:
		config.LogDrivers = []client.ContainerLogDriver{}
	case define.PassthroughLogging:
		// TODO: Add support
		return 0, fmt.Errorf("%s log driver not supported with conmon-rs", define.PassthroughLogging)
	default: //nolint:gocritic
		// No case here should happen except JSONLogging, but keep this here in case the options are extended
		logrus.Errorf("%s logging specified but not supported. Choosing k8s-file logging instead", ctr.LogDriver())
		fallthrough
	case "":
		// to get here, either a user would specify `--log-driver ""`, or this came from another place in libpod
		// since the former case is obscure, and the latter case isn't an error, let's silently fallthrough
		fallthrough
	case define.JSONLogging:
		fallthrough
	case define.KubernetesLogging:
		config.LogDrivers = []client.ContainerLogDriver{{
			Type: client.LogDriverTypeContainerRuntimeInterface,
			// TODO: fix this log path
			// Path:    ctr.LogPath(),
			Path:    "/tmp/conmon-rs-logs",
			MaxSize: uint64(maxSize),
		}}
	}

	config.GlobalArgs = append(config.GlobalArgs, r.runtimeFlags...)

	if ociLogPath != "" {
		config.GlobalArgs = append(config.GlobalArgs, "--log-format=json", "--log", ociLogPath)
	}

	if ctr.config.NoCgroups {
		fmt.Println("---running with no cgroups----")
		logrus.Debugf("Running with no Cgroups")
		config.GlobalArgs = append(config.GlobalArgs, "--cgroup-manager", "disabled")
	}

	if ctr.config.SdNotifyMode == define.SdNotifyModeContainer && ctr.config.SdNotifySocket != "" {
		// TODO: Add support
		return 0, fmt.Errorf("sd-notify mode container not supported with conmon-rs")
	}

	ctx := context.Background()
	if ctr.config.Timeout > 0 {
		var cancel func()
		ctx, cancel = context.WithTimeout(ctx, time.Duration(ctr.config.Timeout)*time.Second)
		defer cancel()
	}

	if !r.enableKeyring {
		config.GlobalArgs = append(config.GlobalArgs, "--no-new-keyring")
	}

	// ignore default path, there is no conmon running per container
	if ctr.config.ConmonPidFile != "" && ctr.config.ConmonPidFile != filepath.Join(ctr.state.RunDir, "conmon.pid") {
		return 0, fmt.Errorf("conmon pid file specified %q, but not supported with conmon-rs", ctr.config.ConmonPidFile)
	}

	if r.noPivot {
		config.GlobalArgs = append(config.GlobalArgs, "--no-pivot")
	}
	fmt.Println("-----global args----:", config.GlobalArgs)

	config.CleanupCmd, err = specgenutil.CreateExitCommandArgs(ctr.runtime.storageConfig, ctr.runtime.config, logrus.IsLevelEnabled(logrus.DebugLevel), ctr.AutoRemove(), false)
	if err != nil {
		return 0, err
	}
	config.CleanupCmd = append(config.CleanupCmd, ctr.ID())

	preserveFDs := ctr.config.PreserveFDs
	if val := os.Getenv("LISTEN_FDS"); val != "" {
		if ctr.config.PreserveFDs > 0 {
			logrus.Warnf("Ignoring LISTEN_FDS to preserve custom user-specified FDs")
		} else {
			fds, err := strconv.Atoi(val)
			if err != nil {
				return 0, fmt.Errorf("converting LISTEN_FDS=%s: %w", val, err)
			}
			preserveFDs = uint(fds)
		}
	}

	if preserveFDs > 0 {
		config.CommandArgs = append(config.CommandArgs, "--preserve-fds", fmt.Sprintf("%d", preserveFDs))
	}
	fmt.Println("-----command args-----:", config.CommandArgs)

	if restoreOptions != nil {
		return 0, r.printError("create: restoreOptions")
	}

	var filesToClose []*os.File
	var extraFiles []int
	if preserveFDs > 0 {
		for fd := 3; fd < int(3+preserveFDs); fd++ {
			f := os.NewFile(uintptr(fd), fmt.Sprintf("fd-%d", fd))
			filesToClose = append(filesToClose, f)
			extraFiles = append(extraFiles, int(f.Fd()))
		}
	}
	fmt.Println("---extra files 1----:", extraFiles)

	config.EnvVars = r.configureConmonEnv(runtimeDir)
	fmt.Println("---Env vars----:", config.EnvVars)

	if r.reservePorts && !rootless.IsRootless() && !ctr.config.NetMode.IsSlirp4netns() {
		ports, err := bindPorts(ctr.convertPortMappings())
		if err != nil {
			return 0, err
		}
		filesToClose = append(filesToClose, ports...)

		// Leak the port we bound in the conmon process. These fd's won't be used
		// by the container and conmon will keep the ports busy so that another
		// process cannot use them.
		for _, port := range ports {
			extraFiles = append(extraFiles, int(port.Fd()))
		}
		fmt.Println("---extra files 2----:", extraFiles)
	}

	// if ctr.config.NetMode.IsSlirp4netns() || rootless.IsRootless() {
	// 	fmt.Println("======IN NET stuff=====")
	// 	if ctr.config.PostConfigureNetNS {
	// 		havePortMapping := len(ctr.config.PortMappings) > 0
	// 		if havePortMapping {
	// 			ctr.rootlessPortSyncR, ctr.rootlessPortSyncW, err = os.Pipe()
	// 			if err != nil {
	// 				return 0, fmt.Errorf("failed to create rootless port sync pipe: %w", err)
	// 			}
	// 		}
	// 		ctr.rootlessSlirpSyncR, ctr.rootlessSlirpSyncW, err = os.Pipe()
	// 		if err != nil {
	// 			return 0, fmt.Errorf("failed to create rootless network sync pipe: %w", err)
	// 		}
	// 	} else {
	// 		if ctr.rootlessSlirpSyncR != nil {
	// 			defer errorhandling.CloseQuiet(ctr.rootlessSlirpSyncR)
	// 		}
	// 		if ctr.rootlessSlirpSyncW != nil {
	// 			defer errorhandling.CloseQuiet(ctr.rootlessSlirpSyncW)
	// 		}
	// 	}
	// 	fmt.Println("---leaking into conmon----")
	// 	// Leak one end in conmon, the other one will be leaked into slirp4netns
	// 	extraFiles = append(extraFiles, int(ctr.rootlessSlirpSyncW.Fd()))
	// 	fmt.Println("---extra files 3---:", extraFiles)

	// 	if ctr.rootlessPortSyncW != nil {
	// 		fmt.Println("---port sync not nil----")
	// 		defer errorhandling.CloseQuiet(ctr.rootlessPortSyncW)
	// 		// Leak one end in conmon, the other one will be leaked into rootlessport
	// 		extraFiles = append(extraFiles, int(ctr.rootlessPortSyncW.Fd()))
	// 	}
	// }

	if ctr.config.NetMode.IsSlirp4netns() || rootless.IsRootless() {
		fmt.Println("======IN NET stuff=====")
		if ctr.config.PostConfigureNetNS {
			havePortMapping := len(ctr.config.PortMappings) > 0
			if havePortMapping {
				ctr.rootlessPortSyncR, ctr.rootlessPortSyncW, err = os.Pipe()
				if err != nil {
					return 0, fmt.Errorf("failed to create rootless port sync pipe: %w", err)
				}
			}
			ctr.rootlessSlirpSyncR, ctr.rootlessSlirpSyncW, err = os.Pipe()
			if err != nil {
				return 0, fmt.Errorf("failed to create rootless network sync pipe: %w", err)
			}
		}

		if ctr.rootlessSlirpSyncW != nil {
			defer errorhandling.CloseQuiet(ctr.rootlessSlirpSyncW)
			// Leak one end in conmon, the other one will be leaked into slirp4netns
			extraFiles = append(extraFiles, int(ctr.rootlessSlirpSyncW.Fd()))
		}
		fmt.Println("---extra files 3---:", extraFiles)

		if ctr.rootlessPortSyncW != nil {
			defer errorhandling.CloseQuiet(ctr.rootlessPortSyncW)
			// Leak one end in conmon, the other one will be leaked into rootlessport
			extraFiles = append(extraFiles, int(ctr.rootlessPortSyncW.Fd()))
		}
		fmt.Println("---extra files 4---:", extraFiles)
	}

	config.CgroupManager, err = r.cgroupManager(ctr)
	if err != nil {
		return 0, err
	}

	client, err := r.client(1)
	if err != nil {
		return 0, err
	}

	if len(extraFiles) > 0 {
		remoteFDs, err := client.RemoteFDs(ctx)
		if err != nil {
			return 0, err
		}
		defer remoteFDs.Close()

		fmt.Println("---extra files----:", extraFiles)
		fds, err := remoteFDs.Send(extraFiles...)
		if err != nil {
			return 0, err
		}

		config.AdditionalFDs = fds[:preserveFDs]
		config.LeakFDs = fds[preserveFDs:]
	}

	var runtimeRestoreStarted time.Time
	if restoreOptions != nil {
		runtimeRestoreStarted = time.Now()
	}

	res, err := client.CreateContainer(ctx, config)
	if err != nil {
		return 0, err
	}

	fmt.Println("===create container pid===:", res.PID)
	fmt.Println("====conmonrs pid====:", client.PID())
	ctr.state.PID = int(res.PID)

	runtimeRestoreDuration := func() int64 {
		if restoreOptions != nil && restoreOptions.PrintStats {
			return time.Since(runtimeRestoreStarted).Microseconds()
		}
		return 0
	}()

	// These fds were passed down to the runtime.  Close them
	// and not interfere
	for _, f := range filesToClose {
		errorhandling.CloseQuiet(f)
	}

	return runtimeRestoreDuration, nil
}

// configureConmonEnv gets the environment values to add to conmon's exec struct
// TODO this may want to be less hardcoded/more configurable in the future
func (r *ConmonRSOCIRuntime) configureConmonEnv(runtimeDir string) map[string]string {
	env := map[string]string{}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "LC_") {
			if key, value, found := strings.Cut(e, "="); found {
				env[key] = value
			}
		}
	}
	if path, ok := os.LookupEnv("PATH"); ok {
		env["PATH"] = path
	}
	if conf, ok := os.LookupEnv("CONTAINERS_CONF"); ok {
		env["CONTAINERS_CONF"] = conf
	}
	if conf, ok := os.LookupEnv("CONTAINERS_HELPER_BINARY_DIR"); ok {
		env["CONTAINERS_HELPER_BINARY_DIR"] = conf
	}
	env["XDG_RUNTIME_DIR"] = runtimeDir
	env["_CONTAINERS_USERNS_CONFIGURED"] = os.Getenv("_CONTAINERS_USERNS_CONFIGURED")
	env["_CONTAINERS_ROOTLESS_UID"] = os.Getenv("_CONTAINERS_ROOTLESS_UID")
	if home := homedir.Get(); home != "" {
		env["HOME"] = home
	}

	return env
}

// UpdateContainerStatus updates the status of the given container.
func (r *ConmonRSOCIRuntime) UpdateContainerStatus(ctr *Container) error {
	fmt.Println("*****conmonrs update container status***")
	runtimeDir, err := util.GetRootlessRuntimeDir()
	if err != nil {
		return err
	}

	// Store old state so we know if we were already stopped
	oldState := ctr.state.State

	state := new(spec.State)

	cmd := exec.Command(r.path, "state", ctr.ID())
	cmd.Env = append(cmd.Env, fmt.Sprintf("XDG_RUNTIME_DIR=%s", runtimeDir))

	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("getting stdout pipe: %w", err)
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("getting stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		out, err2 := io.ReadAll(errPipe)
		if err2 != nil {
			return fmt.Errorf("getting container %s state: %w", ctr.ID(), err)
		}
		if strings.Contains(string(out), "does not exist") || strings.Contains(string(out), "No such file") {
			if err := ctr.removeConmonFiles(); err != nil {
				logrus.Debugf("unable to remove conmon files for container %s", ctr.ID())
			}
			ctr.state.ExitCode = -1
			ctr.state.FinishedTime = time.Now()
			ctr.state.State = define.ContainerStateExited
			return ctr.runtime.state.AddContainerExitCode(ctr.ID(), ctr.state.ExitCode)
		}
		return fmt.Errorf("getting container %s state. stderr/out: %s: %w", ctr.ID(), out, err)
	}
	defer func() {
		_ = cmd.Wait()
	}()

	if err := errPipe.Close(); err != nil {
		return err
	}
	out, err := io.ReadAll(outPipe)
	if err != nil {
		return fmt.Errorf("reading stdout: %s: %w", ctr.ID(), err)
	}
	if err := json.NewDecoder(bytes.NewReader(out)).Decode(state); err != nil {
		return fmt.Errorf("decoding container status for container %s: %w", ctr.ID(), err)
	}
	ctr.state.PID = state.Pid

	switch state.Status {
	case "created":
		ctr.state.State = define.ContainerStateCreated
	case "paused":
		ctr.state.State = define.ContainerStatePaused
	case "running":
		ctr.state.State = define.ContainerStateRunning
	case "stopped":
		ctr.state.State = define.ContainerStateStopped
	default:
		return fmt.Errorf("unrecognized status returned by runtime for container %s: %s: %w",
			ctr.ID(), state.Status, define.ErrInternal)
	}

	// Handle ContainerStateStopping - keep it unless the container
	// transitioned to no longer running.
	if oldState == define.ContainerStateStopping && (ctr.state.State == define.ContainerStatePaused || ctr.state.State == define.ContainerStateRunning) {
		ctr.state.State = define.ContainerStateStopping
	}

	return nil
}

// StartContainer starts the given container.
func (r *ConmonRSOCIRuntime) StartContainer(ctr *Container) error {
	// TODO: streams should probably *not* be our STDIN/OUT/ERR - redirect to buffers?
	runtimeDir, err := util.GetRootlessRuntimeDir()
	if err != nil {
		return err
	}
	env := []string{fmt.Sprintf("XDG_RUNTIME_DIR=%s", runtimeDir)}
	if path, ok := os.LookupEnv("PATH"); ok {
		env = append(env, fmt.Sprintf("PATH=%s", path))
	}
	if err := utils.ExecCmdWithStdStreams(os.Stdin, os.Stdout, os.Stderr, env, r.path, append(r.runtimeFlags, "start", ctr.ID())...); err != nil {
		return err
	}

	ctr.state.StartedTime = time.Now()

	return nil
}

// UpdateContainer updates the given container's cgroup configuration.
func (r *ConmonRSOCIRuntime) UpdateContainer(ctr *Container, resources *spec.LinuxResources) error {
	runtimeDir, err := util.GetRootlessRuntimeDir()
	if err != nil {
		return err
	}
	env := []string{fmt.Sprintf("XDG_RUNTIME_DIR=%s", runtimeDir)}
	if path, ok := os.LookupEnv("PATH"); ok {
		env = append(env, fmt.Sprintf("PATH=%s", path))
	}
	args := r.runtimeFlags
	args = append(args, "update")
	tempFile, additionalArgs, err := generateResourceFile(resources)
	if err != nil {
		return err
	}
	defer os.Remove(tempFile)

	args = append(args, additionalArgs...)
	return utils.ExecCmdWithStdStreams(os.Stdin, os.Stdout, os.Stderr, env, r.path, append(args, ctr.ID())...)
}

// KillContainer sends the given signal to the given container.
// If all is set, all processes in the container will be signalled;
// otherwise, only init will be signalled.
func (r *ConmonRSOCIRuntime) KillContainer(ctr *Container, signal uint, all bool) error {
	if _, err := r.killContainer(ctr, signal, all, false); err != nil {
		return err
	}

	return nil
}

// If captureStderr is requested, OCI runtime STDERR will be captured as a
// *bytes.buffer and returned; otherwise, it is set to os.Stderr.
func (r *ConmonRSOCIRuntime) killContainer(ctr *Container, signal uint, all, captureStderr bool) (*bytes.Buffer, error) {
	fmt.Printf("Sending signal %d to container %s\n", signal, ctr.ID())
	logrus.Debugf("Sending signal %d to container %s", signal, ctr.ID())
	runtimeDir, err := homedir.GetRuntimeDir()
	if err != nil {
		return nil, err
	}
	env := []string{fmt.Sprintf("XDG_RUNTIME_DIR=%s", runtimeDir)}
	var args []string
	args = append(args, r.runtimeFlags...)
	if all {
		args = append(args, "kill", "--all", ctr.ID(), fmt.Sprintf("%d", signal))
	} else {
		args = append(args, "kill", ctr.ID(), fmt.Sprintf("%d", signal))
	}
	var (
		stderr       io.Writer = os.Stderr
		stderrBuffer *bytes.Buffer
	)
	if captureStderr {
		stderrBuffer = new(bytes.Buffer)
		stderr = stderrBuffer
	}
	if err := utils.ExecCmdWithStdStreams(os.Stdin, os.Stdout, stderr, env, r.path, args...); err != nil {
		// Update container state - there's a chance we failed because
		// the container exited in the meantime.
		if err2 := r.UpdateContainerStatus(ctr); err2 != nil {
			logrus.Infof("Error updating status for container %s: %v", ctr.ID(), err2)
		}
		if ctr.ensureState(define.ContainerStateStopped, define.ContainerStateExited) {
			return stderrBuffer, fmt.Errorf("%w: %s", define.ErrCtrStateInvalid, ctr.state.State)
		}
		return stderrBuffer, fmt.Errorf("sending signal to container %s: %w", ctr.ID(), err)
	}

	return stderrBuffer, nil
}

// StopContainer stops the given container.
// The container's stop signal (or SIGTERM if unspecified) will be sent
// first.
// After the given timeout, SIGKILL will be sent.
// If the given timeout is 0, SIGKILL will be sent immediately, and the
// stop signal will be omitted.
// If all is set, we will attempt to use the --all flag will `kill` in
// the OCI runtime to kill all processes in the container, including
// exec sessions. This is only supported if the container has cgroups.
func (r *ConmonRSOCIRuntime) StopContainer(ctr *Container, timeout uint, all bool) error {
	fmt.Printf("-----Stopping container %s (PID %d)\n", ctr.ID(), ctr.state.PID)
	logrus.Debugf("Stopping container %s (PID %d)", ctr.ID(), ctr.state.PID)

	// Ping the container to see if it's alive
	// If it's not, it's already stopped, return
	err := unix.Kill(ctr.state.PID, 0)
	if err == unix.ESRCH {
		return nil
	}

	killCtr := func(signal uint) (bool, error) {
		stderr, err := r.killContainer(ctr, signal, all, true)
		if err != nil {
			// There's an inherent race with the cleanup process (see
			// #16142, #17142). If the container has already been marked as
			// stopped or exited by the cleanup process, we can return
			// immediately.
			if errors.Is(err, define.ErrCtrStateInvalid) && ctr.ensureState(define.ContainerStateStopped, define.ContainerStateExited) {
				return true, nil
			}

			// If the PID is 0, then the container is already stopped.
			if ctr.state.PID == 0 {
				return true, nil
			}

			// Is the container gone?
			// If so, it probably died between the first check and
			// our sending the signal
			// The container is stopped, so exit cleanly
			err := unix.Kill(ctr.state.PID, 0)
			if err == unix.ESRCH {
				return true, nil
			}

			return false, err
		}

		// Before handling error from KillContainer, convert STDERR to a []string
		// (one string per line of output) and print it.
		stderrLines := strings.Split(stderr.String(), "\n")
		for _, line := range stderrLines {
			if line != "" {
				fmt.Fprintf(os.Stderr, "%s\n", line)
			}
		}

		return false, nil
	}

	if timeout > 0 {
		stopSignal := ctr.config.StopSignal
		if stopSignal == 0 {
			stopSignal = uint(syscall.SIGTERM)
		}

		stopped, err := killCtr(stopSignal)
		if err != nil {
			return err
		}
		if stopped {
			return nil
		}

		if err := waitContainerStop(ctr, time.Duration(util.ConvertTimeout(int(timeout)))*time.Second); err != nil {
			sigName := unix.SignalName(syscall.Signal(stopSignal))
			if sigName == "" {
				sigName = fmt.Sprintf("(%d)", stopSignal)
			}
			logrus.Debugf("Timed out stopping container %s with %s, resorting to SIGKILL: %v", ctr.ID(), sigName, err)
			logrus.Warnf("StopSignal %s failed to stop container %s in %d seconds, resorting to SIGKILL", sigName, ctr.Name(), timeout)
		} else {
			// No error, the container is dead
			return nil
		}
	}

	stopped, err := killCtr(uint(unix.SIGKILL))
	if err != nil {
		return fmt.Errorf("sending SIGKILL to container %s: %w", ctr.ID(), err)
	}
	if stopped {
		return nil
	}

	// Give runtime a few seconds to make it happen
	if err := waitContainerStop(ctr, killContainerTimeout); err != nil {
		return err
	}

	return nil
}

// DeleteContainer deletes the given container from the OCI runtime.
func (r *ConmonRSOCIRuntime) DeleteContainer(ctr *Container) error {
	runtimeDir, err := util.GetRootlessRuntimeDir()
	if err != nil {
		return err
	}
	env := []string{fmt.Sprintf("XDG_RUNTIME_DIR=%s", runtimeDir)}
	return utils.ExecCmdWithStdStreams(os.Stdin, os.Stdout, os.Stderr, env, r.path, append(r.runtimeFlags, "delete", "--force", ctr.ID())...)
}

// PauseContainer pauses the given container.
func (r *ConmonRSOCIRuntime) PauseContainer(ctr *Container) error {
	runtimeDir, err := util.GetRootlessRuntimeDir()
	if err != nil {
		return err
	}
	env := []string{fmt.Sprintf("XDG_RUNTIME_DIR=%s", runtimeDir)}
	return utils.ExecCmdWithStdStreams(os.Stdin, os.Stdout, os.Stderr, env, r.path, append(r.runtimeFlags, "pause", ctr.ID())...)
}

// UnpauseContainer unpauses the given container.
func (r *ConmonRSOCIRuntime) UnpauseContainer(ctr *Container) error {
	runtimeDir, err := util.GetRootlessRuntimeDir()
	if err != nil {
		return err
	}
	env := []string{fmt.Sprintf("XDG_RUNTIME_DIR=%s", runtimeDir)}
	return utils.ExecCmdWithStdStreams(os.Stdin, os.Stdout, os.Stderr, env, r.path, append(r.runtimeFlags, "resume", ctr.ID())...)
}

func printPtr(p any) string {
	v := reflect.ValueOf(p)
	for v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	return fmt.Sprint(v.Interface())
}

// WriteCloserWrapper is a simple wrapper around io.Writer that satisfies io.Closer as well.
type WriteCloserWrapper struct {
	io.Writer
	io.Closer
}

// NewWriteCloserWrapper creates a new WriteCloserWrapper.
func NewWriteCloserWrapper(writer io.Writer, closer io.Closer) *WriteCloserWrapper {
	return &WriteCloserWrapper{
		Writer: writer,
		Closer: closer,
	}
}

// Attach to a container.
func (r *ConmonRSOCIRuntime) Attach(ctr *Container, params *AttachOptions) error {
	logrus.WithFields(logrus.Fields{
		"DetachKeys":   printPtr(params.DetachKeys),
		"Start":        params.Start,
		"AttachInput":  params.Streams.AttachInput,
		"AttachOutput": params.Streams.AttachOutput,
		"AttachError":  params.Streams.AttachError,
		"Terminal":     ctr.Terminal(),
		"Stdin":        ctr.Stdin(),
	}).Warnln("attach")

	fmt.Println("----params----:", params)

	var err error

	keys := config.DefaultDetachKeys
	if params.DetachKeys != nil {
		keys = *params.DetachKeys
	}

	config := new(client.AttachConfig)
	config.ID = ctr.ID()
	config.SocketPath, err = ctr.AttachSocketPath()
	// config.SocketPath, err = r.AttachSocketPath(ctr)
	if err != nil {
		return err
	}

	config.Tty = ctr.Terminal()
	config.ContainerStdin = ctr.Stdin()
	config.StopAfterStdinEOF = true
	config.Resize = params.InitialSize

	if params.Streams.AttachInput {
		config.Streams.Stdin = &client.In{
			ReadCloser: io.NopCloser(params.Streams.InputStream),
		}
	}
	if params.Streams.AttachOutput && params.Streams.OutputStream != nil {
		outputStreamWrapper := NewWriteCloserWrapper(params.Streams.OutputStream, nil)
		config.Streams.Stdout = &client.Out{
			WriteCloser: outputStreamWrapper,
		}
	}
	if params.Streams.AttachError && params.Streams.ErrorStream != nil {
		errorStreamWrapper := NewWriteCloserWrapper(params.Streams.ErrorStream, nil)
		config.Streams.Stderr = &client.Out{
			WriteCloser: errorStreamWrapper,
		}
	}

	if params.Start {
		config.PreAttachFunc = func() error {
			if err := r.StartContainer(ctr); err != nil {
				return err
			}
			params.Started <- true
			return nil
		}
	}

	config.PostAttachFunc = func() error {
		if params.AttachReady != nil {
			params.AttachReady <- true
		}
		return nil
	}

	if keys != "" {
		config.DetachKeys, err = term.ToBytes(keys)
		if err != nil {
			return fmt.Errorf("invalid detach keys: %w", err)
		}
	}

	client, err := r.client(1)
	if err != nil {
		return err
	}

	if err := client.AttachContainer(context.Background(), config); err != nil {
		logrus.Errorf("attach done: %v", err)
		return err
	}

	logrus.Warn("attach done: ok")
	return nil
}

// HTTPAttach performs an attach intended to be transported over HTTP.
// For terminal attach, the container's output will be directly streamed
// to output; otherwise, STDOUT and STDERR will be multiplexed, with
// a header prepended as follows: 1-byte STREAM (0, 1, 2 for STDIN,
// STDOUT, STDERR), 3 null (0x00) bytes, 4-byte big endian length.
// If a cancel channel is provided, it can be used to asynchronously
// terminate the attach session. Detach keys, if given, will also cause
// the attach session to be terminated if provided via the STDIN
// channel. If they are not provided, the default detach keys will be
// used instead. Detach keys of "" will disable detaching via keyboard.
// The streams parameter will determine which streams to forward to the
// client.
func (r *ConmonRSOCIRuntime) HTTPAttach(ctr *Container, req *http.Request, w http.ResponseWriter, streams *HTTPAttachStreams, detachKeys *string, cancel <-chan bool, hijackDone chan<- bool, streamAttach, streamLogs bool) error {
	return r.printError("HTTPAttach")
}

// AttachResize resizes the terminal in use by the given container.
func (r *ConmonRSOCIRuntime) AttachResize(ctr *Container, newSize resize.TerminalSize) error {
	return r.printError("AttachResize")
}

// ExecContainer executes a command in a running container.
// Returns an int (PID of exec session), error channel (errors from
// attach), and error (errors that occurred attempting to start the exec
// session). This returns once the exec session is running - not once it
// has completed, as one might expect. The attach session will remain
// running, in a goroutine that will return via the chan error in the
// return signature.
// newSize resizes the tty to this size before the process is started, must be nil if the exec session has no tty
func (r *ConmonRSOCIRuntime) ExecContainer(ctr *Container, sessionID string, options *ExecOptions, streams *define.AttachStreams, newSize *resize.TerminalSize) (int, chan error, error) {
	return -1, nil, r.printError("ExecContainer")
}

// ExecContainerHTTP executes a command in a running container and
// attaches its standard streams to a provided hijacked HTTP session.
// Maintains the same invariants as ExecContainer (returns on session
// start, with a goroutine running in the background to handle attach).
// The HTTP attach itself maintains the same invariants as HTTPAttach.
// newSize resizes the tty to this size before the process is started, must be nil if the exec session has no tty
func (r *ConmonRSOCIRuntime) ExecContainerHTTP(ctr *Container, sessionID string, options *ExecOptions, req *http.Request, w http.ResponseWriter,
	streams *HTTPAttachStreams, cancel <-chan bool, hijackDone chan<- bool, holdConnOpen <-chan bool, newSize *resize.TerminalSize) (int, chan error, error) {
	return -1, nil, r.printError("ExecContainerHTTP")
}

// ExecContainerDetached executes a command in a running container, but
// does not attach to it. Returns the PID of the exec session and an
// error (if starting the exec session failed)
func (r *ConmonRSOCIRuntime) ExecContainerDetached(ctr *Container, sessionID string, options *ExecOptions, stdin bool) (int, error) {
	return -1, r.printError("ExecContainerDetached")
}

// ExecAttachResize resizes the terminal of a running exec session. Only
// allowed with sessions that were created with a TTY.
func (r *ConmonRSOCIRuntime) ExecAttachResize(ctr *Container, sessionID string, newSize resize.TerminalSize) error {
	return r.printError("ExecAttachResize")
}

// ExecStopContainer stops a given exec session in a running container.
// SIGTERM with be sent initially, then SIGKILL after the given timeout.
// If timeout is 0, SIGKILL will be sent immediately, and SIGTERM will
// be omitted.
func (r *ConmonRSOCIRuntime) ExecStopContainer(ctr *Container, sessionID string, timeout uint) error {
	return r.printError("ExecStopContainer")
}

// ExecUpdateStatus checks the status of a given exec session.
// Returns true if the session is still running, or false if it exited.
func (r *ConmonRSOCIRuntime) ExecUpdateStatus(ctr *Container, sessionID string) (bool, error) {
	return false, r.printError("ExecUpdateStatus")
}

// CheckpointContainer checkpoints the given container.
// Some OCI runtimes may not support this - if SupportsCheckpoint()
// returns false, this is not implemented, and will always return an
// error. If CheckpointOptions.PrintStats is true the first return parameter
// contains the number of microseconds the runtime needed to checkpoint
// the given container.
func (r *ConmonRSOCIRuntime) CheckpointContainer(ctr *Container, options ContainerCheckpointOptions) (int64, error) {
	return 0, r.printError("CheckpointContainer")
}

// CheckConmonRunning verifies that the given container's Conmon
// instance is still running. Runtimes without Conmon, or systems where
// the PID of conmon is not available, should mock this as True.
// True indicates that Conmon for the instance is running, False
// indicates it is not.
func (r *ConmonRSOCIRuntime) CheckConmonRunning(ctr *Container) (bool, error) {
	return true, nil
}

// SupportsCheckpoint returns whether this OCI runtime
// implementation supports the CheckpointContainer() operation.
func (r *ConmonRSOCIRuntime) SupportsCheckpoint() bool {
	return false
}

// SupportsJSONErrors is whether the runtime can return JSON-formatted
// error messages.
func (r *ConmonRSOCIRuntime) SupportsJSONErrors() bool {
	return r.supportsJSON
}

// SupportsNoCgroups is whether the runtime supports running containers
// without cgroups.
func (r *ConmonRSOCIRuntime) SupportsNoCgroups() bool {
	return r.supportsNoCgroups
}

// SupportsKVM os whether the OCI runtime supports running containers
// without KVM separation
func (r *ConmonRSOCIRuntime) SupportsKVM() bool {
	return r.supportsKVM
}

// AttachSocketPath is the path to the socket to attach to a given
// container.
func (r *ConmonRSOCIRuntime) AttachSocketPath(ctr *Container) (string, error) {
	if ctr == nil {
		return "", fmt.Errorf("must provide a valid container to get attach socket path: %w", define.ErrInvalidArg)
	}

	return filepath.Join(ctr.bundlePath(), "attach"), nil
}

// ExecAttachSocketPath is the path to the socket to attach to a given
// exec session in the given container.
func (r *ConmonRSOCIRuntime) ExecAttachSocketPath(ctr *Container, sessionID string) (string, error) {
	return "", r.printError("ExecAttachSocketPath")
}

// ExitFilePath is the path to a container's exit file.
func (r *ConmonRSOCIRuntime) ExitFilePath(ctr *Container) (string, error) {
	fmt.Println("**** conmonrs exit file*****")
	if ctr == nil {
		return "", fmt.Errorf("must provide a valid container to get exit file path: %w", define.ErrInvalidArg)
	}
	return filepath.Join(r.exitsDir, ctr.ID()), nil
}

// ExitFilePath is the path to a container's exit file.
func (r *ConmonRSOCIRuntime) OOMFilePath(ctr *Container) (string, error) {
	fmt.Println("**** conmonrs oom file*****")
	if ctr == nil {
		return "", fmt.Errorf("must provide a valid container to get exit file path: %w", define.ErrInvalidArg)
	}
	return filepath.Join(r.exitsDir, "oom-"+ctr.ID()), nil
}

// getConmonVersion returns a string representation of the conmon version.
func (r *ConmonRSOCIRuntime) getConmonRSVersion() (string, error) {
	output, err := utils.ExecCmd(r.conmonPath, "--version")
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.Replace(output, "\n", ", ", 1), "\n"), nil
}

// getOCIRuntimeVersion returns a string representation of the OCI runtime's
// version.
func (r *ConmonRSOCIRuntime) getOCIRuntimeVersion() (string, error) {
	output, err := utils.ExecCmd(r.path, "--version")
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(output, "\n"), nil
}

// RuntimeInfo returns verbose information about the runtime.
func (r *ConmonRSOCIRuntime) RuntimeInfo() (*define.ConmonInfo, *define.OCIRuntimeInfo, error) {
	runtimePackage := cversion.Package(r.path)
	conmonPackage := cversion.Package(r.conmonPath)
	runtimeVersion, err := r.getOCIRuntimeVersion()
	if err != nil {
		return nil, nil, fmt.Errorf("getting version of OCI runtime %s: %w", r.name, err)
	}
	conmonVersion, err := r.getConmonRSVersion()
	if err != nil {
		return nil, nil, fmt.Errorf("getting conmon version: %w", err)
	}
	conmon := define.ConmonInfo{
		Package: conmonPackage,
		Path:    "conmonrs (in PATH)",
		Version: conmonVersion,
	}
	ocirt := define.OCIRuntimeInfo{
		Name:    r.name,
		Path:    "todo: path",
		Package: runtimePackage,
		Version: runtimeVersion,
	}
	return &conmon, &ocirt, nil
}

// Return an error indicating the feature is not implemented
func (r *ConmonRSOCIRuntime) printError(feature string) error {
	return fmt.Errorf("conmon-rs runtime %s: %s not implemented", r.name, feature)
}
