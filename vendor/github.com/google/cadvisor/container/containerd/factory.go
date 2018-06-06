// Copyright 2017 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package containerd

import (
	"flag"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/golang/glog"
	"golang.org/x/net/context"

	"github.com/google/cadvisor/container"
	"github.com/google/cadvisor/container/libcontainer"
	"github.com/google/cadvisor/fs"
	info "github.com/google/cadvisor/info/v1"
	"github.com/google/cadvisor/manager/watcher"
)

var ArgContainerdEndpoint = flag.String("containerd", "unix:///var/run/containerd.sock", "containerd endpoint")

// The namespace under which containerd aliases are unique.
const k8sContainerdNamespace = "containerd"

// Regexp that identifies containerd cgroups, containers started with
// --cgroup-parent have another prefix than 'containerd'
// 用于区分containerd cgroups的Regexp
var containerdCgroupRegexp = regexp.MustCompile(`([a-z0-9]{64})`)

type containerdFactory struct {
	machineInfoFactory info.MachineInfoFactory
	client             containerdClient
	version            string
	// Information about the mounted cgroup subsystems.
	// 那些挂载的cgroup子系统的信息
	cgroupSubsystems libcontainer.CgroupSubsystems
	// Information about mounted filesystems.
	// 挂载的文件系统信息
	fsInfo        fs.FsInfo
	ignoreMetrics container.MetricSet
}

func (self *containerdFactory) String() string {
	return k8sContainerdNamespace
}

func (self *containerdFactory) NewContainerHandler(name string, inHostNamespace bool) (handler container.ContainerHandler, err error) {
	client, err := Client()
	if err != nil {
		return
	}

	metadataEnvs := []string{}
	return newContainerdContainerHandler(
		client,
		name,
		self.machineInfoFactory,
		self.fsInfo,
		&self.cgroupSubsystems,
		inHostNamespace,
		metadataEnvs,
		self.ignoreMetrics,
	)
}

// Returns the containerd ID from the full container name.
// 从完整的container name中返回containerd ID
func ContainerNameToContainerdID(name string) string {
	id := path.Base(name)
	if matches := containerdCgroupRegexp.FindStringSubmatch(id); matches != nil {
		return matches[1]
	}
	return id
}

// isContainerName returns true if the cgroup with associated name
// corresponds to a containerd container.
// isContainerName返回true，如果和这个名字相关的容器代表了一个containerd container
func isContainerName(name string) bool {
	// TODO: May be check with HasPrefix ContainerdNamespace
	if strings.HasSuffix(name, ".mount") {
		return false
	}
	return containerdCgroupRegexp.MatchString(path.Base(name))
}

// Containerd can handle and accept all containerd created containers
// Containerd能够处理并接受所有containerd创建的容器
func (self *containerdFactory) CanHandleAndAccept(name string) (bool, bool, error) {
	// if the container is not associated with containerd, we can't handle it or accept it.
	if !isContainerName(name) {
		return false, false, nil
	}
	// Check if the container is known to containerd and it is running.
	id := ContainerNameToContainerdID(name)
	// If container and task lookup in containerd fails then we assume
	// that the container state is not known to containerd
	// 如果在containerd中寻找container或者task失败
	// 那么我们假设container state对于containerd是为止的
	ctx := context.Background()
	// 调用contaienrd的client，加载container，验证该容器containerd是否知道，并且处于running状态
	_, err := self.client.LoadContainer(ctx, id)
	if err != nil {
		return false, false, fmt.Errorf("failed to load container: %v", err)
	}

	return true, true, nil
}

func (self *containerdFactory) DebugInfo() map[string][]string {
	return map[string][]string{}
}

// Register root container before running this function!
func Register(factory info.MachineInfoFactory, fsInfo fs.FsInfo, ignoreMetrics container.MetricSet) error {
	// 创建containerd的client
	client, err := Client()
	if err != nil {
		return fmt.Errorf("unable to create containerd client: %v", err)
	}

	// 获取containerd的version
	containerdVersion, err := client.Version(context.Background())
	if err != nil {
		return fmt.Errorf("failed to fetch containerd client version: %v", err)
	}

	// 获取cgroup子系统的信息
	cgroupSubsystems, err := libcontainer.GetCgroupSubsystems()
	if err != nil {
		return fmt.Errorf("failed to get cgroup subsystems: %v", err)
	}

	glog.V(1).Infof("Registering containerd factory")
	f := &containerdFactory{
		cgroupSubsystems:   cgroupSubsystems,
		client:             client,
		fsInfo:             fsInfo,
		machineInfoFactory: factory,
		version:            containerdVersion,
		ignoreMetrics:      ignoreMetrics,
	}

	// containerd只监听raw，将containerd加入raw对应的factories
	container.RegisterContainerHandlerFactory(f, []watcher.ContainerWatchSource{watcher.Raw})
	return nil
}
