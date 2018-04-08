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

package pod

import (
	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
)

// MirrorClient knows how to create/delete a mirror pod in the API server.
// MirrorClient知道如何在API server里创建/删除一个mirror pod
type MirrorClient interface {
	// CreateMirrorPod creates a mirror pod in the API server for the given
	// pod or returns an error.  The mirror pod will have the same annotations
	// as the given pod as well as an extra annotation containing the hash of
	// the static pod.
	// CreateMirrorPod为给定的pod在API server里创建一个mirror pod，或者返回一个错误
	// mirror pod和给定的pod有相同的annotations并且有一个额外的annotation包含static
	// pod的哈希
	CreateMirrorPod(pod *v1.Pod) error
	// DeleteMirrorPod deletes the mirror pod with the given full name from
	// the API server or returns an error.
	DeleteMirrorPod(podFullName string) error
}

// basicMirrorClient is a functional MirrorClient.  Mirror pods are stored in
// the kubelet directly because they need to be in sync with the internal
// pods.
// basicMirrorClient是一个functional MirrorClient
// Mirror pods直接存储在kubelet中，因为它们需要和internal pods同步
type basicMirrorClient struct {
	apiserverClient clientset.Interface
}

// NewBasicMirrorClient returns a new MirrorClient.
func NewBasicMirrorClient(apiserverClient clientset.Interface) MirrorClient {
	return &basicMirrorClient{apiserverClient: apiserverClient}
}

func (mc *basicMirrorClient) CreateMirrorPod(pod *v1.Pod) error {
	if mc.apiserverClient == nil {
		return nil
	}
	// Make a copy of the pod.
	// 直接拷贝pod
	copyPod := *pod
	// 重置pod中的annotations
	copyPod.Annotations = make(map[string]string)

	for k, v := range pod.Annotations {
		copyPod.Annotations[k] = v
	}
	// 每个static pod都有一个key为ConfigHashAnnotationKey
	// 的annotations,hash就是它的value
	hash := getPodHash(pod)
	// 为mirror pod多增加一个annotation
	copyPod.Annotations[kubetypes.ConfigMirrorAnnotationKey] = hash
	// 调用api server的client创建pod
	apiPod, err := mc.apiserverClient.CoreV1().Pods(copyPod.Namespace).Create(&copyPod)
	if err != nil && errors.IsAlreadyExists(err) {
		// Check if the existing pod is the same as the pod we want to create.
		if h, ok := apiPod.Annotations[kubetypes.ConfigMirrorAnnotationKey]; ok && h == hash {
			return nil
		}
	}
	return err
}

func (mc *basicMirrorClient) DeleteMirrorPod(podFullName string) error {
	if mc.apiserverClient == nil {
		return nil
	}
	name, namespace, err := kubecontainer.ParsePodFullName(podFullName)
	if err != nil {
		glog.Errorf("Failed to parse a pod full name %q", podFullName)
		return err
	}
	glog.V(2).Infof("Deleting a mirror pod %q", podFullName)
	// TODO(random-liu): Delete the mirror pod with uid precondition in mirror pod manager
	if err := mc.apiserverClient.CoreV1().Pods(namespace).Delete(name, metav1.NewDeleteOptions(0)); err != nil && !errors.IsNotFound(err) {
		glog.Errorf("Failed deleting a mirror pod %q: %v", podFullName, err)
	}
	return nil
}

func IsStaticPod(pod *v1.Pod) bool {
	source, err := kubetypes.GetPodSource(pod)
	return err == nil && source != kubetypes.ApiserverSource
}

func IsMirrorPod(pod *v1.Pod) bool {
	_, ok := pod.Annotations[kubetypes.ConfigMirrorAnnotationKey]
	return ok
}

func getHashFromMirrorPod(pod *v1.Pod) (string, bool) {
	hash, ok := pod.Annotations[kubetypes.ConfigMirrorAnnotationKey]
	return hash, ok
}

func getPodHash(pod *v1.Pod) string {
	// The annotation exists for all static pods.
	return pod.Annotations[kubetypes.ConfigHashAnnotationKey]
}
