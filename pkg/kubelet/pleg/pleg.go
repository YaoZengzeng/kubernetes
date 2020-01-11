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

package pleg

import (
	"k8s.io/apimachinery/pkg/types"
)

// PodLifeCycleEventType define the event type of pod life cycle events.
// PodLifeCycleEventType用于定义pod的生命周期事件
type PodLifeCycleEventType string

const (
	// ContainerStarted - event type when the new state of container is running.
	ContainerStarted PodLifeCycleEventType = "ContainerStarted"
	// ContainerDied - event type when the new state of container is exited.
	ContainerDied PodLifeCycleEventType = "ContainerDied"
	// ContainerRemoved - event type when the old state of container is exited.
	ContainerRemoved PodLifeCycleEventType = "ContainerRemoved"
	// PodSync is used to trigger syncing of a pod when the observed change of
	// the state of the pod cannot be captured by any single event above.
	// PodSync用来触发对于pod的同步，当观察到的pod的状态更新不能被任何上面的单个事件能够描述的时候
	PodSync PodLifeCycleEventType = "PodSync"
	// ContainerChanged - event type when the new state of container is unknown.
	// 当容器的新状态为unkown的时候
	ContainerChanged PodLifeCycleEventType = "ContainerChanged"
)

// PodLifecycleEvent is an event that reflects the change of the pod state.
// PodLifecycleEvent是一个反映pod状态变更的事件
type PodLifecycleEvent struct {
	// The pod ID.
	ID types.UID
	// The type of the event.
	Type PodLifeCycleEventType
	// The accompanied data which varies based on the event type.
	//   - ContainerStarted/ContainerStopped: the container name (string).
	//   - All other event types: unused.
	// 只对ContainerStarted/ContainerStopped有用，用于存放container，其他事件类型并不使用
	Data interface{}
}

// PodLifecycleEventGenerator contains functions for generating pod life cycle events.
// PodLifecycleEventGenerator包含了产生pod life cycle events的功能
type PodLifecycleEventGenerator interface {
	Start()
	// 通过Watch()获取channel，再通过channel获取事件
	Watch() chan *PodLifecycleEvent
	Healthy() (bool, error)
}
