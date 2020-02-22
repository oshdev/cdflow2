package official

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/mergermarket/cdflow2/docker"
	"github.com/mergermarket/cdflow2/util"
)

// Client is a concrete inplementation of our docker interface that uses the official client library.
type Client struct {
	client  *client.Client
	context context.Context
}

// NewClient creates and returns a new client.
func NewClient() (*Client, error) {
	client, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Client{
		client:  client,
		context: context.Background(),
	}, nil
}

// Run runs a container (much like `docker run` in the cli).
func (dockerClient *Client) Run(options *docker.RunOptions) error {
	stdin := false
	if options.InputStream != nil {
		stdin = true
	}
	response, err := dockerClient.client.ContainerCreate(
		dockerClient.context,
		&container.Config{
			Image:        options.Image,
			OpenStdin:    stdin,
			AttachStdin:  stdin,
			AttachStdout: true,
			AttachStderr: true,
			WorkingDir:   options.WorkingDir,
			Entrypoint:   options.Entrypoint,
			Cmd:          options.Cmd,
			Env:          options.Env,
		},
		&container.HostConfig{
			LogConfig: container.LogConfig{Type: "none"},
			Binds:     options.Binds,
			Init:      &options.Init,
		},
		nil,
		util.RandomName(options.NamePrefix),
	)
	if err != nil {
		return err
	}

	if err := dockerClient.await(response.ID, options.InputStream, options.OutputStream, options.ErrorStream, options.Started); err != nil {
		return err
	}

	result, err := dockerClient.client.ContainerInspect(dockerClient.context, response.ID)
	if err != nil {
		return err
	}

	if result.State.Running {
		log.Panicln("unexpected container still running:", result)
	}

	if result.State.ExitCode != options.SuccessStatus {
		return fmt.Errorf("container %s exited with unsuccessful exit code %d", result.ID, result.State.ExitCode)
	}

	if options.BeforeRemove != nil {
		if err := options.BeforeRemove(response.ID); err != nil {
			return fmt.Errorf("error in BeforeRemove function for container: %w", err)
		}
	}

	return dockerClient.client.ContainerRemove(dockerClient.context, response.ID, types.ContainerRemoveOptions{})
}

func (dockerClient *Client) await(container string, inputStream io.ReadCloser, outputStream, errorStream io.Writer, started chan string) error {
	stdin := false
	if inputStream != nil {
		stdin = true
	}
	hijackedResponse, err := dockerClient.client.ContainerAttach(dockerClient.context, container, types.ContainerAttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: true,
		Stdin:  stdin,
	})
	if err != nil {
		return err
	}

	return dockerClient.streamHijackedResponse(hijackedResponse, inputStream, outputStream, errorStream, func() error {
		if err := dockerClient.client.ContainerStart(
			dockerClient.context,
			container,
			types.ContainerStartOptions{},
		); err != nil {
			return err
		}
		if started != nil {
			started <- container
		}
		return nil
	})
}

func (dockerClient *Client) streamHijackedResponse(hijackedResponse types.HijackedResponse, inputStream io.ReadCloser, outputStream, errorStream io.Writer, start func() error) error {
	if inputStream != nil {
		go func() {
			defer inputStream.Close()
			defer hijackedResponse.CloseWrite()
			io.Copy(hijackedResponse.Conn, inputStream)
		}()
	}
	outputDone := make(chan error, 1)
	defer close(outputDone)
	go func() {
		defer hijackedResponse.Close()
		_, err := stdcopy.StdCopy(outputStream, errorStream, hijackedResponse.Reader)
		outputDone <- err
	}()

	if err := start(); err != nil {
		return err
	}

	for {
		select {
		case err := <-outputDone:
			return err
		case <-dockerClient.context.Done():
			return dockerClient.context.Err()
		}
	}
}

// EnsureImage pulls an image if it does not exist locally.
func (dockerClient *Client) EnsureImage(image string, outputStream io.Writer) error {
	// TODO bit lax, this should check the error type
	if _, _, err := dockerClient.client.ImageInspectWithRaw(
		dockerClient.context,
		image,
	); err == nil {
		return nil
	}
	return dockerClient.PullImage(image, outputStream)
}

// PullImage pulls and image.
func (dockerClient *Client) PullImage(image string, outputStream io.Writer) error {
	reader, err := dockerClient.client.ImagePull(
		dockerClient.context,
		image,
		types.ImagePullOptions{},
	)
	if err != nil {
		return err
	}
	_, err = io.Copy(outputStream, reader)
	return err
}

// GetImageRepoDigests inspects an image and pulls out the repo digests.
func (dockerClient *Client) GetImageRepoDigests(image string) ([]string, error) {
	details, _, err := dockerClient.client.ImageInspectWithRaw(dockerClient.context, image)
	if err != nil {
		return nil, err
	}
	return details.RepoDigests, nil
}

// Exec execs a process in a docker container (like `docker exec` in the cli).
func (dockerClient *Client) Exec(options *docker.ExecOptions) error {
	exec, err := dockerClient.client.ContainerExecCreate(
		dockerClient.context,
		options.ID,
		types.ExecConfig{
			AttachStdout: true,
			AttachStderr: true,
			Cmd:          options.Cmd,
		},
	)
	if err != nil {
		return fmt.Errorf("error creating docker exec: %w", err)
	}

	attachResponse, err := dockerClient.client.ContainerExecAttach(
		dockerClient.context,
		exec.ID,
		types.ExecStartCheck{},
	)
	if err != nil {
		return fmt.Errorf("error attaching to docker exec: %w", err)
	}
	defer attachResponse.Close()

	if err := dockerClient.streamHijackedResponse(attachResponse, nil, options.OutputStream, options.ErrorStream, func() error {
		return dockerClient.client.ContainerExecStart(
			dockerClient.context,
			exec.ID,
			types.ExecStartCheck{},
		)
	}); err != nil {
		return fmt.Errorf("error streaming data from exec: %w", err)
	}

	details, err := dockerClient.client.ContainerExecInspect(
		dockerClient.context,
		exec.ID,
	)
	if err != nil {
		return fmt.Errorf("error inspecting exec: %w", err)
	}

	if details.ExitCode != 0 {
		return errors.New("exec process exited with error status code " + string(details.ExitCode))
	}

	return nil
}

// Stop stops a container.
func (dockerClient *Client) Stop(id string, timeout time.Duration) error {
	return dockerClient.client.ContainerStop(dockerClient.context, id, &timeout)
}

// CreateVolume creates a docker volume and returns its ID.
func (dockerClient *Client) CreateVolume() (string, error) {
	volume, err := dockerClient.client.VolumeCreate(dockerClient.context, volume.VolumeCreateBody{})
	if err != nil {
		return "", err
	}
	return volume.Name, nil
}

// RemoveVolume removes a docker volume given its ID.
func (dockerClient *Client) RemoveVolume(id string) error {
	return dockerClient.client.VolumeRemove(dockerClient.context, id, false)
}

// CreateContainer creates a docker container.
func (dockerClient *Client) CreateContainer(options *docker.CreateContainerOptions) (string, error) {
	container, err := dockerClient.client.ContainerCreate(
		dockerClient.context,
		&container.Config{
			Image: options.Image,
		},
		&container.HostConfig{
			Binds: options.Binds,
		},
		nil,
		"",
	)
	if err != nil {
		return "", err
	}
	return container.ID, nil
}

// RemoveContainer removes a docker container.
func (dockerClient *Client) RemoveContainer(id string) error {
	return dockerClient.client.ContainerRemove(dockerClient.context, id, types.ContainerRemoveOptions{})
}

// CopyFromContainer returns a tar stream for a path within a container (like `docker cp CONTAINER -`).
func (dockerClient *Client) CopyFromContainer(id string, path string) (io.ReadCloser, error) {
	reader, _, err := dockerClient.client.CopyFromContainer(dockerClient.context, id, path)
	return reader, err
}

// CopyToContainer takes a tar stream and copies it into the container.
func (dockerClient *Client) CopyToContainer(id string, path string, reader io.Reader) error {
	return dockerClient.client.CopyToContainer(dockerClient.context, id, path, reader, types.CopyToContainerOptions{})
}
