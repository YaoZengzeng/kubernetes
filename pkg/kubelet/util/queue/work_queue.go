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

package queue

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/clock"
)

// WorkQueue allows queuing items with a timestamp. An item is
// considered ready to process if the timestamp has expired.
// WorkQueue允许将一个伴有timestamp的item加入队列
// 当timestamp耗尽的时候，item就认为可以处理了
type WorkQueue interface {
	// GetWork dequeues and returns all ready items.
	// GetWork出队并且返回所有处于ready状态的items
	GetWork() []types.UID
	// Enqueue inserts a new item or overwrites an existing item.
	// Enqueue插入一个新的item或者覆盖一个已经存在的item
	Enqueue(item types.UID, delay time.Duration)
}

type basicWorkQueue struct {
	clock clock.Clock
	lock  sync.Mutex
	queue map[types.UID]time.Time
}

var _ WorkQueue = &basicWorkQueue{}

func NewBasicWorkQueue(clock clock.Clock) WorkQueue {
	queue := make(map[types.UID]time.Time)
	return &basicWorkQueue{queue: queue, clock: clock}
}

func (q *basicWorkQueue) GetWork() []types.UID {
	q.lock.Lock()
	defer q.lock.Unlock()
	now := q.clock.Now()
	var items []types.UID
	// 遍历queue这个map，获取其中的timestamp早于now的item
	for k, v := range q.queue {
		if v.Before(now) {
			items = append(items, k)
			delete(q.queue, k)
		}
	}
	return items
}

func (q *basicWorkQueue) Enqueue(item types.UID, delay time.Duration) {
	q.lock.Lock()
	defer q.lock.Unlock()
	q.queue[item] = q.clock.Now().Add(delay)
}
