// Copyright 2014 Google Inc. All Rights Reserved.
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

// Package container defines types for sub-container events and also
// defines an interface for container operation handlers.
package container

import info "github.com/google/cadvisor/info/v1"

// ListType describes whether listing should be just for a
// specific container or performed recursively.
type ListType int

const (
	ListSelf ListType = iota
	ListRecursive
)

type ContainerType int

const (
	ContainerTypeRaw ContainerType = iota
	ContainerTypeDocker
	ContainerTypeRkt
	ContainerTypeSystemd
	ContainerTypeCrio
	ContainerTypeContainerd
)

// Interface for container operation handlers.
type ContainerHandler interface {
	// Returns the ContainerReference
	ContainerReference() (info.ContainerReference, error)

	// Returns container's isolation spec.
	// 返回该容器的isolation spec
	GetSpec() (info.ContainerSpec, error)

	// Returns the current stats values of the container.
	// 返回该容器当前的stats
	GetStats() (*info.ContainerStats, error)

	// Returns the subcontainers of this container.
	// 返回该容器的subcontainers
	ListContainers(listType ListType) ([]info.ContainerReference, error)

	// Returns the processes inside this container.
	// 返回容器中的processes
	ListProcesses(listType ListType) ([]int, error)

	// Returns absolute cgroup path for the requested resource.
	// 返回指定资源的绝对cgroup路径
	GetCgroupPath(resource string) (string, error)

	// Returns container labels, if available.
	// 返回container的labels
	GetContainerLabels() map[string]string

	// Returns the container's ip address, if available
	GetContainerIPAddress() string

	// Returns whether the container still exists.
	Exists() bool

	// Cleanup frees up any resources being held like fds or go routines, etc.
	Cleanup()

	// Start starts any necessary background goroutines - must be cleaned up in Cleanup().
	// It is expected that most implementations will be a no-op.
	// 大多数实现的Start()函数都是空操作
	Start()

	// Type of handler
	// Container的类型，包括ContainerTypeRaw, ContainerTypeContainerd等等
	Type() ContainerType
}
