package main

import (
	"context"
	"fmt"
	"log"
	"flag"
	"os"
	"os/signal"
	"os/exec"
	"strings"


	"github.com/docker/docker/client"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"

)

// getContainerIDByName fetches the container ID given its name
func getContainerIDByName(cli *client.Client, ctx context.Context, containerName string) (string, error) {
	containers, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return "", err
	}

	for _, c := range containers {
		for _, name := range c.Names {
			if name == "/"+containerName { // Docker container names have a leading slash
				return c.ID, nil
			}
		}
	}

	return "", fmt.Errorf("container with name %s not found", containerName)
}

func waitForContainerStop(cli *client.Client, ctx context.Context, containerID string) {
	waitBodyCh, errCh := cli.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)

	select {
	case waitBody := <-waitBodyCh:
		if waitBody.StatusCode == 0 {
			fmt.Println("Container stopped")
		} else {
			fmt.Printf("Container stopped with status code: %d\n", waitBody.StatusCode)
		}
	case err := <-errCh:
		fmt.Printf("Error waiting for container to stop: %v\n", err)
	}
}

func recreateOriginalContainer(ctx context.Context, cli *client.Client, containerJSON *types.ContainerJSON, networkingConfig *network.NetworkingConfig) {
	// Start the original container
	resp, err := cli.ContainerCreate(ctx, containerJSON.Config, containerJSON.HostConfig, networkingConfig, nil, containerJSON.Name)
	if err  != nil {
		log.Fatalf("Error creating container: %v", err)
	}

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		log.Fatalf("Error starting container: %v", err)
	}

	fmt.Printf("Original container %s started successfully\n", containerJSON.Name)
}

func cleanupDevContainer(ctx context.Context, cli *client.Client, resp *container.CreateResponse) {
	c, err := cli.ContainerInspect(context.Background(), resp.ID)
	if err!= nil {
		panic(err)
	}

	if c.State.Running {
		timeout := 0
		cli.ContainerStop(context.Background(), resp.ID, container.StopOptions{Timeout: &timeout})
		waitForContainerStop(cli, ctx, resp.ID)
	}

	if err := cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{}); err != nil {
		log.Fatalf("Error removing container: %v", err)
	}

	println("Container removed successfully")
}

func removeOriginalContainer(ctx context.Context, cli *client.Client, containerID string) {
	println("Stopping original container")

	// Stop the running container
	timeout := 0
	if err := cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeout}); err != nil {
		log.Fatalf("Error stopping container: %v", err)
	}

	waitForContainerStop(cli, ctx, containerID)

	if err := cli.ContainerRemove(ctx, containerID, container.RemoveOptions{}); err != nil {
		log.Fatalf("Error removing container: %v", err)
	}
}

func startDevContainer(ctx context.Context, cli *client.Client, sigCh chan os.Signal, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, name string) *container.CreateResponse {
	// Create the new container
	resp, err := cli.ContainerCreate(ctx, config, hostConfig, networkingConfig, nil, name)
	if err != nil {
		log.Fatalf("Error creating container: %v", err)
	}

	// Attach to the container by executing docker attach
	cmd := exec.Command("docker", "attach", resp.ID)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	go func () {
		err = cmd.Run()
		if err != nil {
			log.Fatalf("Error attaching to container: %v", err)
		}

		sigCh <- os.Interrupt
	}()

	println("Quit the container to restore the original one")

	// Start the new container on an interactive shell
	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		log.Fatalf("Error starting container: %v", err)
	}

	return &resp
}

func cloneRemoteRepo(remote, branch string) string {
	repoURL := remote
	repoName := repoURL[strings.LastIndex(repoURL, "/")+1:strings.LastIndex(repoURL, ".")]
	targetDir := "/tmp" + "/" + repoName

	// if targetDir already exists, remove it
	if _, err := os.Stat(targetDir); err == nil {
		os.RemoveAll(targetDir)
	}

	cloneArgs := []string{"clone", repoURL, targetDir}
	if branch != "" {
		cloneArgs = append(cloneArgs, "-b", branch, "--single-branch")
	}

	cmd := exec.Command("git", cloneArgs...)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err!= nil {
		log.Fatalf("Failed to clone repository: %v", err)
	}

	return targetDir
}

func devContainerConfiguration(containerJSON *types.ContainerJSON, newImage, sourcePath, targetPath string) (*container.Config, *container.HostConfig, *network.NetworkingConfig) {
	// Create new container configuration
	config := *containerJSON.Config
	config.Image = newImage
	config.WorkingDir = targetPath
	config.Cmd = []string{}
	config.Entrypoint = []string{ "/bin/sh" }

	// get PATH from original container Env
	path := ""
	for _, env := range config.Env {
		if env[:4] == "PATH" {
			path = env
			break
		}
	}
	path = path + ":/go/bin:/usr/local/go/bin"
	config.Env = append(config.Env, path)
	config.Env = append(config.Env, "DEV_CONTAINER=true")

	config.AttachStdin = true
	config.AttachStdout = true
	config.AttachStderr = true
	config.Tty = true
	config.OpenStdin = true
	config.StdinOnce = true

	hostConfig := *containerJSON.HostConfig
	hostConfig.Mounts = []mount.Mount{
		{
			Type:   mount.TypeBind,
			Source: sourcePath,
			Target: targetPath,
		},
		// Mount the git public key for private repositories
		{
			Type:   mount.TypeBind,
			Source: os.Getenv("HOME") + "/.ssh",
			Target: "/root/.ssh",
		},
		// Mount the git config file for private repositories
		{
			Type:   mount.TypeBind,
			Source: os.Getenv("HOME") + "/.gitconfig",
			Target: "/root/.gitconfig",
		},
		// Mount the go folder for go modules
		{
			Type:   mount.TypeBind,
			Source: os.Getenv("HOME") + "/go/pkg",
			Target: "/go/pkg",
		},
	}

	networkingConfig := network.NetworkingConfig{
		EndpointsConfig: containerJSON.NetworkSettings.Networks,
	}


	return &config, &hostConfig, &networkingConfig
}

// TODO : launch exec go version on existing container to get the current golang version
//         	and launch a container with the same version
//         check if .ait.toml in sourcePath else create a default one?
//        if container does not exist in stack create it?
//        	get env from .env?

func main() {
	// Parse command-line arguments
	containerName := flag.String("name", "", "Name of the running container")
	sourcePath := flag.String("source", "", "Source path for the new volume mount")
	targetPath := flag.String("target", "/app", "Target path for the new volume mount in the container")
	newImage := flag.String("image", "docker-dev-golang:latest", "New image for the container")
	remote := flag.String("remote", "", "Remote git repository to clone")
	branch := flag.String("branch", "master", "Branch to checkout")
	flag.Parse()
	
	if remote != nil && *remote != "" {
		*sourcePath = cloneRemoteRepo(*remote, *branch)
	}

	if *containerName == "" {
		log.Fatalf("Argument for -name is required")
	}

	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("Error creating Docker client: %v", err)
	}

	// Container ID or name of the running container
	containerID, err := getContainerIDByName(cli, ctx, *containerName)
	if err != nil {
		log.Fatalf("Error getting Docker container's id by name: %v", err)
	}

	// Fetch the current configuration of the running container
	containerJSON, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		log.Fatalf("Error inspecting container: %v", err)
	}

	removeOriginalContainer(ctx, cli, containerID)

	name := containerJSON.Name + "-dev"

	config, hostConfig, networkingConfig := devContainerConfiguration(&containerJSON, *newImage, *sourcePath, *targetPath)
	
	// Handle terminal signals for proper cleanup
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	resp := startDevContainer(ctx, cli, sigCh, config, hostConfig, networkingConfig, name)

	go func () {
		// In case the main container's process fails
		waitForContainerStop(cli, ctx, resp.ID)
		println("Container has exited")
		sigCh <- os.Interrupt
	}()

	<-sigCh
	fmt.Println("Interrupt signal received, stopping container...")

	cleanupDevContainer(ctx, cli, resp)

	recreateOriginalContainer(ctx, cli, &containerJSON, networkingConfig)
}

