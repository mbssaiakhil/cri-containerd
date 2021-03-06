/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package server

import (
	"fmt"
	"os"

	"github.com/containerd/containerd/api/services/events/v1"
	"github.com/containerd/containerd/api/services/tasks/v1"
	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/typeurl"
	"github.com/golang/glog"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"
	"k8s.io/kubernetes/pkg/kubelet/apis/cri/v1alpha1/runtime"
)

// StopPodSandbox stops the sandbox. If there are any running containers in the
// sandbox, they should be forcibly terminated.
func (c *criContainerdService) StopPodSandbox(ctx context.Context, r *runtime.StopPodSandboxRequest) (retRes *runtime.StopPodSandboxResponse, retErr error) {
	glog.V(2).Infof("StopPodSandbox for sandbox %q", r.GetPodSandboxId())
	defer func() {
		if retErr == nil {
			glog.V(2).Infof("StopPodSandbox %q returns successfully", r.GetPodSandboxId())
		}
	}()

	sandbox, err := c.sandboxStore.Get(r.GetPodSandboxId())
	if err != nil {
		return nil, fmt.Errorf("an error occurred when try to find sandbox %q: %v",
			r.GetPodSandboxId(), err)
	}
	// Use the full sandbox id.
	id := sandbox.ID

	// Stop all containers inside the sandbox. This terminates the container forcibly,
	// and container may still be so production should not rely on this behavior.
	// TODO(random-liu): Delete the sandbox container before this after permanent network namespace
	// is introduced, so that no container will be started after that.
	containers := c.containerStore.List()
	for _, container := range containers {
		if container.SandboxID != id {
			continue
		}
		// Forcibly stop the container. Do not use `StopContainer`, because it introduces a race
		// if a container is removed after list.
		if err = c.stopContainer(ctx, container, 0); err != nil {
			return nil, fmt.Errorf("failed to stop container %q: %v", container.ID, err)
		}
	}

	// Teardown network for sandbox.
	_, err = c.os.Stat(sandbox.NetNS)
	if err == nil {
		if !sandbox.Config.GetLinux().GetSecurityContext().GetNamespaceOptions().GetHostNetwork() {
			if teardownErr := c.netPlugin.TearDownPod(sandbox.NetNS, sandbox.Config.GetMetadata().GetNamespace(),
				sandbox.Config.GetMetadata().GetName(), id); teardownErr != nil {
				return nil, fmt.Errorf("failed to destroy network for sandbox %q: %v", id, teardownErr)
			}
		}
	} else if !os.IsNotExist(err) { // It's ok for sandbox.NetNS to *not* exist
		return nil, fmt.Errorf("failed to stat netns path for sandbox %q before tearing down the network: %v", id, err)
	}
	glog.V(2).Infof("TearDown network for sandbox %q successfully", id)

	sandboxRoot := getSandboxRootDir(c.rootDir, id)
	if err := c.unmountSandboxFiles(sandboxRoot, sandbox.Config); err != nil {
		return nil, fmt.Errorf("failed to unmount sandbox files in %q: %v", sandboxRoot, err)
	}

	if err := c.stopSandboxContainer(ctx, id); err != nil {
		return nil, fmt.Errorf("failed to stop sandbox container %q: %v", id, err)
	}
	return &runtime.StopPodSandboxResponse{}, nil
}

// stopSandboxContainer kills and deletes sandbox container.
func (c *criContainerdService) stopSandboxContainer(ctx context.Context, id string) error {
	cancellable, cancel := context.WithCancel(ctx)
	eventstream, err := c.eventService.Subscribe(cancellable, &events.SubscribeRequest{})
	if err != nil {
		return fmt.Errorf("failed to get containerd event: %v", err)
	}
	defer cancel()

	resp, err := c.taskService.Get(ctx, &tasks.GetTaskRequest{ContainerID: id})
	if err != nil {
		if isContainerdGRPCNotFoundError(err) {
			return nil
		}
		return fmt.Errorf("failed to get sandbox container: %v", err)
	}
	if resp.Task.Status != task.StatusStopped {
		// TODO(random-liu): [P1] Handle sandbox container graceful deletion.
		if _, err := c.taskService.Kill(ctx, &tasks.KillRequest{
			ContainerID: id,
			Signal:      uint32(unix.SIGKILL),
			All:         true,
		}); err != nil && !isContainerdGRPCNotFoundError(err) && !isRuncProcessAlreadyFinishedError(err) {
			return fmt.Errorf("failed to kill sandbox container: %v", err)
		}

		if err := c.waitSandboxContainer(eventstream, id, resp.Task.Pid); err != nil {
			return fmt.Errorf("failed to wait for pod sandbox to stop: %v", err)
		}
	}

	// Delete the sandbox container from containerd.
	_, err = c.taskService.Delete(ctx, &tasks.DeleteTaskRequest{ContainerID: id})
	if err != nil && !isContainerdGRPCNotFoundError(err) {
		return fmt.Errorf("failed to delete sandbox container: %v", err)
	}
	return nil
}

// waitSandboxContainer wait sandbox container stop event.
func (c *criContainerdService) waitSandboxContainer(eventstream events.Events_SubscribeClient, id string, pid uint32) error {
	for {
		evt, err := eventstream.Recv()
		if err != nil {
			return err
		}
		// Continue until the event received is of type task exit.
		if !typeurl.Is(evt.Event, &events.TaskExit{}) {
			continue
		}
		any, err := typeurl.UnmarshalAny(evt.Event)
		if err != nil {
			return err
		}
		e := any.(*events.TaskExit)
		if e.ContainerID == id && e.Pid == pid {
			return nil
		}
	}
}
