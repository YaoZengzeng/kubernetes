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

package types

import (
	"fmt"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeapi "k8s.io/kubernetes/pkg/apis/core"
)

const (
	ConfigSourceAnnotationKey    = "kubernetes.io/config.source"
	ConfigMirrorAnnotationKey    = v1.MirrorPodAnnotationKey
	ConfigFirstSeenAnnotationKey = "kubernetes.io/config.seen"
	ConfigHashAnnotationKey      = "kubernetes.io/config.hash"
	CriticalPodAnnotationKey     = "scheduler.alpha.kubernetes.io/critical-pod"
)

// PodOperation defines what changes will be made on a pod configuration.
// PodOperation定义了会对一个pod configuration做什么改变
type PodOperation int

const (
	// This is the current pod configuration
	SET PodOperation = iota
	// Pods with the given ids are new to this source
	ADD
	// Pods with the given ids are gracefully deleted from this source
	DELETE
	// Pods with the given ids have been removed from this source
	REMOVE
	// Pods with the given ids have been updated in this source
	UPDATE
	// Pods with the given ids have unexpected status in this source,
	// kubelet should reconcile status with this source
	// 该source中，给定id的pod有着不符合预期的状态，kubelet应该对它进行调整
	RECONCILE
	// Pods with the given ids have been restored from a checkpoint.
	// 给定id的pods从checkpoint中恢复过来
	RESTORE

	// These constants identify the sources of pods
	// 以下这些常量包含了pod的source
	// Updates from a file
	FileSource = "file"
	// Updates from querying a web page
	HTTPSource = "http"
	// Updates from Kubernetes API Server
	ApiserverSource = "api"
	// Updates from all sources
	AllSource = "*"

	NamespaceDefault = metav1.NamespaceDefault
)

// PodUpdate defines an operation sent on the channel. You can add or remove single services by
// sending an array of size one and Op == ADD|REMOVE (with REMOVE, only the ID is required).
// For setting the state of the system to a given state for this source configuration, set
// Pods as desired and Op to SET, which will reset the system state to that specified in this
// operation for this source channel. To remove all pods, set Pods to empty object and Op to SET.
// 如果想要增加或者移除一个服务，可以发送一个size为1的array，并且将Op设置为ADD或者REMOVE
// 如果想让这个source configuration设置到给定状态，那就按要求设置Pods，并且将Op设置为SET
// 如果要移除所有的pod，将Pods设置为empty object并且将Op设置为SET
//
// Additionally, Pods should never be nil - it should always point to an empty slice. While
// functionally similar, this helps our unit tests properly check that the correct PodUpdates
// are generated.
// Pods永远不能为nil，它应该指向一个empty slice
// 尽管功能是相似的，它能帮助我们的单元测试更好地检查，正确的PodUpdates已经被创建了
type PodUpdate struct {
	// 完整的Pods信息
	Pods   []*v1.Pod
	// 具体的操作
	Op     PodOperation
	// PodUpdate的来源
	Source string
}

// Gets all validated sources from the specified sources.
func GetValidatedSources(sources []string) ([]string, error) {
	validated := make([]string, 0, len(sources))
	for _, source := range sources {
		switch source {
		case AllSource:
			return []string{FileSource, HTTPSource, ApiserverSource}, nil
		case FileSource, HTTPSource, ApiserverSource:
			validated = append(validated, source)
			break
		case "":
			break
		default:
			return []string{}, fmt.Errorf("unknown pod source %q", source)
		}
	}
	return validated, nil
}

// GetPodSource returns the source of the pod based on the annotation.
// GetPodSource根据annotation返回pod的source
func GetPodSource(pod *v1.Pod) (string, error) {
	if pod.Annotations != nil {
		if source, ok := pod.Annotations[ConfigSourceAnnotationKey]; ok {
			return source, nil
		}
	}
	return "", fmt.Errorf("cannot get source of pod %q", pod.UID)
}

// SyncPodType classifies pod updates, eg: create, update.
// SyncPodType区分pod updates的类型
type SyncPodType int

const (
	// SyncPodSync is when the pod is synced to ensure desired state
	// 需要将pod同步到期望的状态
	SyncPodSync SyncPodType = iota
	// SyncPodUpdate is when the pod is updated from source
	// 从source获取到pod的更新
	SyncPodUpdate
	// SyncPodCreate is when the pod is created from source
	// 从source获取到pod被创建
	SyncPodCreate
	// SyncPodKill is when the pod is killed based on a trigger internal to the kubelet for eviction.
	// kubelet用于驱逐的内部的trigger
	// If a SyncPodKill request is made to pod workers, the request is never dropped, and will always be processed.
	// 如果一个SyncPodKill请求发送到pod workers，该请求不会被丢弃，它永远都会被处理
	SyncPodKill
)

func (sp SyncPodType) String() string {
	switch sp {
	case SyncPodCreate:
		return "create"
	case SyncPodUpdate:
		return "update"
	case SyncPodSync:
		return "sync"
	case SyncPodKill:
		return "kill"
	default:
		return "unknown"
	}
}

// IsCriticalPod returns true if the pod bears the critical pod annotation
// key. Both the rescheduler and the kubelet use this key to make admission
// and scheduling decisions.
func IsCriticalPod(pod *v1.Pod) bool {
	return IsCritical(pod.Namespace, pod.Annotations)
}

// IsCritical returns true if parameters bear the critical pod annotation
// key. The DaemonSetController use this key directly to make scheduling decisions.
func IsCritical(ns string, annotations map[string]string) bool {
	// Critical pods are restricted to "kube-system" namespace as of now.
	if ns != kubeapi.NamespaceSystem {
		return false
	}
	val, ok := annotations[CriticalPodAnnotationKey]
	if ok && val == "" {
		return true
	}
	return false
}
