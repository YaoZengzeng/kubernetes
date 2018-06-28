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
	"fmt"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	runtimeapi "k8s.io/kubernetes/pkg/kubelet/apis/cri/v1alpha1/runtime"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
)

// GenericPLEG is an extremely simple generic PLEG that relies solely on
// periodic listing to discover container changes. It should be used
// as temporary replacement for container runtimes do not support a proper
// event generator yet.
// GenericPLEG是一个最为简单的generic PLEG，仅仅只依赖于定期地list来获取容器的变化
// 它应该是一个暂时的替代方案，作为那些不支持event generator的容器运行时
//
// Note that GenericPLEG assumes that a container would not be created,
// terminated, and garbage collected within one relist period. If such an
// incident happens, GenenricPLEG would miss all events regarding this
// container. In the case of relisting failure, the window may become longer.
// Note that this assumption is not unique -- many kubelet internal components
// rely on terminated containers as tombstones for bookkeeping purposes. The
// garbage collector is implemented to work with such situations. However, to
// guarantee that kubelet can handle missing container events, it is
// recommended to set the relist period short and have an auxiliary, longer
// periodic sync in kubelet as the safety net.
// GenericPLEG假设一个容器不会再一个relist period中被创建，删除以及GC
// 如果这种情况发生，GenenricPLEG会丢失该容器所有的events
type GenericPLEG struct {
	// The period for relisting.
	relistPeriod time.Duration
	// The container runtime.
	runtime kubecontainer.Runtime
	// The channel from which the subscriber listens events.
	// 订阅者可以从eventChannel中监听事件
	eventChannel chan *PodLifecycleEvent
	// The internal cache for pod/container information.
	// 内部的cache用于缓存pod/container信息
	podRecords podRecords
	// Time of the last relisting.
	// 上一次relisting的时间
	relistTime atomic.Value
	// Cache for storing the runtime states required for syncing pods.
	// Cache缓存了一些同步pods所需的runtime states
	cache kubecontainer.Cache
	// For testability.
	clock clock.Clock
	// Pods that failed to have their status retrieved during a relist. These pods will be
	// retried during the next relisting.
	// 在relist中，获取status失败的pods，这些pods会在下一次relisting的时候再次获取
	podsToReinspect map[types.UID]*kubecontainer.Pod
}

// plegContainerState has a one-to-one mapping to the
// kubecontainer.ContainerState except for the non-existent state. This state
// is introduced here to complete the state transition scenarios.
// plegContainerState和kubecontainer.ContainerState有着一对一的映射，除了non-existent state
// 之所以引入这个状态是为了完善state transition scenarios
type plegContainerState string

const (
	plegContainerRunning     plegContainerState = "running"
	plegContainerExited      plegContainerState = "exited"
	plegContainerUnknown     plegContainerState = "unknown"
	plegContainerNonExistent plegContainerState = "non-existent"

	// The threshold needs to be greater than the relisting period + the
	// relisting time, which can vary significantly. Set a conservative
	// threshold to avoid flipping between healthy and unhealthy.
	relistThreshold = 3 * time.Minute
)

func convertState(state kubecontainer.ContainerState) plegContainerState {
	switch state {
	case kubecontainer.ContainerStateCreated:
		// kubelet doesn't use the "created" state yet, hence convert it to "unknown".
		// kubelet并不使用"created"这个状态，因此将其转换为"unknown"
		return plegContainerUnknown
	case kubecontainer.ContainerStateRunning:
		return plegContainerRunning
	case kubecontainer.ContainerStateExited:
		return plegContainerExited
	case kubecontainer.ContainerStateUnknown:
		return plegContainerUnknown
	default:
		panic(fmt.Sprintf("unrecognized container state: %v", state))
	}
}

type podRecord struct {
	old     *kubecontainer.Pod
	current *kubecontainer.Pod
}

// podRecords中包含了old pod以及current pod的情况
type podRecords map[types.UID]*podRecord

func NewGenericPLEG(runtime kubecontainer.Runtime, channelCapacity int,
	relistPeriod time.Duration, cache kubecontainer.Cache, clock clock.Clock) PodLifecycleEventGenerator {
	return &GenericPLEG{
		relistPeriod: relistPeriod,
		runtime:      runtime,
		eventChannel: make(chan *PodLifecycleEvent, channelCapacity),
		podRecords:   make(podRecords),
		// cache中缓存了从运行时获取的pod status
		cache:        cache,
		clock:        clock,
	}
}

// Returns a channel from which the subscriber can receive PodLifecycleEvent
// events.
// Watch()仅仅只是返回一个channel, subscriber可以接收PodLifecycleEvent
// TODO: support multiple subscribers.
func (g *GenericPLEG) Watch() chan *PodLifecycleEvent {
	return g.eventChannel
}

// Start spawns a goroutine to relist periodically.
// Start启动一个goroutine，阶段性地进行relist
func (g *GenericPLEG) Start() {
	// 每隔1秒，调用一次g.relist函数
	go wait.Until(g.relist, g.relistPeriod, wait.NeverStop)
}

func (g *GenericPLEG) Healthy() (bool, error) {
	relistTime := g.getRelistTime()
	elapsed := g.clock.Since(relistTime)
	// relistThreshold为3分钟
	if elapsed > relistThreshold {
		return false, fmt.Errorf("pleg was last seen active %v ago; threshold is %v", elapsed, relistThreshold)
	}
	return true, nil
}

func generateEvents(podID types.UID, cid string, oldState, newState plegContainerState) []*PodLifecycleEvent {
	// 如果容器的状态没有发生变化，则返回nil
	if newState == oldState {
		return nil
	}

	glog.V(4).Infof("GenericPLEG: %v/%v: %v -> %v", podID, cid, oldState, newState)
	// 根据容器新的状态，产生PodLifecycleEvent
	switch newState {
	case plegContainerRunning:
		return []*PodLifecycleEvent{{ID: podID, Type: ContainerStarted, Data: cid}}
	case plegContainerExited:
		return []*PodLifecycleEvent{{ID: podID, Type: ContainerDied, Data: cid}}
	case plegContainerUnknown:
		// 当容器的状态未知时，设置的event类型为ContainerChanged
		return []*PodLifecycleEvent{{ID: podID, Type: ContainerChanged, Data: cid}}
	case plegContainerNonExistent:
		// 如果容器不存在了，根据之前的状态产生PodLifecycleEvent
		switch oldState {
		case plegContainerExited:
			// We already reported that the container died before.
			// 如果我们之前已经汇报了container died，则将container移除
			return []*PodLifecycleEvent{{ID: podID, Type: ContainerRemoved, Data: cid}}
		default:
			// 否则接连产生ContainerDied和ContainerRemoved两个事件
			return []*PodLifecycleEvent{{ID: podID, Type: ContainerDied, Data: cid}, {ID: podID, Type: ContainerRemoved, Data: cid}}
		}
	default:
		panic(fmt.Sprintf("unrecognized container state: %v", newState))
	}
}

func (g *GenericPLEG) getRelistTime() time.Time {
	val := g.relistTime.Load()
	if val == nil {
		return time.Time{}
	}
	return val.(time.Time)
}

func (g *GenericPLEG) updateRelistTime(timestamp time.Time) {
	g.relistTime.Store(timestamp)
}

// relist queries the container runtime for list of pods/containers, compare
// with the internal pods/containers, and generates events accordingly.
// relist请求容器运行时list pod以及容器，并且和内部的pods/containers进行比较
// 并据此产生events
func (g *GenericPLEG) relist() {
	glog.V(5).Infof("GenericPLEG: Relisting")

	if lastRelistTime := g.getRelistTime(); !lastRelistTime.IsZero() {
		metrics.PLEGRelistInterval.Observe(metrics.SinceInMicroseconds(lastRelistTime))
	}

	timestamp := g.clock.Now()
	defer func() {
		metrics.PLEGRelistLatency.Observe(metrics.SinceInMicroseconds(timestamp))
	}()

	// Get all the pods.
	// 从运行时获取所有的pods
	podList, err := g.runtime.GetPods(true)
	if err != nil {
		glog.Errorf("GenericPLEG: Unable to retrieve pods: %v", err)
		return
	}

	g.updateRelistTime(timestamp)

	pods := kubecontainer.Pods(podList)
	g.podRecords.setCurrent(pods)

	// Compare the old and the current pods, and generate events.
	// 比较old以及current pods，并且据此产生events
	// 根据pod uid对event进行分类
	eventsByPodID := map[types.UID][]*PodLifecycleEvent{}
	for pid := range g.podRecords {
		oldPod := g.podRecords.getOld(pid)
		pod := g.podRecords.getCurrent(pid)
		// Get all containers in the old and the new pod.
		// 获取old以及new pod中所有的containers
		allContainers := getContainersFromPods(oldPod, pod)
		for _, container := range allContainers {
			// 针对每个容器，创建对应的event
			events := computeEvents(oldPod, pod, &container.ID)
			for _, e := range events {
				// 扩展eventsByPodID
				// 用eventsByPodID对每个pod的event进行归类
				updateEvents(eventsByPodID, e)
			}
		}
	}

	var needsReinspection map[types.UID]*kubecontainer.Pod
	if g.cacheEnabled() {
		// 如果g.cache不为nil
		needsReinspection = make(map[types.UID]*kubecontainer.Pod)
	}

	// If there are events associated with a pod, we should update the
	// podCache.
	// 如果有和pod相关的events，我们应该更新podCache
	for pid, events := range eventsByPodID {
		pod := g.podRecords.getCurrent(pid)
		if g.cacheEnabled() {
			// updateCache() will inspect the pod and update the cache. If an
			// error occurs during the inspection, we want PLEG to retry again
			// in the next relist. To achieve this, we do not update the
			// associated podRecord of the pod, so that the change will be
			// detect again in the next relist.
			// updateCache()会检查pod并且更新cache，如果在检查期间遇到了问题，我们想要
			// PLEG能够在下一次relist的时候try again
			// 为此我们不更新相关的podRecord里的pod，这样在下次relist的时候也能检测到change
			// TODO: If many pods changed during the same relist period,
			// inspecting the pod and getting the PodStatus to update the cache
			// serially may take a while. We should be aware of this and
			// parallelize if needed.
			if err := g.updateCache(pod, pid); err != nil {
				glog.Errorf("PLEG: Ignoring events for pod %s/%s: %v", pod.Name, pod.Namespace, err)

				// make sure we try to reinspect the pod during the next relisting
				// 确保我们在下一次relisting的时候reinspect the pod
				needsReinspection[pid] = pod

				continue
			} else if _, found := g.podsToReinspect[pid]; found {
				// this pod was in the list to reinspect and we did so because it had events, so remove it
				// from the list (we don't want the reinspection code below to inspect it a second time in
				// this relist execution)
				// update成功，从g.podsToReinspect中删除
				delete(g.podsToReinspect, pid)
			}
		}
		// Update the internal storage and send out the events.
		// 更新podRecords，如果pod已经不存在了，则将其删除，否则将current变为old
		g.podRecords.update(pid)
		for i := range events {
			// Filter out events that are not reliable and no other components use yet.
			// 对于"ContainerChanged"类型直接忽略
			if events[i].Type == ContainerChanged {
				continue
			}
			// 将event从eventChannel中发出
			g.eventChannel <- events[i]
		}
	}

	if g.cacheEnabled() {
		// reinspect any pods that failed inspection during the previous relist
		// reinspect那些在上一次relist的时候failed那些pods
		if len(g.podsToReinspect) > 0 {
			glog.V(5).Infof("GenericPLEG: Reinspecting pods that previously failed inspection")
			for pid, pod := range g.podsToReinspect {
				if err := g.updateCache(pod, pid); err != nil {
					glog.Errorf("PLEG: pod %s/%s failed reinspection: %v", pod.Name, pod.Namespace, err)
					needsReinspection[pid] = pod
				}
			}
		}

		// Update the cache timestamp.  This needs to happen *after*
		// all pods have been properly updated in the cache.
		// 更新cache的timestamp，这需要在cache中所有的pods都更新完成之后
		// 才能发生		
		g.cache.UpdateTime(timestamp)
	}

	// make sure we retain the list of pods that need reinspecting the next time relist is called
	// 将needsReinspection赋值给g.podsToReinspect，确保它们在下一次relist的时候会被检测
	g.podsToReinspect = needsReinspection
}

func getContainersFromPods(pods ...*kubecontainer.Pod) []*kubecontainer.Container {
	cidSet := sets.NewString()
	var containers []*kubecontainer.Container
	// 获取pods中所有不重复的containers
	for _, p := range pods {
		if p == nil {
			continue
		}
		for _, c := range p.Containers {
			cid := string(c.ID.ID)
			if cidSet.Has(cid) {
				continue
			}
			cidSet.Insert(cid)
			containers = append(containers, c)
		}
		// Update sandboxes as containers
		// TODO: keep track of sandboxes explicitly.
		// 将sandbox也作为containers对待
		for _, c := range p.Sandboxes {
			cid := string(c.ID.ID)
			if cidSet.Has(cid) {
				continue
			}
			cidSet.Insert(cid)
			containers = append(containers, c)
		}

	}
	return containers
}

func computeEvents(oldPod, newPod *kubecontainer.Pod, cid *kubecontainer.ContainerID) []*PodLifecycleEvent {
	var pid types.UID
	if oldPod != nil {
		pid = oldPod.ID
	} else if newPod != nil {
		pid = newPod.ID
	}
	oldState := getContainerState(oldPod, cid)
	newState := getContainerState(newPod, cid)
	// 从新老pod中分别获取容器状态，并产生event
	return generateEvents(pid, cid.ID, oldState, newState)
}

func (g *GenericPLEG) cacheEnabled() bool {
	return g.cache != nil
}

// Preserve an older cached status' pod IP if the new status has no pod IP
// and its sandboxes have exited
// 如果新的status没有pod IP并且它的sandboxes已经退出，保留older cached status的pod IP
func (g *GenericPLEG) getPodIP(pid types.UID, status *kubecontainer.PodStatus) string {
	if status.IP != "" {
		return status.IP
	}

	oldStatus, err := g.cache.Get(pid)
	if err != nil || oldStatus.IP == "" {
		return ""
	}

	for _, sandboxStatus := range status.SandboxStatuses {
		// If at least one sandbox is ready, then use this status update's pod IP
		// 如果至少有一个sandbox处于ready状态，那么使用该status的pod ip
		if sandboxStatus.State == runtimeapi.PodSandboxState_SANDBOX_READY {
			return status.IP
		}
	}

	if len(status.SandboxStatuses) == 0 {
		// Without sandboxes (which built-in runtimes like rkt don't report)
		// look at all the container statuses, and if any containers are
		// running then use the new pod IP
		for _, containerStatus := range status.ContainerStatuses {
			if containerStatus.State == kubecontainer.ContainerStateCreated || containerStatus.State == kubecontainer.ContainerStateRunning {
				return status.IP
			}
		}
	}

	// For pods with no ready containers or sandboxes (like exited pods)
	// use the old status' pod IP
	// 对于已经没有处于ready状态的容器或者sandboxes，则使用old status的pod IP
	return oldStatus.IP
}

func (g *GenericPLEG) updateCache(pod *kubecontainer.Pod, pid types.UID) error {
	if pod == nil {
		// The pod is missing in the current relist. This means that
		// the pod has no visible (active or inactive) containers.
		glog.V(4).Infof("PLEG: Delete status for pod %q", string(pid))
		// 如果pod为空，则直接从cache中删除
		g.cache.Delete(pid)
		return nil
	}
	timestamp := g.clock.Now()
	// TODO: Consider adding a new runtime method
	// GetPodStatus(pod *kubecontainer.Pod) so that Docker can avoid listing
	// all containers again.
	status, err := g.runtime.GetPodStatus(pod.ID, pod.Name, pod.Namespace)
	glog.V(4).Infof("PLEG: Write status for %s/%s: %#v (err: %v)", pod.Name, pod.Namespace, status, err)
	if err == nil {
		// Preserve the pod IP across cache updates if the new IP is empty.
		// When a pod is torn down, kubelet may race with PLEG and retrieve
		// a pod status after network teardown, but the kubernetes API expects
		// the completed pod's IP to be available after the pod is dead.
		status.IP = g.getPodIP(pid, status)
	}

	// 从容器运行时获取pod status之后，写入cache
	g.cache.Set(pod.ID, status, err, timestamp)
	return err
}

func updateEvents(eventsByPodID map[types.UID][]*PodLifecycleEvent, e *PodLifecycleEvent) {
	if e == nil {
		return
	}
	eventsByPodID[e.ID] = append(eventsByPodID[e.ID], e)
}

func getContainerState(pod *kubecontainer.Pod, cid *kubecontainer.ContainerID) plegContainerState {
	// Default to the non-existent state.
	// 默认的容器的状态为"non-existent"
	state := plegContainerNonExistent
	if pod == nil {
		return state
	}
	// 查找pod中是否存在该容器，包括sandbox或者container
	c := pod.FindContainerByID(*cid)
	if c != nil {
		return convertState(c.State)
	}
	// Search through sandboxes too.
	c = pod.FindSandboxByID(*cid)
	if c != nil {
		return convertState(c.State)
	}

	return state
}

func (pr podRecords) getOld(id types.UID) *kubecontainer.Pod {
	r, ok := pr[id]
	if !ok {
		return nil
	}
	return r.old
}

func (pr podRecords) getCurrent(id types.UID) *kubecontainer.Pod {
	r, ok := pr[id]
	if !ok {
		return nil
	}
	return r.current
}

func (pr podRecords) setCurrent(pods []*kubecontainer.Pod) {
	for i := range pr {
		// 先将所有的podRecords中的current都设置为nil
		pr[i].current = nil
	}
	for _, pod := range pods {
		// 遍历新获取的pods，设置对应的current
		if r, ok := pr[pod.ID]; ok {
			r.current = pod
		} else {
			pr[pod.ID] = &podRecord{current: pod}
		}
	}
}

func (pr podRecords) update(id types.UID) {
	r, ok := pr[id]
	if !ok {
		return
	}
	pr.updateInternal(id, r)
}

func (pr podRecords) updateInternal(id types.UID, r *podRecord) {
	if r.current == nil {
		// Pod no longer exists; delete the entry.
		// 如果Pod已经存在了，则将该entry删除
		delete(pr, id)
		return
	}
	// 将r.current赋值给r.old
	r.old = r.current
	r.current = nil
}
