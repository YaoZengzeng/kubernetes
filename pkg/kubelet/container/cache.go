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

package container

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// Cache stores the PodStatus for the pods. It represents *all* the visible
// pods/containers in the container runtime. All cache entries are at least as
// new or newer than the global timestamp (set by UpdateTime()), while
// individual entries may be slightly newer than the global timestamp. If a pod
// has no states known by the runtime, Cache returns an empty PodStatus object
// with ID populated.
// Cache保存了pods的PodStatus，它代表了在容器运行时所有可见的pods/containers
// 所有的cache entries都比global timestamp更新，或一样新，有个别的entries可能会比
// global timestamp更新，如果runtime不知道pod的存在，Cache就返回一个空的PodStatus对象
// 其中仅仅保存了ID
//
// Cache provides two methods to retrive the PodStatus: the non-blocking Get()
// and the blocking GetNewerThan() method. The component responsible for
// populating the cache is expected to call Delete() to explicitly free the
// cache entries.
// Cache提供了两种方法用于获取PodStatus: 非阻塞的Get()方法以及阻塞的GetNewerThan()方法
// 负责填充cache的组件负责调用Delete()来显式地清除cache entries
type Cache interface {
	// Get是非阻塞的
	Get(types.UID) (*PodStatus, error)
	Set(types.UID, *PodStatus, error, time.Time)
	// GetNewerThan is a blocking call that only returns the status
	// when it is newer than the given time.
	// GetNewerThan是阻塞的，只返回比给定时间更新的status	
	GetNewerThan(types.UID, time.Time) (*PodStatus, error)
	Delete(types.UID)
	UpdateTime(time.Time)
}

// data表示pod的状态信息
type data struct {
	// Status of the pod.
	// pod的状态
	status *PodStatus
	// Error got when trying to inspect the pod.
	err error
	// Time when the data was last modified.
	modified time.Time
}

type subRecord struct {
	time time.Time
	ch   chan *data
}

// cache implements Cache.
type cache struct {
	// Lock which guards all internal data structures.
	lock sync.RWMutex
	// Map that stores the pod statuses.
	// 用来存储pod status的Map
	pods map[types.UID]*data
	// A global timestamp represents how fresh the cached data is. All
	// cache content is at the least newer than this timestamp. Note that the
	// timestamp is nil after initialization, and will only become non-nil when
	// it is ready to serve the cached statuses.
	// global timestamp用来表示缓存的数据有多新，所有缓存的内容都至少要新过这个timestamp
	timestamp *time.Time
	// Map that stores the subscriber records.
	// 存储subscriber records的map
	subscribers map[types.UID][]*subRecord
}

// NewCache creates a pod cache.
func NewCache() Cache {
	return &cache{pods: map[types.UID]*data{}, subscribers: map[types.UID][]*subRecord{}}
}

// Get returns the PodStatus for the pod; callers are expected not to
// modify the objects returned.
// Get返回pod的PodStatus，调用者不能修改返回的objects
func (c *cache) Get(id types.UID) (*PodStatus, error) {
	c.lock.RLock()
	defer c.lock.RUnlock()
	d := c.get(id)
	return d.status, d.err
}

// GetNewerThan用于获取比minTime更新的status
func (c *cache) GetNewerThan(id types.UID, minTime time.Time) (*PodStatus, error) {
	ch := c.subscribe(id, minTime)
	d := <-ch
	return d.status, d.err
}

// Set sets the PodStatus for the pod.
// Set设置pod的PodStatus
func (c *cache) Set(id types.UID, status *PodStatus, err error, timestamp time.Time) {
	c.lock.Lock()
	defer c.lock.Unlock()
	defer c.notify(id, timestamp)
	c.pods[id] = &data{status: status, err: err, modified: timestamp}
}

// Delete removes the entry of the pod.
// Delete移除pod的entry
func (c *cache) Delete(id types.UID) {
	c.lock.Lock()
	defer c.lock.Unlock()
	delete(c.pods, id)
}

//  UpdateTime modifies the global timestamp of the cache and notify
//  subscribers if needed.
//  UpdateTime修改cache的global timestamp，并且有需要的话，通知subscriber
func (c *cache) UpdateTime(timestamp time.Time) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.timestamp = &timestamp
	// Notify all the subscribers if the condition is met.
	// 通知所有的subscriber
	for id := range c.subscribers {
		c.notify(id, *c.timestamp)
	}
}

func makeDefaultData(id types.UID) *data {
	return &data{status: &PodStatus{ID: id}, err: nil}
}

func (c *cache) get(id types.UID) *data {
	d, ok := c.pods[id]
	if !ok {
		// Cache should store *all* pod/container information known by the
		// container runtime. A cache miss indicates that there are no states
		// regarding the pod last time we queried the container runtime.
		// What this *really* means is that there are no visible pod/containers
		// associated with this pod. Simply return an default (mostly empty)
		// PodStatus to reflect this.
		// Cache应该保存所有容器运行时已知的pod/container信息
		// 一次cache miss意味着我们在上次访问容器运行时的时候没有该pod的状态信息
		// 实际上这意味着该pod没有可见的pod/containers与它关联
		// 简单地返回一个默认的PodStatus来反映
		return makeDefaultData(id)
	}
	return d
}

// getIfNewerThan returns the data it is newer than the given time.
// Otherwise, it returns nil. The caller should acquire the lock.
// getIfNewerThan返回data，如果它比给定的time更新的话
// 否则返回nil
func (c *cache) getIfNewerThan(id types.UID, minTime time.Time) *data {
	d, ok := c.pods[id]
	// 当容器第一次被创建，globalTimestampIsNewer为true
	globalTimestampIsNewer := (c.timestamp != nil && c.timestamp.After(minTime))
	if !ok && globalTimestampIsNewer {
		// Status is not cached, but the global timestamp is newer than
		// minTime, return the default status.
		// Status没有被缓存，但是global timestamp比minTime更新
		// 返回默认的status
		return makeDefaultData(id)
	}
	if ok && (d.modified.After(minTime) || globalTimestampIsNewer) {
		// Status is cached, return status if either of the following is true.
		//   * status was modified after minTime
		//   * the global timestamp of the cache is newer than minTime.
		// 如果Status被缓存了，在以下两种情况之一为true时，返回status
		//	 * status在minTime之后被修改了
		//	 * minTime比cache的global timestamp无关
		return d
	}
	// The pod status is not ready.
	return nil
}

// notify sends notifications for pod with the given id, if the requirements
// are met. Note that the caller should acquire the lock.
// notify发送通知至给定id的pod，如果要求满足的话
func (c *cache) notify(id types.UID, timestamp time.Time) {
	list, ok := c.subscribers[id]
	if !ok {
		// No one to notify.
		return
	}
	newList := []*subRecord{}
	for i, r := range list {
		if timestamp.Before(r.time) {
			// Doesn't meet the time requirement; keep the record.
			// 没有满足时间要求，保存record
			newList = append(newList, list[i])
			continue
		}
		// 否则，发送数据
		r.ch <- c.get(id)
		close(r.ch)
	}
	// 更新c.subscribers[]
	if len(newList) == 0 {
		delete(c.subscribers, id)
	} else {
		c.subscribers[id] = newList
	}
}

func (c *cache) subscribe(id types.UID, timestamp time.Time) chan *data {
	ch := make(chan *data, 1)
	c.lock.Lock()
	defer c.lock.Unlock()
	d := c.getIfNewerThan(id, timestamp)
	if d != nil {
		// If the cache entry is ready, send the data and return immediately.
		// 如果cache已经准备完成，则马上发送数据并返回
		ch <- d
		return ch
	}
	// Add the subscription record.
	// 否则，加入subscription record
	c.subscribers[id] = append(c.subscribers[id], &subRecord{time: timestamp, ch: ch})
	return ch
}
