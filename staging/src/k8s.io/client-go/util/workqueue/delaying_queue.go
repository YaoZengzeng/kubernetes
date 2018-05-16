/*
Copyright 2016 The Kubernetes Authors.

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
	"container/heap"
	"time"

	"k8s.io/apimachinery/pkg/util/clock"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

// DelayingInterface is an Interface that can Add an item at a later time. This makes it easier to
// requeue items after failures without ending up in a hot-loop.
// DelayingInterface是一个能在later time添加item的接口
// 这能够让item在failures之后，重新入队更加容易而不用停止hot-loop
type DelayingInterface interface {
	Interface
	// AddAfter adds an item to the workqueue after the indicated duration has passed
	// 在指定的时间段之后，将item加入workqueue
	AddAfter(item interface{}, duration time.Duration)
}

// NewDelayingQueue constructs a new workqueue with delayed queuing ability
func NewDelayingQueue() DelayingInterface {
	return newDelayingQueue(clock.RealClock{}, "")
}

func NewNamedDelayingQueue(name string) DelayingInterface {
	return newDelayingQueue(clock.RealClock{}, name)
}

func newDelayingQueue(clock clock.Clock, name string) DelayingInterface {
	ret := &delayingType{
		// 创建一个队列
		Interface:       NewNamed(name),
		clock:           clock,
		// 每隔10秒产生一个Tick
		heartbeat:       clock.Tick(maxWait),
		stopCh:          make(chan struct{}),
		// 配置的channel大小为1000
		waitingForAddCh: make(chan *waitFor, 1000),
		metrics:         newRetryMetrics(name),
	}

	// 创建goroutine，进行循环
	go ret.waitingLoop()

	return ret
}

// delayingType wraps an Interface and provides delayed re-enquing
// delayingType对队列接口进行封装并且提供延迟入队
type delayingType struct {
	Interface

	// clock tracks time for delayed firing
	clock clock.Clock

	// stopCh lets us signal a shutdown to the waiting loop
	// stopCh能让我们给waiting loop发送shutdown信号
	stopCh chan struct{}

	// heartbeat ensures we wait no more than maxWait before firing
	// heartbeat确保我们在firing之前我们不会等到超过maxWait
	//
	// TODO: replace with Ticker (and add to clock) so this can be cleaned up.
	// clock.Tick will leak.
	heartbeat <-chan time.Time

	// waitingForAddCh is a buffered channel that feeds waitingForAdd
	// waitingForAddCh是一个缓冲的channel，用于反馈waitingForAdd
	waitingForAddCh chan *waitFor

	// metrics counts the number of retries
	metrics retryMetrics
}

// waitFor holds the data to add and the time it should be added
// waitFor保存了要添加的数据以及它应该被添加的时间
type waitFor struct {
	data    t
	readyAt time.Time
	// index in the priority queue (heap)
	// 在优先级队列中的索引
	index int
}

// waitForPriorityQueue implements a priority queue for waitFor items.
// waitForPriorityQueue实现了将waitFor作为item的优先级队列
//
// waitForPriorityQueue implements heap.Interface. The item occuring next in
// time (i.e., the item with the smallest readyAt) is at the root (index 0).
// Peek returns this minimum item at index 0. Pop returns the minimum item after
// it has been removed from the queue and placed at index Len()-1 by
// container/heap. Push adds an item at index Len(), and container/heap
// percolates it into the correct location.
// waitForPriorityQueue实现了heap.Interface这一接口
// 下一次返回的item，（有着最小的readAt）处于根（index为0）
// Peek返回在index为0的最小的item
// Pop返回最小的item，当它从队列中移除时，并且被container/heap存放到index为Len() - 1的地方
// Push将item放到index为Len()的地方，container/heap之后会将它放到合适的地方
type waitForPriorityQueue []*waitFor

func (pq waitForPriorityQueue) Len() int {
	return len(pq)
}
func (pq waitForPriorityQueue) Less(i, j int) bool {
	return pq[i].readyAt.Before(pq[j].readyAt)
}
func (pq waitForPriorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

// Push adds an item to the queue. Push should not be called directly; instead,
// use `heap.Push`.
// Push将一个item加入队列，Push不应该被直接调用，相反，我们应该使用`heap.Push`
func (pq *waitForPriorityQueue) Push(x interface{}) {
	n := len(*pq)
	item := x.(*waitFor)
	item.index = n
	*pq = append(*pq, item)
}

// Pop removes an item from the queue. Pop should not be called directly;
// instead, use `heap.Pop`.
func (pq *waitForPriorityQueue) Pop() interface{} {
	n := len(*pq)
	item := (*pq)[n-1]
	item.index = -1
	*pq = (*pq)[0:(n - 1)]
	return item
}

// Peek returns the item at the beginning of the queue, without removing the
// item or otherwise mutating the queue. It is safe to call directly.
func (pq waitForPriorityQueue) Peek() interface{} {
	return pq[0]
}

// ShutDown gives a way to shut off this queue
func (q *delayingType) ShutDown() {
	q.Interface.ShutDown()
	close(q.stopCh)
}

// AddAfter adds the given item to the work queue after the given delay
func (q *delayingType) AddAfter(item interface{}, duration time.Duration) {
	// don't add if we're already shutting down
	if q.ShuttingDown() {
		return
	}

	q.metrics.retry()

	// immediately add things with no delay
	// duration小于等于0，直接将item加入队列
	if duration <= 0 {
		q.Add(item)
		return
	}

	select {
	case <-q.stopCh:
		// unblock if ShutDown() is called
		// 构造waitFor，加入waitingForAddCh这个channel中
	case q.waitingForAddCh <- &waitFor{data: item, readyAt: q.clock.Now().Add(duration)}:
	}
}

// maxWait keeps a max bound on the wait time. It's just insurance against weird things happening.
// Checking the queue every 10 seconds isn't expensive and we know that we'll never end up with an
// expired item sitting for more than 10 seconds.
// maxWait保持了最长的等待时间，这只是为了确保不会有奇怪的事情发生
// 每10秒检查队列一次并不昂贵，但这样我们就知道，不会有item在队列中等待超过10秒
const maxWait = 10 * time.Second

// waitingLoop runs until the workqueue is shutdown and keeps a check on the list of items to be added.
// waitingLoop一直运行，直到workqueue被关闭，并且保持检查新增加的items
func (q *delayingType) waitingLoop() {
	defer utilruntime.HandleCrash()

	// Make a placeholder channel to use when there are no items in our list
	// 创建一个placeholder channel，当我们的list中没有item的时候，
	never := make(<-chan time.Time)

	// 创建priority queue
	waitingForQueue := &waitForPriorityQueue{}
	heap.Init(waitingForQueue)

	// waitingEntryByData保证加入的data不会重复
	waitingEntryByData := map[t]*waitFor{}

	for {
		// 当队列被关闭时，退出
		if q.Interface.ShuttingDown() {
			return
		}

		now := q.clock.Now()

		// Add ready entries
		// 将处于ready的entry加入队列中
		for waitingForQueue.Len() > 0 {
			entry := waitingForQueue.Peek().(*waitFor)
			// 直到找到入队时间在当前时间点之后的entry
			if entry.readyAt.After(now) {
				break
			}

			entry = heap.Pop(waitingForQueue).(*waitFor)
			// 加入队列
			q.Add(entry.data)
			delete(waitingEntryByData, entry.data)
		}

		// Set up a wait for the first item's readyAt (if one exists)
		// 获取下一个item的readyAt
		nextReadyAt := never
		if waitingForQueue.Len() > 0 {
			entry := waitingForQueue.Peek().(*waitFor)
			nextReadyAt = q.clock.After(entry.readyAt.Sub(now))
		}

		select {
		case <-q.stopCh:
			return

		case <-q.heartbeat:
			// continue the loop, which will add ready items

		// 获取下一个到期时间
		case <-nextReadyAt:
			// continue the loop, which will add ready items

		case waitEntry := <-q.waitingForAddCh:
			// 从channel中获取数据
			if waitEntry.readyAt.After(q.clock.Now()) {
				// 将从channel中获取的waitEntry放入waitingForQueue中
				insert(waitingForQueue, waitingEntryByData, waitEntry)
			} else {
				// 如果在当前时间点之前，直接加入队列
				q.Add(waitEntry.data)
			}

			drained := false
			for !drained {
				select {
				case waitEntry := <-q.waitingForAddCh:
					if waitEntry.readyAt.After(q.clock.Now()) {
						insert(waitingForQueue, waitingEntryByData, waitEntry)
					} else {
						q.Add(waitEntry.data)
					}
				default:
					// 从管道中读完数据，则drained为true
					drained = true
				}
			}
		}
	}
}

// insert adds the entry to the priority queue, or updates the readyAt if it already exists in the queue
// insert将entry加入priority queue，或者更新readyAt，如果它已经在队列中存在的话
func insert(q *waitForPriorityQueue, knownEntries map[t]*waitFor, entry *waitFor) {
	// if the entry already exists, update the time only if it would cause the item to be queued sooner
	// 如果entry已经存在的话，如果它能更快地让item入队的话，更新time
	existing, exists := knownEntries[entry.data]
	if exists {
		// 如果entry已经存在，并且时间能提前
		if existing.readyAt.After(entry.readyAt) {
			existing.readyAt = entry.readyAt
			// 更新
			heap.Fix(q, existing.index)
		}

		return
	}

	// 加入heap
	heap.Push(q, entry)
	knownEntries[entry.data] = entry
}
