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

type PodLifeCycleEventType string

const (
	ContainerStarted PodLifeCycleEventType = "ContainerStarted"
	ContainerDied    PodLifeCycleEventType = "ContainerDied"
	ContainerRemoved PodLifeCycleEventType = "ContainerRemoved"
	// PodSync is used to trigger syncing of a pod when the observed change of
	// the state of the pod cannot be captured by any single event above.
	// 如果观察到的pod的变化不能被上述任何一种单一的event描述，则触发一次对pod的同步
	PodSync PodLifeCycleEventType = "PodSync"
	// Do not use the events below because they are disabled in GenericPLEG.
	ContainerChanged PodLifeCycleEventType = "ContainerChanged"
)

// PodLifecycleEvent is an event that reflects the change of the pod state.
// PodLifecycleEvent是表示pod状态改变的事件
type PodLifecycleEvent struct {
	// The pod ID.
	ID types.UID
	// The type of the event.
	Type PodLifeCycleEventType
	// The accompanied data which varies based on the event type.
	//   - ContainerStarted/ContainerStopped: the container name (string).
	//   - All other event types: unused.
	// 根据事件类型的不同，伴随的数据也是不同的：
	//	 - ContainerStarted/ContainerStopped: 这两种类型，返回容器的名字（string）
	//	 - 其他所有类型：不使用
	Data interface{}
}

type PodLifecycleEventGenerator interface {
	Start()
	Watch() chan *PodLifecycleEvent
	Healthy() (bool, error)
}
