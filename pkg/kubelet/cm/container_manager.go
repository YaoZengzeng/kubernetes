/*
Copyright 2015 The Kubernetes Authors.

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

package cm

import (
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	// TODO: Migrate kubelet to either use its own internal objects or client library.
	"k8s.io/api/core/v1"
	internalapi "k8s.io/kubernetes/pkg/kubelet/apis/cri"
	"k8s.io/kubernetes/pkg/kubelet/config"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/status"
	"k8s.io/kubernetes/plugin/pkg/scheduler/schedulercache"

	"fmt"
	"strconv"
	"strings"
)

type ActivePodsFunc func() []*v1.Pod

// Manages the containers running on a machine.
// 管理运行在一台机器上的容器
type ContainerManager interface {
	// Runs the container manager's housekeeping.
	// - Ensures that the Docker daemon is in a container.
	// - Creates the system container where all non-containerized processes run.
	// 运行container manager的housekeeping
	// - 确保Docker daemon在一个容器中
	// - 创建system container，所有的non-containerized进程都运行在里面
	Start(*v1.Node, ActivePodsFunc, config.SourcesReady, status.PodStatusProvider, internalapi.RuntimeService) error

	// SystemCgroupsLimit returns resources allocated to system cgroups in the machine.
	// These cgroups include the system and Kubernetes services.
	// SystemCgroupsLimit返回机器上给system cgroups分配的资源
	// 这些cgroups包括system以及Kubernetes services
	SystemCgroupsLimit() v1.ResourceList

	// GetNodeConfig returns a NodeConfig that is being used by the container manager.
	// GetNodeConfig返回container manager使用的NodeConfig
	GetNodeConfig() NodeConfig

	// Status returns internal Status.
	Status() Status

	// NewPodContainerManager is a factory method which returns a podContainerManager object
	// Returns a noop implementation if qos cgroup hierarchy is not enabled
	// NewPodContainerManager是一个工厂方法，它会返回一个podContainerManager对象
	// 如果qos cgroup hierarchy不是使能的话，就会返回一个noop implementation
	NewPodContainerManager() PodContainerManager

	// GetMountedSubsystems returns the mounted cgroup subsystems on the node
	// GetMountedSubsystems返回在node中挂载的cgroup子系统
	GetMountedSubsystems() *CgroupSubsystems

	// GetQOSContainersInfo returns the names of top level QoS containers
	// GetQOSContainersInfo返回顶层的QoS容器的名字
	GetQOSContainersInfo() QOSContainersInfo

	// GetNodeAllocatableReservation returns the amount of compute resources that have to be reserved from scheduling.
	// GetNodeAllocatableReservation返回在调度时需要保留的计算资源的数量
	GetNodeAllocatableReservation() v1.ResourceList

	// GetCapacity returns the amount of compute resources tracked by container manager available on the node.
	// GetCapacity返回container manager追踪的本节点的计算资源可以获取的数目
	GetCapacity() v1.ResourceList

	// GetDevicePluginResourceCapacity returns the amount of device plugin resources available on the node
	// and inactive device plugin resources previously registered on the node.
	GetDevicePluginResourceCapacity() (v1.ResourceList, []string)

	// UpdateQOSCgroups performs housekeeping updates to ensure that the top
	// level QoS containers have their desired state in a thread-safe way
	// UpdateQOSCgroups会进行housekeeping udpates从而确保top level Qos Container
	// 达到预期的状态
	UpdateQOSCgroups() error

	// GetResources returns RunContainerOptions with devices, mounts, and env fields populated for
	// extended resources required by container.
	GetResources(pod *v1.Pod, container *v1.Container) (*kubecontainer.RunContainerOptions, error)

	// UpdatePluginResources calls Allocate of device plugin handler for potential
	// requests for device plugin resources, and returns an error if fails.
	// Otherwise, it updates allocatableResource in nodeInfo if necessary,
	// to make sure it is at least equal to the pod's requested capacity for
	// any registered device plugin resource
	UpdatePluginResources(*schedulercache.NodeInfo, *lifecycle.PodAdmitAttributes) error

	InternalContainerLifecycle() InternalContainerLifecycle
}

type NodeConfig struct {
	RuntimeCgroupsName    string
	SystemCgroupsName     string
	KubeletCgroupsName    string
	ContainerRuntime      string
	CgroupsPerQOS         bool
	CgroupRoot            string
	CgroupDriver          string
	KubeletRootDir        string
	ProtectKernelDefaults bool
	NodeAllocatableConfig
	ExperimentalQOSReserved               map[v1.ResourceName]int64
	ExperimentalCPUManagerPolicy          string
	ExperimentalCPUManagerReconcilePeriod time.Duration
}

type NodeAllocatableConfig struct {
	KubeReservedCgroupName   string
	SystemReservedCgroupName string
	EnforceNodeAllocatable   sets.String
	KubeReserved             v1.ResourceList
	SystemReserved           v1.ResourceList
	HardEvictionThresholds   []evictionapi.Threshold
}

type Status struct {
	// Any soft requirements that were unsatisfied.
	SoftRequirements error
}

const (
	// Uer visible keys for managing node allocatable enforcement on the node.
	NodeAllocatableEnforcementKey = "pods"
	SystemReservedEnforcementKey  = "system-reserved"
	KubeReservedEnforcementKey    = "kube-reserved"
)

// containerManager for the kubelet is currently an injected dependency.
// We need to parse the --qos-reserve-requests option in
// cmd/kubelet/app/server.go and there isn't really a good place to put
// the code.  If/When the kubelet dependency injection gets worked out,
// maybe there will be a better place for it.
func parsePercentage(v string) (int64, error) {
	if !strings.HasSuffix(v, "%") {
		return 0, fmt.Errorf("percentage expected, got '%s'", v)
	}
	percentage, err := strconv.ParseInt(strings.TrimRight(v, "%"), 10, 0)
	if err != nil {
		return 0, fmt.Errorf("invalid number in percentage '%s'", v)
	}
	if percentage < 0 || percentage > 100 {
		return 0, fmt.Errorf("percentage must be between 0 and 100")
	}
	return percentage, nil
}

// ParseQOSReserved parses the --qos-reserve-requests option
func ParseQOSReserved(m map[string]string) (*map[v1.ResourceName]int64, error) {
	reservations := make(map[v1.ResourceName]int64)
	for k, v := range m {
		switch v1.ResourceName(k) {
		// Only memory resources are supported.
		case v1.ResourceMemory:
			q, err := parsePercentage(v)
			if err != nil {
				return nil, err
			}
			reservations[v1.ResourceName(k)] = q
		default:
			return nil, fmt.Errorf("cannot reserve %q resource", k)
		}
	}
	return &reservations, nil
}
