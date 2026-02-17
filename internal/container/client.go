package container

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
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
	hostConfig := &container.HostConfig{
		Binds: opts.Mounts,
		Resources: container.Resources{
			Memory:   opts.Memory,
			NanoCPUs: opts.NanoCPUs,
		},
		RestartPolicy: container.RestartPolicy{
			Name: container.RestartPolicyAlways,
		},
	}

	var networkingConfig *network.NetworkingConfig
	if opts.Network != "" {
		networkingConfig = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				opts.Network: {},
			},
		}
	}

	resp, err := c.cli.ContainerCreate(ctx, &container.Config{
		Image: image,
		Env:   opts.Env,
	}, hostConfig, networkingConfig, nil, name)
	if err != nil {
		return fmt.Errorf("failed to create container %s: %w", name, err)
	}

	if err := c.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start container %s: %w", name, err)
	}

	fmt.Printf("Container %s started with ID %s\n", name, resp.ID)
	return nil
}

// StopContainer stops and removes a container by name.
func (c *Client) StopContainer(ctx context.Context, name string) error {
	// check if container exists
	containers, err := c.cli.ContainerList(ctx, container.ListOptions{All: true})
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
	if err := c.cli.ContainerStop(ctx, containerID, container.StopOptions{}); err != nil {
		return fmt.Errorf("failed to stop container %s: %w", name, err)
	}

	fmt.Printf("Removing container %s...\n", name)
	if err := c.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{}); err != nil {
		return fmt.Errorf("failed to remove container %s: %w", name, err)
	}

	return nil
}

// Logs returns the logs of a container.
func (c *Client) Logs(ctx context.Context, name string) error {
	// simple implementation to just dump logs to stdout
	out, err := c.cli.ContainerLogs(ctx, name, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return err
	}
	defer out.Close()
	stdcopy.StdCopy(os.Stdout, os.Stderr, out)
	return nil
}

// Close closes the underlying docker client connection.
func (c *Client) Close() error {
	return c.cli.Close()
}
