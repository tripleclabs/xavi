package container

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

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
	cli             *client.Client
	registryAuth    string
	registryAddress string
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

	// Store encoded auth for subsequent pulls
	encodedJSON, err := json.Marshal(authConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal auth config: %w", err)
	}
	c.registryAuth = base64.StdEncoding.EncodeToString(encodedJSON)
	c.registryAddress = registryAddr

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
		pullAuth := ""
		if c.registryAddress != "" && strings.HasPrefix(image, c.registryAddress+"/") {
			pullAuth = c.registryAuth
		}
		reader, err := c.cli.ImagePull(ctx, image, types.ImagePullOptions{
			RegistryAuth: pullAuth,
		})
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
		hostPort, containerPort, ok := strings.Cut(p, ":")
		if ok {
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
	// Docker normalizes short names (e.g. "nginx:latest" -> "docker.io/library/nginx:latest",
	// "wearecococo/caddy:2.10.2" -> "docker.io/wearecococo/caddy:2.10.2")
	if !imagesMatch(opts.Image, inspect.Config.Image) && !imagesMatch(opts.Image, inspect.Image) {
		fmt.Printf("  [Divergence] Image mismatch: desired=%s, actual_config=%s, actual_id=%s\n", opts.Image, inspect.Config.Image, inspect.Image)
		return false
	}

	// 2. Command check
	if len(opts.Cmd) > 0 {
		if len(inspect.Config.Cmd) != len(opts.Cmd) {
			fmt.Printf("  [Divergence] Command length mismatch: desired=%v, actual=%v\n", opts.Cmd, inspect.Config.Cmd)
			return false
		}
		for i := range opts.Cmd {
			if inspect.Config.Cmd[i] != opts.Cmd[i] {
				fmt.Printf("  [Divergence] Command mismatch at index %d: desired=%s, actual=%s\n", i, opts.Cmd[i], inspect.Config.Cmd[i])
				return false
			}
		}
	} else {
		// Desired is empty (use default).
		// If actual is ["postgres"] and we are using a postgres image, it's likely a match.
		// For now, let's just skip the check if desired is empty to be safe against image defaults.
	}

	// 3. Env check
	if !compareEnv(inspect.Config.Env, opts.Env) {
		fmt.Printf("  [Divergence] Env mismatch: desired=%v, actual=%v\n", opts.Env, inspect.Config.Env)
		return false
	}

	// 4. Mounts check (Binds)
	// Docker often suffixes mounts with :rw, let's normalize
	normalizedActual := make([]string, len(inspect.HostConfig.Binds))
	for i, b := range inspect.HostConfig.Binds {
		normalizedActual[i] = strings.TrimSuffix(b, ":rw")
	}
	if !compareSlices(normalizedActual, opts.Mounts) {
		fmt.Printf("  [Divergence] Mounts mismatch: desired=%v, actual_normalized=%v\n", opts.Mounts, normalizedActual)
		return false
	}

	// 5. Ports check
	if len(inspect.HostConfig.PortBindings) != len(hostConfig.PortBindings) {
		fmt.Printf("  [Divergence] Port count mismatch: desired_count=%d, actual_count=%d\n", len(hostConfig.PortBindings), len(inspect.HostConfig.PortBindings))
		return false
	}
	for k, v := range hostConfig.PortBindings {
		existing, ok := inspect.HostConfig.PortBindings[k]
		if !ok {
			fmt.Printf("  [Divergence] Port %s missing in actual container\n", k)
			return false
		}
		if len(existing) != len(v) {
			fmt.Printf("  [Divergence] Port %s binding count mismatch\n", k)
			return false
		}
		for i := range v {
			// Docker might return "0.0.0.0" even if we didn't specify it, or vice versa
			actualIP := existing[i].HostIP
			if actualIP == "" {
				actualIP = "0.0.0.0"
			}
			desiredIP := v[i].HostIP
			if desiredIP == "" {
				desiredIP = "0.0.0.0"
			}

			if actualIP != desiredIP || existing[i].HostPort != v[i].HostPort {
				fmt.Printf("  [Divergence] Port %s binding mismatch: desired=%s:%s, actual=%s:%s\n", k, desiredIP, v[i].HostPort, actualIP, existing[i].HostPort)
				return false
			}
		}
	}

	return true
}

// normalizeImageName expands a short Docker image reference to its fully qualified form.
func normalizeImageName(image string) string {
	// Strip tag/digest to count path components
	ref := strings.Split(image, ":")[0]
	ref = strings.Split(ref, "@")[0]

	parts := strings.Split(ref, "/")
	switch len(parts) {
	case 1:
		// e.g. "postgres" -> "docker.io/library/postgres"
		return "docker.io/library/" + image
	case 2:
		// e.g. "wearecococo/caddy" -> "docker.io/wearecococo/caddy"
		if !strings.Contains(parts[0], ".") {
			return "docker.io/" + image
		}
		return image
	default:
		return image
	}
}

func imagesMatch(a, b string) bool {
	return normalizeImageName(a) == normalizeImageName(b)
}

func compareEnv(actual, desired []string) bool {
	// Docker adds many default env vars (PATH, HOSTNAME, etc.)
	// We only care if our DESIRED env vars are present and correct.
	for _, d := range desired {
		found := false
		for _, a := range actual {
			if a == d {
				found = true
				break
			}
		}
		if !found {
			return false
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
