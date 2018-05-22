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

package lifecycle

import "k8s.io/api/core/v1"

// PodAdmitAttributes is the context for a pod admission decision.
// The member fields of this struct should never be mutated.
// PodAdmitAttributes包含了做出一个pod admission decision的上下文
// 这个结构中的成员结构不能随意改变
type PodAdmitAttributes struct {
	// the pod to evaluate for admission
	Pod *v1.Pod
	// all pods bound to the kubelet excluding the pod being evaluated
	// 除了被评估的pod以外，所有和kubelet绑定的pod
	OtherPods []*v1.Pod
}

// PodAdmitResult provides the result of a pod admission decision.
// PodAdmitResult提供了一个pod admission decision的结果
type PodAdmitResult struct {
	// if true, the pod should be admitted.
	Admit bool
	// a brief single-word reason why the pod could not be admitted.
	Reason string
	// a brief message explaining why the pod could not be admitted.
	Message string
}

// PodAdmitHandler is notified during pod admission.
// PodAdmitHandler在pod admission才被调用
type PodAdmitHandler interface {
	// Admit evaluates if a pod can be admitted.
	// Admit评估一个pod是否能被admitted
	Admit(attrs *PodAdmitAttributes) PodAdmitResult
}

// PodAdmitTarget maintains a list of handlers to invoke.
// PodAdmitTarget维护了一系列要被调用的handlers
type PodAdmitTarget interface {
	// AddPodAdmitHandler adds the specified handler.
	AddPodAdmitHandler(a PodAdmitHandler)
}

// PodSyncLoopHandler is invoked during each sync loop iteration.
// PodSyncLoopHandler在每个sync loop iteration被调用
type PodSyncLoopHandler interface {
	// ShouldSync returns true if the pod needs to be synced.
	// This operation must return immediately as its called for each pod.
	// The provided pod should never be modified.
	// ShouldSync返回true，如果pod需要被sync的话
	// 这个操作必须马上返回，因为它会被每个pod所调用
	// 提供的pod不能被修改
	ShouldSync(pod *v1.Pod) bool
}

// PodSyncLoopTarget maintains a list of handlers to pod sync loop.
type PodSyncLoopTarget interface {
	// AddPodSyncLoopHandler adds the specified handler.
	AddPodSyncLoopHandler(a PodSyncLoopHandler)
}

// ShouldEvictResponse provides the result of a should evict request.
// ShouldEvictResponse提供了一个需要被evict的request的result
type ShouldEvictResponse struct {
	// if true, the pod should be evicted.
	Evict bool
	// a brief CamelCase reason why the pod should be evicted.
	Reason string
	// a brief message why the pod should be evicted.
	Message string
}

// PodSyncHandler is invoked during each sync pod operation.
// PodSyncHandler在每次同步pod的时候被调用
type PodSyncHandler interface {
	// ShouldEvict is invoked during each sync pod operation to determine
	// if the pod should be evicted from the kubelet.  If so, the pod status
	// is updated to mark its phase as failed with the provided reason and message,
	// and the pod is immediately killed.
	// This operation must return immediately as its called for each sync pod.
	// The provided pod should never be modified.
	// ShouldEvict在每次sync pod操作中被调用，用于确定pod是否需要从kubelet中驱逐出去
	// 如果是的话，pod status会被更新并且标记它的phase为因为指定的reason和message而fail
	// 并且pod会马上被kill
	// 本操作必须马上返回，因为它会被每个同步的pod所调用
	// 提供的pod不能被修改
	ShouldEvict(pod *v1.Pod) ShouldEvictResponse
}

// PodSyncTarget maintains a list of handlers to pod sync.
type PodSyncTarget interface {
	// AddPodSyncHandler adds the specified handler
	AddPodSyncHandler(a PodSyncHandler)
}

// PodLifecycleTarget groups a set of lifecycle interfaces for convenience.
type PodLifecycleTarget interface {
	PodAdmitTarget
	PodSyncLoopTarget
	PodSyncTarget
}

// PodAdmitHandlers maintains a list of handlers to pod admission.
type PodAdmitHandlers []PodAdmitHandler

// AddPodAdmitHandler adds the specified observer.
func (handlers *PodAdmitHandlers) AddPodAdmitHandler(a PodAdmitHandler) {
	*handlers = append(*handlers, a)
}

// PodSyncLoopHandlers maintains a list of handlers to pod sync loop.
type PodSyncLoopHandlers []PodSyncLoopHandler

// AddPodSyncLoopHandler adds the specified observer.
func (handlers *PodSyncLoopHandlers) AddPodSyncLoopHandler(a PodSyncLoopHandler) {
	*handlers = append(*handlers, a)
}

// PodSyncHandlers maintains a list of handlers to pod sync.
type PodSyncHandlers []PodSyncHandler

// AddPodSyncHandler adds the specified handler.
func (handlers *PodSyncHandlers) AddPodSyncHandler(a PodSyncHandler) {
	*handlers = append(*handlers, a)
}
