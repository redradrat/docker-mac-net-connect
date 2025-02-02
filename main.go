//go:build darwin

package main

import (
	"context"
	"fmt"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/context/docker"
	"github.com/docker/cli/cli/flags"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/ipc"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/chipmk/docker-mac-net-connect/networkmanager"
	"github.com/chipmk/docker-mac-net-connect/version"
)

const (
	ExitSetupSuccess = 0
	ExitSetupFailed  = 1
)

const (
	ENV_WG_TUN_FD             = "WG_TUN_FD"
	ENV_WG_UAPI_FD            = "WG_UAPI_FD"
	ENV_WG_PROCESS_FOREGROUND = "WG_PROCESS_FOREGROUND"
)

func main() {
	logLevel := func() int {
		switch os.Getenv("LOG_LEVEL") {
		case "verbose", "debug":
			return device.LogLevelVerbose
		case "error":
			return device.LogLevelError
		case "silent":
			return device.LogLevelSilent
		}
		return device.LogLevelVerbose
	}()

	fmt.Printf("docker-mac-net-connect version '%s'\n", version.Version)

	tun, err := tun.CreateTUN("utun", device.DefaultMTU)
	if err != nil {
		fmt.Println(fmt.Sprintf("Error during tunnel setup: %q", fmt.Errorf("failed to create TUN device: %v", err)))
		os.Exit(ExitSetupFailed)
	}

	interfaceName, err := tun.Name()
	if err != nil {
		fmt.Errorf("failed to get TUN device name: %v", err)
		os.Exit(ExitSetupFailed)
	}

	logger := device.NewLogger(
		logLevel,
		fmt.Sprintf("(%s) ", interfaceName),
	)

	fileUAPI, err := ipc.UAPIOpen(interfaceName)

	if err != nil {
		logger.Errorf("UAPI listen error: %v", err)
		os.Exit(ExitSetupFailed)
	}

	device := device.NewDevice(tun, conn.NewDefaultBind(), logger)

	logger.Verbosef("Device started")

	errs := make(chan error)
	term := make(chan os.Signal, 1)

	uapi, err := ipc.UAPIListen(interfaceName, fileUAPI)
	if err != nil {
		logger.Errorf("failed to listen on UAPI socket: %v", err)
		os.Exit(ExitSetupFailed)
	}

	go func() {
		for {
			conn, err := uapi.Accept()
			if err != nil {
				errs <- err
				return
			}
			go device.IpcHandle(conn)
		}
	}()

	logger.Verbosef("UAPI listener started")

	// Wireguard configuration

	hostPeerIp := "10.33.33.1"
	vmPeerIp := "10.33.33.2"

	c, err := wgctrl.New()
	if err != nil {
		logger.Errorf("failed to create new wgctrl client: %v", err)
		os.Exit(ExitSetupFailed)
	}

	defer c.Close()

	hostPrivateKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		logger.Errorf("failed to generate host private key: %v", err)
		os.Exit(ExitSetupFailed)
	}

	vmPrivateKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		logger.Errorf("failed to generate VM private key: %v", err)
		os.Exit(ExitSetupFailed)
	}

	_, wildcardIpNet, err := net.ParseCIDR("0.0.0.0/0")
	if err != nil {
		logger.Errorf("failed to parse wildcard CIDR: %v", err)
		os.Exit(ExitSetupFailed)
	}

	_, vmIpNet, err := net.ParseCIDR(vmPeerIp + "/32")
	if err != nil {
		logger.Errorf("failed to parse VM peer CIDR: %v", err)
		os.Exit(ExitSetupFailed)
	}

	peer := wgtypes.PeerConfig{
		PublicKey: vmPrivateKey.PublicKey(),
		AllowedIPs: []net.IPNet{
			*wildcardIpNet,
			*vmIpNet,
		},
	}

	port := 3333
	err = c.ConfigureDevice(interfaceName, wgtypes.Config{
		ListenPort: &port,
		PrivateKey: &hostPrivateKey,
		Peers:      []wgtypes.PeerConfig{peer},
	})
	if err != nil {
		logger.Errorf("failed to configure Wireguard device: %v\n", err)
		os.Exit(ExitSetupFailed)
	}

	networkManager := networkmanager.New()

	_, stderr, err := networkManager.SetInterfaceAddress(hostPeerIp, vmPeerIp, interfaceName)
	if err != nil {
		logger.Errorf("failed to set interface address with ifconfig: %v. %v", err, stderr)
		os.Exit(ExitSetupFailed)
	}

	logger.Verbosef("Interface %s created\n", interfaceName)

	dockerCli, err := command.NewDockerCli()
	if err != nil {
		logger.Errorf("failed to create docker cli client: %v. %v", err, stderr)
		os.Exit(ExitSetupFailed)
	}

	if err := dockerCli.Initialize(flags.NewClientOptions()); err != nil {
		logger.Errorf("failed to initialize docker cli client: %v. %v", err, stderr)
		os.Exit(ExitSetupFailed)
	}

	dockerHost := client.DefaultDockerHost
	currentContext := dockerCli.CurrentContext()
	if currentContext != "" {
		logger.Verbosef("Detected existing docker context. Using context %q from now on...\n", currentContext)

		md, err := dockerCli.ContextStore().GetMetadata(dockerCli.CurrentContext())
		if err != nil {
			logger.Errorf("failed to get docker context endpoint: %v. %v", err, stderr)
			os.Exit(ExitSetupFailed)
		}
		typed, ok := md.Endpoints[docker.DockerEndpoint].(docker.EndpointMeta)
		if !ok {
			logger.Errorf("endpoint %q of context %q is not of type EndpointMeta.", docker.DockerEndpoint, currentContext)
			os.Exit(ExitSetupFailed)
		}

		// Set the docker host to the result of whatever we were able to get from the context store.
		dockerHost = typed.Host
	}

	dockerApiClient, err := client.NewClientWithOpts(client.FromEnv, client.WithHost(dockerHost))
	if err != nil {
		logger.Errorf("failed to create Docker client: %v", err)
		os.Exit(ExitSetupFailed)
	}

	logger.Verbosef("Wireguard server listening\n")

	ctx := context.Background()

	go func() {
		for {
			logger.Verbosef("Setting up Wireguard on Docker Desktop VM\n")

			err = setupVm(ctx, dockerApiClient, port, hostPeerIp, vmPeerIp, hostPrivateKey, vmPrivateKey)
			if err != nil {
				logger.Errorf("failed to setup VM: %v", err)
				time.Sleep(5 * time.Second)
				continue
			}

			networks, err := dockerApiClient.NetworkList(ctx, types.NetworkListOptions{})
			if err != nil {
				logger.Errorf("failed to list Docker networks: %v", err)
				time.Sleep(5 * time.Second)
				continue
			}

			for _, network := range networks {
				networkManager.ProcessDockerNetworkCreate(network, interfaceName)
			}

			logger.Verbosef("Watching Docker events\n")

			msgs, errsChan := dockerApiClient.Events(ctx, types.EventsOptions{
				Filters: filters.NewArgs(
					filters.Arg("type", "network"),
					filters.Arg("event", "create"),
					filters.Arg("event", "destroy"),
				),
			})

			for loop := true; loop; {
				select {
				case err := <-errsChan:
					logger.Errorf("Error: %v\n", err)
					loop = false
				case msg := <-msgs:
					// Add routes when new Docker networks are created
					if msg.Type == "network" && msg.Action == "create" {
						network, err := dockerApiClient.NetworkInspect(ctx, msg.Actor.ID, types.NetworkInspectOptions{})
						if err != nil {
							logger.Errorf("failed to inspect new Docker network: %v", err)
							continue
						}

						networkManager.ProcessDockerNetworkCreate(network, interfaceName)
						continue
					}

					// Delete routes when Docker networks are destroyed
					if msg.Type == "network" && msg.Action == "destroy" {
						network, exists := networkManager.DockerNetworks[msg.Actor.ID]
						if !exists {
							logger.Errorf("Unknown Docker network with ID %s. No routes will be removed.")
							continue
						}

						networkManager.ProcessDockerNetworkDestroy(network)
						continue
					}
				}
			}

			time.Sleep(5 * time.Second)
		}
	}()

	// Wait for program to terminate

	signal.Notify(term, syscall.SIGTERM)
	signal.Notify(term, os.Interrupt)

	select {
	case <-term:
	case <-errs:
	case <-device.Wait():
	}

	// Clean up

	uapi.Close()
	device.Close()

	logger.Verbosef("Shutting down\n")
}

func setupVm(
	ctx context.Context,
	dockerCli *client.Client,
	serverPort int,
	hostPeerIp string,
	vmPeerIp string,
	hostPrivateKey wgtypes.Key,
	vmPrivateKey wgtypes.Key,
) error {
	imageName := fmt.Sprintf("%s:%s", version.SetupImage, version.Version)

	_, _, err := dockerCli.ImageInspectWithRaw(ctx, imageName)
	if err != nil {
		fmt.Printf("Image doesn't exist locally. Pulling...\n")

		pullStream, err := dockerCli.ImagePull(ctx, imageName, types.ImagePullOptions{})
		if err != nil {
			return fmt.Errorf("failed to pull setup image: %w", err)
		}

		io.Copy(os.Stdout, pullStream)
	}

	resp, err := dockerCli.ContainerCreate(ctx, &container.Config{
		Image: imageName,
		Env: []string{
			"SERVER_PORT=" + strconv.Itoa(serverPort),
			"HOST_PEER_IP=" + hostPeerIp,
			"VM_PEER_IP=" + vmPeerIp,
			"HOST_PUBLIC_KEY=" + hostPrivateKey.PublicKey().String(),
			"VM_PRIVATE_KEY=" + vmPrivateKey.String(),
		},
	}, &container.HostConfig{
		AutoRemove:  true,
		NetworkMode: "host",
		CapAdd:      []string{"NET_ADMIN"},
	}, nil, nil, "wireguard-setup")
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}

	// Run container to completion
	err = dockerCli.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{})
	if err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}

	func() error {
		reader, err := dockerCli.ContainerLogs(ctx, resp.ID, types.ContainerLogsOptions{
			ShowStdout: true,
			ShowStderr: true,
			Follow:     true,
		})
		if err != nil {
			return fmt.Errorf("failed to get logs for container %s: %w", resp.ID, err)
		}

		defer reader.Close()

		_, err = stdcopy.StdCopy(os.Stdout, os.Stderr, reader)
		if err != nil {
			return err
		}

		return nil
	}()

	fmt.Println("Setup container complete")

	return nil
}
