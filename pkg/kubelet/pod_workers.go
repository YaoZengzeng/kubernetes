/*
Copyright 2014 The Kubernetes Authors.

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

package kubelet

import (
	"fmt"
	"sync"
	"time"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/kubernetes/pkg/kubelet/util/queue"
)

// OnCompleteFunc is a function that is invoked when an operation completes.
// If err is non-nil, the operation did not complete successfully.
type OnCompleteFunc func(err error)

// PodStatusFunc is a function that is invoked to generate a pod status.
type PodStatusFunc func(pod *v1.Pod, podStatus *kubecontainer.PodStatus) v1.PodStatus

// KillPodOptions are options when performing a pod update whose update type is kill.
// KillPodOptions是执行一个kill类型的pod update包含的选项
type KillPodOptions struct {
	// PodStatusFunc is the function to invoke to set pod status in response to a kill request.
	// PodStatusFunc是为了回复kill request要调用的函数，用于设置pod status
	PodStatusFunc PodStatusFunc
	// PodTerminationGracePeriodSecondsOverride is optional override to use if a pod is being killed as part of kill operation.
	PodTerminationGracePeriodSecondsOverride *int64
}

// UpdatePodOptions is an options struct to pass to a UpdatePod operation.
type UpdatePodOptions struct {
	// pod to update
	Pod *v1.Pod
	// the mirror pod for the pod to update, if it is a static pod
	MirrorPod *v1.Pod
	// the type of update (create, update, sync, kill)
	// update的类型，包括create, update，sync以及kill
	UpdateType kubetypes.SyncPodType
	// optional callback function when operation completes
	// this callback is not guaranteed to be completed since a pod worker may
	// drop update requests if it was fulfilling a previous request.  this is
	// only guaranteed to be invoked in response to a kill pod request which is
	// always delivered.
	// OnCompleteFunc并不保证会执行，因为pod worker可能会丢弃update requests，如果它已经
	// 充满了pervious request
	// OnCompleteFunc只有在请求是kill pod时才会肯定被执行
	OnCompleteFunc OnCompleteFunc
	// if update type is kill, use the specified options to kill the pod.
	// 如果更新的类型为kill，使用特定的options用于kill the pod
	KillPodOptions *KillPodOptions
}

// PodWorkers is an abstract interface for testability.
type PodWorkers interface {
	UpdatePod(options *UpdatePodOptions)
	ForgetNonExistingPodWorkers(desiredPods map[types.UID]empty)
	ForgetWorker(uid types.UID)
}

// syncPodOptions provides the arguments to a SyncPod operation.
// syncPodOptions提供了SyncPod操作需要的参数
type syncPodOptions struct {
	// the mirror pod for the pod to sync, if it is a static pod
	mirrorPod *v1.Pod
	// pod to sync
	pod *v1.Pod
	// the type of update (create, update, sync)
	// update的类型
	updateType kubetypes.SyncPodType
	// the current status
	// pod当前的状态
	podStatus *kubecontainer.PodStatus
	// if update type is kill, use the specified options to kill the pod.
	// 如果update类型为kill，则用特定的options去kill pod
	killPodOptions *KillPodOptions
}

// the function to invoke to perform a sync.
type syncPodFnType func(options syncPodOptions) error

const (
	// jitter factor for resyncInterval
	workerResyncIntervalJitterFactor = 0.5

	// jitter factor for backOffPeriod
	workerBackOffPeriodJitterFactor = 0.5
)

type podWorkers struct {
	// Protects all per worker fields.
	podLock sync.Mutex

	// Tracks all running per-pod goroutines - per-pod goroutine will be
	// processing updates received through its corresponding channel.
	// 追踪所有正在运行的per-pod goroutines
	// per-pod goroutine会处理通过相应管道获取的update
	podUpdates map[types.UID]chan UpdatePodOptions
	// Track the current state of per-pod goroutines.
	// Currently all update request for a given pod coming when another
	// update of this pod is being processed are ignored.
	// 追踪per-pod goroutine的当前状态
	// 如果当前pod正在处理一个update，那么其他的update请求都会被忽略
	isWorking map[types.UID]bool
	// Tracks the last undelivered work item for this pod - a work item is
	// undelivered if it comes in while the worker is working.
	// 追踪该pod上一个undelivered work
	// 一个work item会处于undelivered状态，当worker正在处理其他item的时候
	lastUndeliveredWorkUpdate map[types.UID]UpdatePodOptions

	workQueue queue.WorkQueue

	// This function is run to sync the desired stated of pod.
	// NOTE: This function has to be thread-safe - it can be called for
	// different pods at the same time.
	// 该函数能将pod同步到desired state
	syncPodFn syncPodFnType

	// The EventRecorder to use
	recorder record.EventRecorder

	// backOffPeriod is the duration to back off when there is a sync error.
	// backoffPeriod是当sync error发生时的回退时间
	backOffPeriod time.Duration

	// resyncInterval is the duration to wait until the next sync.
	// resyncInterval是到下一个sync的时间间隔
	resyncInterval time.Duration

	// podCache stores kubecontainer.PodStatus for all pods.
	// podCache用于存储所有pod的kubecontainer.PodStatus
	podCache kubecontainer.Cache
}

func newPodWorkers(syncPodFn syncPodFnType, recorder record.EventRecorder, workQueue queue.WorkQueue,
	resyncInterval, backOffPeriod time.Duration, podCache kubecontainer.Cache) *podWorkers {
	return &podWorkers{
		podUpdates:                map[types.UID]chan UpdatePodOptions{},
		isWorking:                 map[types.UID]bool{},
		lastUndeliveredWorkUpdate: map[types.UID]UpdatePodOptions{},
		syncPodFn:                 syncPodFn,
		recorder:                  recorder,
		// PodWorker中的workQueue就是kubelet中的workQueue
		workQueue:                 workQueue,
		resyncInterval:            resyncInterval,
		backOffPeriod:             backOffPeriod,
		podCache:                  podCache,
	}
}

func (p *podWorkers) managePodLoop(podUpdates <-chan UpdatePodOptions) {
	var lastSyncTime time.Time
	// 不断从podUpdates中获取更新信息
	for update := range podUpdates {
		err := func() error {
			podUID := update.Pod.UID
			// This is a blocking call that would return only if the cache
			// has an entry for the pod that is newer than minRuntimeCache
			// Time. This ensures the worker doesn't start syncing until
			// after the cache is at least newer than the finished time of
			// the previous sync.
			// 确保从运行时获取的pod status比上次sync的finished time更新，才进行同步
			status, err := p.podCache.GetNewerThan(podUID, lastSyncTime)
			if err != nil {
				// This is the legacy event thrown by manage pod loop
				// all other events are now dispatched from syncPodFn
				// 这是唯一通过manage pod loop发送的event
				// 其他所有的event现在都是通过syncPodFn发送的
				p.recorder.Eventf(update.Pod, v1.EventTypeWarning, events.FailedSync, "error determining status: %v", err)
				return err
			}
			// 调用kubelet.go中的syncPod函数
			err = p.syncPodFn(syncPodOptions{
				mirrorPod:      update.MirrorPod,
				pod:            update.Pod,
				// podStatus从cache中获取
				podStatus:      status,
				killPodOptions: update.KillPodOptions,
				updateType:     update.UpdateType,
			})
			// 设置lastSyncTime
			lastSyncTime = time.Now()
			return err
		}()
		// notify the call-back function if the operation succeeded or not
		// 同步结束，调用onCompleteFunc
		if update.OnCompleteFunc != nil {
			update.OnCompleteFunc(err)
		}
		if err != nil {
			// IMPORTANT: we do not log errors here, the syncPodFn is responsible for logging errors
			// 不要在此处记录error，syncPodFn会负责记录错误
			glog.Errorf("Error syncing pod %s (%q), skipping: %v", update.Pod.UID, format.Pod(update.Pod), err)
		}
		// 每次同步完，都将pod重新加入到work queue中
		p.wrapUp(update.Pod.UID, err)
	}
}

// Apply the new setting to the specified pod.
// If the options provide an OnCompleteFunc, the function is invoked if the update is accepted.
// Update requests are ignored if a kill pod request is pending.
// 给指定pod添加新的设置
// 如果options提供了一个OnCompleteFunc函数，该函数就会在update被接收之后被调用
// Update请求会被忽略，如果一个kill pod request正在被pending
func (p *podWorkers) UpdatePod(options *UpdatePodOptions) {
	pod := options.Pod
	uid := pod.UID
	var podUpdates chan UpdatePodOptions
	var exists bool

	p.podLock.Lock()
	defer p.podLock.Unlock()
	// 如果在podUpdates中还不包含相应的pod
	if podUpdates, exists = p.podUpdates[uid]; !exists {
		// We need to have a buffer here, because checkForUpdates() method that
		// puts an update into channel is called from the same goroutine where
		// the channel is consumed. However, it is guaranteed that in such case
		// the channel is empty, so buffer of size 1 is enough.
		// podUpdates这个channel缓存的大小为1，因为checkForUpdates()方法会将一个update
		// 写入channel，这个方法和channel被消费的地方是处于同一个goroutine的
		// 不过在那个时候可以确保channel为空，这样大小为1的buffer就足够了
		podUpdates = make(chan UpdatePodOptions, 1)
		// podUpdates是一个map[id]chan，能够从该channel中获取某个pod最新的UpdatePodOptions
		p.podUpdates[uid] = podUpdates

		// Creating a new pod worker either means this is a new pod, or that the
		// kubelet just restarted. In either case the kubelet is willing to believe
		// the status of the pod for the first pod worker sync. See corresponding
		// comment in syncPod.
		// 创建一个新的pod worker，这可能意味着这是一个新pod，也可能是kubelet刚刚启动
		// 在任意情况下，kubelet都会愿意在第一个pod worker的sync中，相信pod的状态
		go func() {
			defer runtime.HandleCrash()
			// 创建一个pod worker，通过传入的管道podUpdates来获取更新
			p.managePodLoop(podUpdates)
		}()
	}
	// 如果pod worker已经存在了，那么每次sync只是给podUpdates这个channel
	// 发送更新的内容

	// 如果该pod还不在isWorking这个map中
	if !p.isWorking[pod.UID] {
		// 将pod加入isWorking这个map中
		p.isWorking[pod.UID] = true
		// 将其中的options放入podUpdates
		podUpdates <- *options
	} else {
		// if a request to kill a pod is pending, we do not let anything overwrite that request.
		// 如果一个请求kill pod的请求正在被pending，我们确保它不会被覆盖
		update, found := p.lastUndeliveredWorkUpdate[pod.UID]
		if !found || update.UpdateType != kubetypes.SyncPodKill {
			// 将最后一个未发送的update请求放在lastUndeliveredWorkUpdate中
			// 如果lastUndeliveredWorkUpdate找到了，并且类型不是SyncPodKill，就会被覆盖
			p.lastUndeliveredWorkUpdate[pod.UID] = *options
		}
	}
}

func (p *podWorkers) removeWorker(uid types.UID) {
	if ch, ok := p.podUpdates[uid]; ok {
		close(ch)
		delete(p.podUpdates, uid)
		// If there is an undelivered work update for this pod we need to remove it
		// since per-pod goroutine won't be able to put it to the already closed
		// channel when it finish processing the current work update.
		if _, cached := p.lastUndeliveredWorkUpdate[uid]; cached {
			delete(p.lastUndeliveredWorkUpdate, uid)
		}
	}
}
func (p *podWorkers) ForgetWorker(uid types.UID) {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	p.removeWorker(uid)
}

func (p *podWorkers) ForgetNonExistingPodWorkers(desiredPods map[types.UID]empty) {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	for key := range p.podUpdates {
		if _, exists := desiredPods[key]; !exists {
			p.removeWorker(key)
		}
	}
}

func (p *podWorkers) wrapUp(uid types.UID, syncErr error) {
	// Requeue the last update if the last sync returned error.
	// 如果上一次同步遇到了error, 则将上一次update重新加入队列
	switch {
	// 后入队列的会将之前入队的覆盖
	case syncErr == nil:
		// No error; requeue at the regular resync interval.
		// 没有错误，则以正常的同步时间间隔入队
		p.workQueue.Enqueue(uid, wait.Jitter(p.resyncInterval, workerResyncIntervalJitterFactor))
	default:
		// Error occurred during the sync; back off and then retry.
		// 在同步期间遇到了问题，回退并且重试
		p.workQueue.Enqueue(uid, wait.Jitter(p.backOffPeriod, workerBackOffPeriodJitterFactor))
	}
	p.checkForUpdates(uid)
}

func (p *podWorkers) checkForUpdates(uid types.UID) {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	if workUpdate, exists := p.lastUndeliveredWorkUpdate[uid]; exists {
		// 将上一次undeliveredWorkUpdate写入p.podUpdates[uid]
		p.podUpdates[uid] <- workUpdate
		delete(p.lastUndeliveredWorkUpdate, uid)
	} else {
		// 否则，如果没有undelivered updates，则将p.isWorking[uid]设置为false
		p.isWorking[uid] = false
	}
}

// killPodNow returns a KillPodFunc that can be used to kill a pod.
// It is intended to be injected into other modules that need to kill a pod.
// killPodNow返回一个KillPodFunc，它可以用于kill一个pod
// 它主要用于注入其他需要kill pod的模块
func killPodNow(podWorkers PodWorkers, recorder record.EventRecorder) eviction.KillPodFunc {
	return func(pod *v1.Pod, status v1.PodStatus, gracePeriodOverride *int64) error {
		// determine the grace period to use when killing the pod
		gracePeriod := int64(0)
		if gracePeriodOverride != nil {
			gracePeriod = *gracePeriodOverride
		} else if pod.Spec.TerminationGracePeriodSeconds != nil {
			gracePeriod = *pod.Spec.TerminationGracePeriodSeconds
		}

		// we timeout and return an error if we don't get a callback within a reasonable time.
		// the default timeout is relative to the grace period (we settle on 10s to wait for kubelet->runtime traffic to complete in sigkill)
		timeout := int64(gracePeriod + (gracePeriod / 2))
		minTimeout := int64(10)
		if timeout < minTimeout {
			timeout = minTimeout
		}
		timeoutDuration := time.Duration(timeout) * time.Second

		// open a channel we block against until we get a result
		type response struct {
			err error
		}
		ch := make(chan response)
		podWorkers.UpdatePod(&UpdatePodOptions{
			Pod:        pod,
			UpdateType: kubetypes.SyncPodKill,
			OnCompleteFunc: func(err error) {
				ch <- response{err: err}
			},
			KillPodOptions: &KillPodOptions{
				PodStatusFunc: func(p *v1.Pod, podStatus *kubecontainer.PodStatus) v1.PodStatus {
					return status
				},
				PodTerminationGracePeriodSecondsOverride: gracePeriodOverride,
			},
		})

		// wait for either a response, or a timeout
		select {
		case r := <-ch:
			return r.err
		case <-time.After(timeoutDuration):
			recorder.Eventf(pod, v1.EventTypeWarning, events.ExceededGracePeriod, "Container runtime did not kill the pod within specified grace period.")
			return fmt.Errorf("timeout waiting to kill pod")
		}
	}
}
