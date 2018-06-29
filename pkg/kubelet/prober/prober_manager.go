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

package prober

import (
	"sync"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/status"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
)

// Manager manages pod probing. It creates a probe "worker" for every container that specifies a
// probe (AddPod). The worker periodically probes its assigned container and caches the results. The
// manager use the cached probe results to set the appropriate Ready state in the PodStatus when
// requested (UpdatePodStatus). Updating probe parameters is not currently supported.
// TODO: Move liveness probing out of the runtime, to here.
// Probe Manager负责探测pod，它会为每个设置了probe(AddPod)的容器创建一个probe worker
// 这个worker会定期探测它负责的容器并且缓存结果
// manager利用这些缓存的探测结果在被请求的时候设置PodStatus(UpdatePodStatus)
type Manager interface {
	// AddPod creates new probe workers for every container probe. This should be called for every
	// pod created.
	// AddPod为每个container probe创建一个新的probe worker，每个创建的pod都应该调用
	AddPod(pod *v1.Pod)

	// RemovePod handles cleaning up the removed pod state, including terminating probe workers and
	// deleting cached results.
	// RemovePod负责清除已经被移除的pod state，包括结束probe workers以及删除缓存的result
	RemovePod(pod *v1.Pod)

	// CleanupPods handles cleaning up pods which should no longer be running.
	// It takes a list of "active pods" which should not be cleaned up.
	// CleanupPods清理那些不应该再继续运行的pods
	// 它会接收一系列的"active pods"，它们不会被清除
	CleanupPods(activePods []*v1.Pod)

	// UpdatePodStatus modifies the given PodStatus with the appropriate Ready state for each
	// container based on container running status, cached probe results and worker states.
	// UpdatePodStatus修改给定的PodStatus，为每个容器设置合适的Ready state
	// 基于容器的运行状态，缓存的probe results以及worker states
	UpdatePodStatus(types.UID, *v1.PodStatus)

	// Start starts the Manager sync loops.
	Start()
}

type manager struct {
	// Map of active workers for probes
	// 用于探测的active workers
	workers map[probeKey]*worker
	// Lock for accessing & mutating workers
	workerLock sync.RWMutex

	// The statusManager cache provides pod IP and container IDs for probing.
	// statusManager cache提供pod IP以及container IDs用于probing
	statusManager status.Manager

	// readinessManager manages the results of readiness probes
	// readinessManager管理readiness probes的结果
	readinessManager results.Manager

	// livenessManager manages the results of liveness probes
	// livenessManager管理liveness probes的结果
	livenessManager results.Manager

	// prober executes the probe actions.
	// prober执行probe操作
	prober *prober
}

func NewManager(
	statusManager status.Manager,
	livenessManager results.Manager,
	runner kubecontainer.ContainerCommandRunner,
	refManager *kubecontainer.RefManager,
	recorder record.EventRecorder) Manager {

	prober := newProber(runner, refManager, recorder)
	readinessManager := results.NewManager()
	return &manager{
		statusManager:    statusManager,
		prober:           prober,
		readinessManager: readinessManager,
		livenessManager:  livenessManager,
		workers:          make(map[probeKey]*worker),
	}
}

// Start syncing probe status. This should only be called once.
// Start同步probe status, 本函数只能被调用一次
func (m *manager) Start() {
	// Start syncing readiness.
	// 开始同步readiness
	go wait.Forever(m.updateReadiness, 0)
}

// Key uniquely identifying container probes
// Key用于区分container probes
type probeKey struct {
	podUID        types.UID
	containerName string
	probeType     probeType
}

// Type of probe (readiness or liveness)
type probeType int

const (
	liveness probeType = iota
	readiness
)

// For debugging.
func (t probeType) String() string {
	switch t {
	case readiness:
		return "Readiness"
	case liveness:
		return "Liveness"
	default:
		return "UNKNOWN"
	}
}

func (m *manager) AddPod(pod *v1.Pod) {
	m.workerLock.Lock()
	defer m.workerLock.Unlock()

	key := probeKey{podUID: pod.UID}
	// 遍历pod的spec中的所有容器
	for _, c := range pod.Spec.Containers {
		key.containerName = c.Name

		// 如果定义了ReadinessProbe或者LivenessProbe
		// 则创建一个worker
		if c.ReadinessProbe != nil {
			// 设置probeKey的probeType为readiness
			key.probeType = readiness
			if _, ok := m.workers[key]; ok {
				glog.Errorf("Readiness probe already exists! %v - %v",
					format.Pod(pod), c.Name)
				return
			}
			// 创建新的worker
			w := newWorker(m, readiness, pod, c)
			m.workers[key] = w
			go w.run()
		}

		if c.LivenessProbe != nil {
			// 设置probe类型为liveness
			key.probeType = liveness
			if _, ok := m.workers[key]; ok {
				glog.Errorf("Liveness probe already exists! %v - %v",
					format.Pod(pod), c.Name)
				return
			}
			w := newWorker(m, liveness, pod, c)
			m.workers[key] = w
			go w.run()
		}
	}
}

func (m *manager) RemovePod(pod *v1.Pod) {
	m.workerLock.RLock()
	defer m.workerLock.RUnlock()

	key := probeKey{podUID: pod.UID}
	for _, c := range pod.Spec.Containers {
		key.containerName = c.Name
		for _, probeType := range [...]probeType{readiness, liveness} {
			key.probeType = probeType
			if worker, ok := m.workers[key]; ok {
				worker.stop()
			}
		}
	}
}

func (m *manager) CleanupPods(activePods []*v1.Pod) {
	desiredPods := make(map[types.UID]sets.Empty)
	for _, pod := range activePods {
		desiredPods[pod.UID] = sets.Empty{}
	}

	m.workerLock.RLock()
	defer m.workerLock.RUnlock()

	for key, worker := range m.workers {
		// 停止probe manager里面的worker
		if _, ok := desiredPods[key.podUID]; !ok {
			worker.stop()
		}
	}
}

func (m *manager) UpdatePodStatus(podUID types.UID, podStatus *v1.PodStatus) {
	for i, c := range podStatus.ContainerStatuses {
		var ready bool
		if c.State.Running == nil {
			ready = false
		} else if result, ok := m.readinessManager.Get(kubecontainer.ParseContainerID(c.ContainerID)); ok {
			ready = result == results.Success
		} else {
			// The check whether there is a probe which hasn't run yet.
			_, exists := m.getWorker(podUID, c.Name, readiness)
			ready = !exists
		}
		podStatus.ContainerStatuses[i].Ready = ready
	}
	// init containers are ready if they have exited with success or if a readiness probe has
	// succeeded.
	// init containers处于ready状态，如果他们成功退出，或者readiness probe成功
	for i, c := range podStatus.InitContainerStatuses {
		var ready bool
		if c.State.Terminated != nil && c.State.Terminated.ExitCode == 0 {
			ready = true
		}
		podStatus.InitContainerStatuses[i].Ready = ready
	}
}

func (m *manager) getWorker(podUID types.UID, containerName string, probeType probeType) (*worker, bool) {
	m.workerLock.RLock()
	defer m.workerLock.RUnlock()
	worker, ok := m.workers[probeKey{podUID, containerName, probeType}]
	return worker, ok
}

// Called by the worker after exiting.
func (m *manager) removeWorker(podUID types.UID, containerName string, probeType probeType) {
	m.workerLock.Lock()
	defer m.workerLock.Unlock()
	// 仅仅只是从m.workers中移除而已
	delete(m.workers, probeKey{podUID, containerName, probeType})
}

// workerCount returns the total number of probe workers. For testing.
func (m *manager) workerCount() int {
	m.workerLock.RLock()
	defer m.workerLock.RUnlock()
	return len(m.workers)
}

// probeManager启动的时候，会启动一个goroutine定时读取readinessManager中的数据
// 并调用statusManager去更新api server中的状态信息
// 负责service的组件获得了这个状态，就能根据不同的值来确定是否需要更新endpoints的内容
// 也就是service的请求能不能发送到这个pod
func (m *manager) updateReadiness() {
	update := <-m.readinessManager.Updates()

	// 判断readiness探测的结果是否为Success
	ready := update.Result == results.Success
	// 设置status manager的container readiness
	m.statusManager.SetContainerReadiness(update.PodUID, update.ContainerID, ready)
}
