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
	"math/rand"
	"time"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
)

// worker handles the periodic probing of its assigned container. Each worker has a go-routine
// associated with it which runs the probe loop until the container permanently terminates, or the
// stop channel is closed. The worker uses the probe Manager's statusManager to get up-to-date
// container IDs.
// worker负责处理它的容器的periodic probing
// 每个worker都有一个goroutine，它运行probe loop直到容器永远结束，或者stop channel被关闭
// worker使用probe Manager的status Manager去获取最新的container ID
type worker struct {
	// Channel for stopping the probe.
	stopCh chan struct{}

	// The pod containing this probe (read-only)
	pod *v1.Pod

	// The container to probe (read-only)
	container v1.Container

	// Describes the probe configuration (read-only)
	spec *v1.Probe

	// The type of the worker.
	probeType probeType

	// The probe value during the initial delay.
	initialValue results.Result

	// Where to store this workers results.
	// 用于存储worker results
	resultsManager results.Manager
	probeManager   *manager

	// The last known container ID for this worker.
	containerID kubecontainer.ContainerID
	// The last probe result for this worker.
	// 该worker上次的probe的result
	lastResult results.Result
	// How many times in a row the probe has returned the same result.
	// 同一个probe结果已经返回了多少次
	resultRun int

	// If set, skip probing.
	// 如果onHold设置的haul，则跳过probing
	onHold bool
}

// Creates and starts a new probe worker.
func newWorker(
	m *manager,
	probeType probeType,
	pod *v1.Pod,
	container v1.Container) *worker {

	w := &worker{
		// 一个1的缓存，因此stop()函数就不会被阻塞
		stopCh:       make(chan struct{}, 1), // Buffer so stop() can be non-blocking.
		pod:          pod,
		container:    container,
		probeType:    probeType,
		probeManager: m,
	}

	switch probeType {
	// 只有readiness和liveness这两种情况
	case readiness:
		w.spec = container.ReadinessProbe
		w.resultsManager = m.readinessManager
		// readiness默认的initialValue位Failure
		w.initialValue = results.Failure
	case liveness:
		w.spec = container.LivenessProbe
		w.resultsManager = m.livenessManager
		// liveness默认的initialValue为Success
		w.initialValue = results.Success
	}

	return w
}

// run periodically probes the container.
func (w *worker) run() {
	probeTickerPeriod := time.Duration(w.spec.PeriodSeconds) * time.Second

	// If kubelet restarted the probes could be started in rapid succession.
	// Let the worker wait for a random portion of tickerPeriod before probing.
	time.Sleep(time.Duration(rand.Float64() * float64(probeTickerPeriod)))

	probeTicker := time.NewTicker(probeTickerPeriod)

	defer func() {
		// Clean up.
		probeTicker.Stop()
		if !w.containerID.IsEmpty() {
			w.resultsManager.Remove(w.containerID)
		}

		// 从probeManager中移除worker
		w.probeManager.removeWorker(w.pod.UID, w.container.Name, w.probeType)
	}()

probeLoop:
	for w.doProbe() {
		// Wait for next probe tick.
		select {
		case <-w.stopCh:
			break probeLoop
		case <-probeTicker.C:
			// continue
		}
	}
}

// stop stops the probe worker. The worker handles cleanup and removes itself from its manager.
// It is safe to call stop multiple times.
// stop停止probe worker
// worker负责清理并且从manager中移除自己
// 多次调用本函数是安全的
func (w *worker) stop() {
	select {
	case w.stopCh <- struct{}{}:
	default: // Non-blocking.
	}
}

// doProbe probes the container once and records the result.
// Returns whether the worker should continue.
// doProbe探测一次容器并且记录结果
// 返回worker是否继续
func (w *worker) doProbe() (keepGoing bool) {
	defer func() { recover() }() // Actually eat panics (HandleCrash takes care of logging)
	// HandleCrash()会负责处理日志的工作
	defer runtime.HandleCrash(func(_ interface{}) { keepGoing = true })

	// 获取pod的status
	status, ok := w.probeManager.statusManager.GetPodStatus(w.pod.UID)
	// 从status manager中找不到status，则pod还未被创建，或者已经删除了
	if !ok {
		// Either the pod has not been created yet, or it was already deleted.
		glog.V(3).Infof("No status for pod: %v", format.Pod(w.pod))
		return true
	}

	// Worker should terminate if pod is terminated.
	if status.Phase == v1.PodFailed || status.Phase == v1.PodSucceeded {
		glog.V(3).Infof("Pod %v %v, exiting probe worker",
			format.Pod(w.pod), status.Phase)
		return false
	}

	// 获取pod中容器的状态
	c, ok := podutil.GetContainerStatus(status.ContainerStatuses, w.container.Name)
	if !ok || len(c.ContainerID) == 0 {
		// Either the container has not been created yet, or it was deleted.
		glog.V(3).Infof("Probe target container not found: %v - %v",
			format.Pod(w.pod), w.container.Name)
		return true // Wait for more information.
	}

	// pod更新了容器，使用最新的容器
	if w.containerID.String() != c.ContainerID {
		if !w.containerID.IsEmpty() {
			w.resultsManager.Remove(w.containerID)
		}
		w.containerID = kubecontainer.ParseContainerID(c.ContainerID)
		w.resultsManager.Set(w.containerID, w.initialValue, w.pod)
		// We've got a new container; resume probing.
		w.onHold = false
	}

	if w.onHold {
		// Worker is on hold until there is a new container.
		// 保持worker，直到有新的容器被创建
		return true
	}

	if c.State.Running == nil {
		glog.V(3).Infof("Non-running container probed: %v - %v",
			format.Pod(w.pod), w.container.Name)
		if !w.containerID.IsEmpty() {
			w.resultsManager.Set(w.containerID, results.Failure, w.pod)
		}
		// Abort if the container will not be restarted.
		// 如果容器不会再被重启了，就退出
		return c.State.Terminated == nil ||
			w.pod.Spec.RestartPolicy != v1.RestartPolicyNever
	}

	// 容器启动时间太短，没有超过配置的初始化等待时间InitialDelaySeconds
	if int32(time.Since(c.State.Running.StartedAt.Time).Seconds()) < w.spec.InitialDelaySeconds {
		return true
	}

	// TODO: in order for exec probes to correctly handle downward API env, we must be able to reconstruct
	// the full container environment here, OR we must make a call to the CRI in order to get those environment
	// values from the running container.
	// 调用probe检测容器状态
	result, err := w.probeManager.prober.probe(w.probeType, w.pod, status, w.container, w.containerID)
	if err != nil {
		// Prober error, throw away the result.
		return true
	}

	if w.lastResult == result {
		w.resultRun++
	} else {
		w.lastResult = result
		w.resultRun = 1
	}

	if (result == results.Failure && w.resultRun < int(w.spec.FailureThreshold)) ||
		(result == results.Success && w.resultRun < int(w.spec.SuccessThreshold)) {
		// Success or failure is below threshold - leave the probe state unchanged.
		// 如果Sucess或者failure的次数小于threshold，则保持probe state不变
		return true
	}

	// 设置result manager
	w.resultsManager.Set(w.containerID, result, w.pod)

	if w.probeType == liveness && result == results.Failure {
		// The container fails a liveness check, it will need to be restarted.
		// 如果容器没有通过liveness check，它需要被重启
		// Stop probing until we see a new container ID. This is to reduce the
		// chance of hitting #21751, where running `docker exec` when a
		// container is being stopped may lead to corrupted container state.
		w.onHold = true
		w.resultRun = 1
	}

	return true
}
