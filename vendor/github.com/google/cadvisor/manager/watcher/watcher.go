// Copyright 2016 Google Inc. All Rights Reserved.
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
package watcher

// SubcontainerEventType indicates an addition or deletion event.
// SubcontainerEventType代表了一个addition或者deletion事件
type ContainerEventType int

const (
	ContainerAdd ContainerEventType = iota
	ContainerDelete
)

type ContainerWatchSource int

const (
	Raw ContainerWatchSource = iota
	Rkt
)

// ContainerEvent represents a
type ContainerEvent struct {
	// The type of event that occurred.
	// 事件的类型
	EventType ContainerEventType

	// The full container name of the container where the event occurred.
	// 事件发生的容器的full container name
	Name string

	// The watcher that detected this change event
	// 探测到此次change event的watcher
	WatchSource ContainerWatchSource
}

type ContainerWatcher interface {
	// Registers a channel to listen for events affecting subcontainers (recursively).
	// 注册一个channel，用于监听影响subcontainers的events
	Start(events chan ContainerEvent) error

	// Stops watching for subcontainer changes.
	Stop() error
}
