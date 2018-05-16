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

package workqueue

import (
	"sync"
)

type Interface interface {
	Add(item interface{})
	Len() int
	Get() (item interface{}, shutdown bool)
	Done(item interface{})
	ShutDown()
	ShuttingDown() bool
}

// New constructs a new work queue (see the package comment).
func New() *Type {
	return NewNamed("")
}

func NewNamed(name string) *Type {
	return &Type{
		dirty:      set{},
		processing: set{},
		cond:       sync.NewCond(&sync.Mutex{}),
		metrics:    newQueueMetrics(name),
	}
}

// Type is a work queue (see the package comment).
type Type struct {
	// queue defines the order in which we will work on items. Every
	// element of queue should be in the dirty set and not in the
	// processing set.
	// queue定义了我们处理item的顺序
	// queue中的每个item都应该在dirty set而不是在processing set中
	queue []t

	// dirty defines all of the items that need to be processed.
	// dirty定义了所有需要被处理的items
	dirty set

	// Things that are currently being processed are in the processing set.
	// These things may be simultaneously in the dirty set. When we finish
	// processing something and remove it from this set, we'll check if
	// it's in the dirty set, and if so, add it to the queue.
	// 那些正在被处理的items被存放在processing set中，这些item同时可能存在于dirty set里
	// 当我们处理完成一个item并且将它从这个队列移除，我们会检查它是否存在在dirty set中
	// 如果是的话，加入queue
	processing set

	cond *sync.Cond

	shuttingDown bool

	metrics queueMetrics
}

type empty struct{}
type t interface{}
// set的key的类型为interface, value的类型为struct{}
type set map[t]empty

func (s set) has(item t) bool {
	_, exists := s[item]
	return exists
}

func (s set) insert(item t) {
	s[item] = empty{}
}

func (s set) delete(item t) {
	delete(s, item)
}

// Add marks item as needing processing.
// Add将item标记为需要处理
func (q *Type) Add(item interface{}) {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	// 如果队列已经关闭，或者item已经存在
	if q.shuttingDown {
		return
	}
	if q.dirty.has(item) {
		return
	}

	q.metrics.add(item)

	// 加入dirty set
	q.dirty.insert(item)
	// 如果已经在processing队列中，直接返回
	if q.processing.has(item) {
		return
	}

	// 重新入队
	q.queue = append(q.queue, item)
	// Signal()唤醒一个正在调用cond.Wait()等待的goroutine
	q.cond.Signal()
}

// Len returns the current queue length, for informational purposes only. You
// shouldn't e.g. gate a call to Add() or Get() on Len() being a particular
// value, that can't be synchronized properly.
func (q *Type) Len() int {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	return len(q.queue)
}

// Get blocks until it can return an item to be processed. If shutdown = true,
// the caller should end their goroutine. You must call Done with item when you
// have finished processing it.
// Get会一直阻塞直到能够返回一个item进行处理，如果shutdown为true，则调用者需要结束它的goroutine
// 当我们处理完一个item之后，必须调用Done
func (q *Type) Get() (item interface{}, shutdown bool) {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	// 如果队列中没有item，并且队列没有被关闭
	for len(q.queue) == 0 && !q.shuttingDown {
		// Wait()自动释放cond.Lock，并且暂停goroutine的执行
		// 当接下来继续开始执行的时候，Wait()会在返回之前获取cond.Lock
		// Wait()只能被Signal或者Broadcast唤醒
		q.cond.Wait()
	}
	if len(q.queue) == 0 {
		// 说明队列被关闭了
		// We must be shutting down.
		return nil, true
	}

	// 获取队列中的第一个item
	item, q.queue = q.queue[0], q.queue[1:]

	q.metrics.get(item)

	// 加入processing set中
	q.processing.insert(item)
	// 从dirty set中删除
	q.dirty.delete(item)

	return item, false
}

// Done marks item as done processing, and if it has been marked as dirty again
// while it was being processed, it will be re-added to the queue for
// re-processing.
// Done标记item处理完成，如果它在被处理的时候又被标记为dirty了
// 它会重新加入队列，再次处理
func (q *Type) Done(item interface{}) {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()

	q.metrics.done(item)

	// 当一个item处理完成之后，从processing中删除
	q.processing.delete(item)
	if q.dirty.has(item) {
		// 如果在dirty中还要该item，则将其加入队列
		q.queue = append(q.queue, item)
		// 队列中新增了item，调用Signal进行通知
		q.cond.Signal()
	}
}

// ShutDown will cause q to ignore all new items added to it. As soon as the
// worker goroutines have drained the existing items in the queue, they will be
// instructed to exit.
// ShutDown会让队列忽略所有加入其中的items, 只要worker goroutines已经drained队列中已存的items
// 它们就会被要求退出
func (q *Type) ShutDown() {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	q.shuttingDown = true
	// Broadcast()唤醒所有正在调用cond.Wait()的goroutine
	q.cond.Broadcast()
}

func (q *Type) ShuttingDown() bool {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()

	return q.shuttingDown
}
