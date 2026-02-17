package container

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/docker/docker/api/types"
	docker_container "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
)

// Client wraps the docker client to provide a simplified interface for Xavi.
type Client struct {
	cli *client.Client
}

// NewClient creates a new docker client from environment variables.
func NewClient() (*Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}
	return &Client{cli: cli}, nil
}

// Login authenticates with a docker registry.
func (c *Client) Login(ctx context.Context, username, password, registryAddr string) error {
	authConfig := registry.AuthConfig{
		Username:      username,
		Password:      password,
		ServerAddress: registryAddr,
	}

	_, err := c.cli.RegistryLogin(ctx, authConfig)
	if err != nil {
		return fmt.Errorf("failed to login to registry %s: %w", registryAddr, err)
	}

	return nil
}

// RunOptions configuration for running a container
type RunOptions struct {
	Image    string
	Name     string
	Env      []string
	Mounts   []string // format: /host/path:/container/path
	Memory   int64    // in bytes
	NanoCPUs int64    // 1e9 = 1 CPU
	Network  string
	Cmd      []string // Command override
	Ports    []string // format: hostPort:containerPort
}

// EnsureNetwork ensures a docker network exists.
func (c *Client) EnsureNetwork(ctx context.Context, name string) error {
	// Check if network exists
	_, err := c.cli.NetworkInspect(ctx, name, types.NetworkInspectOptions{})
	if err == nil {
		return nil
	}
	if !client.IsErrNotFound(err) {
		return fmt.Errorf("failed to inspect network %s: %w", name, err)
	}

	// Create network
	fmt.Printf("Creating network %s...\n", name)
	_, err = c.cli.NetworkCreate(ctx, name, types.NetworkCreate{
		Driver: "bridge",
	})
	if err != nil {
		return fmt.Errorf("failed to create network %s: %w", name, err)
	}

	return nil
}

// RunContainer starts a container with the given options.
// It pulls the image if not present.
func (c *Client) RunContainer(ctx context.Context, opts RunOptions) error {
	image := opts.Image
	name := opts.Name

	// check if image exists locally
	_, _, err := c.cli.ImageInspectWithRaw(ctx, image)
	if client.IsErrNotFound(err) {
		fmt.Printf("Pulling image %s...\n", image)
		reader, err := c.cli.ImagePull(ctx, image, types.ImagePullOptions{})
		if err != nil {
			return fmt.Errorf("failed to pull image %s: %w", image, err)
		}
		defer reader.Close()
		// discard output for now, or pipe to stdout if verbose
		io.Copy(io.Discard, reader)
	} else if err != nil {
		return fmt.Errorf("failed to inspect image %s: %w", image, err)
	}

	// Create container
	hostConfig := &docker_container.HostConfig{
		Binds: opts.Mounts,
		Resources: docker_container.Resources{
			Memory:   opts.Memory,
			NanoCPUs: opts.NanoCPUs,
		},
		RestartPolicy: docker_container.RestartPolicy{
			Name: docker_container.RestartPolicyAlways,
		},
	}

	// Port Mappings
	exposedPorts := make(nat.PortSet)
	portBindings := make(nat.PortMap)
	for _, p := range opts.Ports {
		var hostPort, containerPort string
		n, _ := fmt.Sscanf(p, "%[^:]:%s", &hostPort, &containerPort)
		if n == 2 {
			cPort := nat.Port(containerPort + "/tcp")
			exposedPorts[cPort] = struct{}{}
			portBindings[cPort] = []nat.PortBinding{
				{HostIP: "0.0.0.0", HostPort: hostPort},
			}
		}
	}
	hostConfig.PortBindings = portBindings

	var networkingConfig *network.NetworkingConfig
	if opts.Network != "" {
		networkingConfig = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				opts.Network: {},
			},
		}
	}

	// CONFIG CONVERGENCE CHECK
	inspect, err := c.cli.ContainerInspect(ctx, name)
	if err == nil {
		// Container exists, check if it matches desired state
		matches := c.compareConfig(inspect, opts, hostConfig, exposedPorts)
		if matches {
			if inspect.State.Running {
				fmt.Printf("Container %s already running and matches desired state, skipping.\n", name)
				return nil
			}
			fmt.Printf("Container %s exists and matches desired state but is not running. Starting...\n", name)
			return c.cli.ContainerStart(ctx, inspect.ID, docker_container.StartOptions{})
		}

		fmt.Printf("Container %s configuration mismatch. Recreating...\n", name)
		if err := c.StopContainer(ctx, name); err != nil {
			return fmt.Errorf("failed to remove old container %s: %w", name, err)
		}
	} else if !client.IsErrNotFound(err) {
		return fmt.Errorf("failed to inspect container %s: %w", name, err)
	}

	resp, err := c.cli.ContainerCreate(ctx, &docker_container.Config{
		Image:        image,
		Env:          opts.Env,
		Cmd:          opts.Cmd,
		ExposedPorts: exposedPorts,
	}, hostConfig, networkingConfig, nil, name)
	if err != nil {
		return fmt.Errorf("failed to create container %s: %w", name, err)
	}

	if err := c.cli.ContainerStart(ctx, resp.ID, docker_container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start container %s: %w", name, err)
	}

	fmt.Printf("Container %s started with ID %s\n", name, resp.ID)
	return nil
}

// StopContainer stops and removes a container by name.
func (c *Client) StopContainer(ctx context.Context, name string) error {
	// check if container exists
	containers, err := c.cli.ContainerList(ctx, docker_container.ListOptions{All: true})
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	var containerID string
	for _, container := range containers {
		for _, n := range container.Names {
			if n == "/"+name {
				containerID = container.ID
				break
			}
		}
	}

	if containerID == "" {
		return nil // Container not found, nothing to do
	}

	fmt.Printf("Stopping container %s (%s)...\n", name, containerID)
	if err := c.cli.ContainerStop(ctx, containerID, docker_container.StopOptions{}); err != nil {
		return fmt.Errorf("failed to stop container %s: %w", name, err)
	}

	fmt.Printf("Removing container %s...\n", name)
	if err := c.cli.ContainerRemove(ctx, containerID, docker_container.RemoveOptions{}); err != nil {
		return fmt.Errorf("failed to remove container %s: %w", name, err)
	}

	return nil
}

// Logs returns the logs of a container.
func (c *Client) Logs(ctx context.Context, name string) error {
	// simple implementation to just dump logs to stdout
	out, err := c.cli.ContainerLogs(ctx, name, docker_container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return err
	}
	defer out.Close()
	stdcopy.StdCopy(os.Stdout, os.Stderr, out)
	return nil
}

func (c *Client) compareConfig(inspect types.ContainerJSON, opts RunOptions, hostConfig *docker_container.HostConfig, exposedPorts nat.PortSet) bool {
	// 1. Image check
	// Note: Comparing Image IDs is better than names/tags because tags can change.
	// However, Xavi might want to pull latest. For now, let's compare the Image field directly.
	if inspect.Config.Image != opts.Image && inspect.Image != opts.Image {
		// check if inspect.Image is the resolved ID of opts.Image
		// (omitted for simplicity, but let's assume direct match for now)
		return false
	}

	// 2. Command check
	if len(inspect.Config.Cmd) != len(opts.Cmd) {
		return false
	}
	for i := range opts.Cmd {
		if inspect.Config.Cmd[i] != opts.Cmd[i] {
			return false
		}
	}

	// 3. Env check
	if !compareSlices(inspect.Config.Env, opts.Env) {
		return false
	}

	// 4. Mounts check (Binds)
	if !compareSlices(inspect.HostConfig.Binds, opts.Mounts) {
		return false
	}

	// 5. Ports check
	if len(inspect.HostConfig.PortBindings) != len(hostConfig.PortBindings) {
		return false
	}
	for k, v := range hostConfig.PortBindings {
		existing, ok := inspect.HostConfig.PortBindings[k]
		if !ok || len(existing) != len(v) {
			return false
		}
		for i := range v {
			if existing[i].HostIP != v[i].HostIP || existing[i].HostPort != v[i].HostPort {
				return false
			}
		}
	}

	return true
}

func compareSlices(a, b []string) bool {
	if len(a) != len(b) {
		// Filter out standard docker envs that might be injected (PATH, etc) if needed,
		// but for Xavi we'll assume exact match or a more lax check.
		// Let's do a semi-lax check: ensure all b elements are in a.
		count := 0
		for _, target := range b {
			found := false
			for _, actual := range a {
				if actual == target {
					found = true
					break
				}
			}
			if found {
				count++
			}
		}
		return count == len(b)
	}

	// Sort and compare? Or just check contents.
	// Since order might matter or differ, let's just check if they have the same elements.
	sa := make([]string, len(a))
	copy(sa, a)
	sb := make([]string, len(b))
	copy(sb, b)
	sort.Strings(sa)
	sort.Strings(sb)
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}

// Close closes the underlying docker client connection.
func (c *Client) Close() error {
	return c.cli.Close()
}
