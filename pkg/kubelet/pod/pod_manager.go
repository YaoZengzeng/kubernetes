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
	"sync"

	"github.com/golang/glog"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/kubelet/checkpoint"
	"k8s.io/kubernetes/pkg/kubelet/configmap"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/secret"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
)

// Manager stores and manages access to pods, maintaining the mappings
// between static pods and mirror pods.
// Manager存储和管理对于pods的访问，维护了static pods和mirror pods之间的映射
//
// The kubelet discovers pod updates from 3 sources: file, http, and
// apiserver. Pods from non-apiserver sources are called static pods, and API
// server is not aware of the existence of static pods. In order to monitor
// the status of such pods, the kubelet creates a mirror pod for each static
// pod via the API server.
// kubelet从3个地方获取pod的更新：file, http以及apiserver
// 从非apiserver处获取的pods被称为static pods，apiserver不会意识到这些static pods的存在
// 为了监视这些pods的状态，kubelet会通过apiserver为每个static pod创建mirror pod
//
// A mirror pod has the same pod full name (name and namespace) as its static
// counterpart (albeit different metadata such as UID, etc). By leveraging the
// fact that the kubelet reports the pod status using the pod full name, the
// status of the mirror pod always reflects the actual status of the static
// pod. When a static pod gets deleted, the associated orphaned mirror pod
// will also be removed.
// 一个mirror pod和它对应的static pod有着相同的full name(name以及namespace)，当然例如UID
// 这些元数据是不同的。通过利用kubelet是使用pod的full name来汇报pod status
// 所以mirror pod的状态总是反应了static pod的真实状态
// 当一个static pod被删除了之后，相关的orphaned mirror pod也会被移除
type Manager interface {
	// GetPods returns the regular pods bound to the kubelet and their spec.
	GetPods() []*v1.Pod
	// GetPodByFullName returns the (non-mirror) pod that matches full name, as well as
	// whether the pod was found.
	GetPodByFullName(podFullName string) (*v1.Pod, bool)
	// GetPodByName provides the (non-mirror) pod that matches namespace and
	// name, as well as whether the pod was found.
	GetPodByName(namespace, name string) (*v1.Pod, bool)
	// GetPodByUID provides the (non-mirror) pod that matches pod UID, as well as
	// whether the pod is found.
	GetPodByUID(types.UID) (*v1.Pod, bool)
	// GetPodByMirrorPod returns the static pod for the given mirror pod and
	// whether it was known to the pod manger.
	// GetPodByMirrorPod根据给定的mirror pod返回static pod
	GetPodByMirrorPod(*v1.Pod) (*v1.Pod, bool)
	// GetMirrorPodByPod returns the mirror pod for the given static pod and
	// whether it was known to the pod manager.
	GetMirrorPodByPod(*v1.Pod) (*v1.Pod, bool)
	// GetPodsAndMirrorPods returns the both regular and mirror pods.
	GetPodsAndMirrorPods() ([]*v1.Pod, []*v1.Pod)
	// SetPods replaces the internal pods with the new pods.
	// It is currently only used for testing.
	// SetPods用新的pods替换internal pods
	// 现在它只用于测试
	SetPods(pods []*v1.Pod)
	// AddPod adds the given pod to the manager.
	AddPod(pod *v1.Pod)
	// UpdatePod updates the given pod in the manager.
	UpdatePod(pod *v1.Pod)
	// DeletePod deletes the given pod from the manager.  For mirror pods,
	// this means deleting the mappings related to mirror pods.  For non-
	// mirror pods, this means deleting from indexes for all non-mirror pods.
	// DeletePod删除给定pod，对于mirror pods，这意味着删除和mirror pods之间的映射
	// 对于非mirror pods, 这意味着从所有的非mirror pods中删除
	DeletePod(pod *v1.Pod)
	// DeleteOrphanedMirrorPods deletes all mirror pods which do not have
	// associated static pods. This method sends deletion requests to the API
	// server, but does NOT modify the internal pod storage in basicManager.
	// DeleteOrphanedMirrorPods删除所有没有关联的static pods的所有mirror pods
	// 该方法将deletion requests发送给api server，但是不会修改basicManager里的
	// internal pod的存储
	DeleteOrphanedMirrorPods()
	// TranslatePodUID returns the actual UID of a pod. If the UID belongs to
	// a mirror pod, returns the UID of its static pod. Otherwise, returns the
	// original UID.
	//
	// All public-facing functions should perform this translation for UIDs
	// because user may provide a mirror pod UID, which is not recognized by
	// internal Kubelet functions.
	// TranslatePodUID返回pod的真实UID，如果UID属于一个mirror pod, 则返回它的static pod
	// 的UID，否则，返回original UID
	//
	// 所有public-facing的函数都要对UID实行这样的转换，因为用户可能会提供一个mirror pod
	// 的UID，它不能被内部的kubelet函数识别
	TranslatePodUID(uid types.UID) kubetypes.ResolvedPodUID
	// GetUIDTranslations returns the mappings of static pod UIDs to mirror pod
	// UIDs and mirror pod UIDs to static pod UIDs.
	// GetUIDTranslations返回static pod UID到mirror pod UID的映射
	// 以及mirror pod UID到static pod UID的映射
	GetUIDTranslations() (podToMirror map[kubetypes.ResolvedPodUID]kubetypes.MirrorPodUID, mirrorToPod map[kubetypes.MirrorPodUID]kubetypes.ResolvedPodUID)
	// IsMirrorPodOf returns true if mirrorPod is a correct representation of
	// pod; false otherwise.
	// IsMIrrorPodOf返回是否mirrorPod是不是正确代表了pod
	IsMirrorPodOf(mirrorPod, pod *v1.Pod) bool

	MirrorClient
}

// basicManager is a functional Manager.
//
// All fields in basicManager are read-only and are updated calling SetPods,
// AddPod, UpdatePod, or DeletePod.
// basicManager是一个functional Manager
// basicManager中所有的字段都是只读的并且在调用SetPods，AddPods，UpdatePod或者DeletePod
// 的时候更新
type basicManager struct {
	// Protects all internal maps.
	lock sync.RWMutex

	// Regular pods indexed by UID.
	// 通过uid找到pod
	podByUID map[kubetypes.ResolvedPodUID]*v1.Pod
	// Mirror pods indexed by UID.
	mirrorPodByUID map[kubetypes.MirrorPodUID]*v1.Pod

	// Pods indexed by full name for easy access.
	// 通过pod的full name找到pod
	podByFullName       map[string]*v1.Pod
	mirrorPodByFullName map[string]*v1.Pod

	// Mirror pod UID to pod UID map.
	// mirror pod UID到pod UID的映射
	translationByUID map[kubetypes.MirrorPodUID]kubetypes.ResolvedPodUID

	// basicManager is keeping secretManager and configMapManager up-to-date.
	// basicManager保持secretManager和configMapManager到最新状态
	secretManager     secret.Manager
	configMapManager  configmap.Manager
	checkpointManager checkpoint.Manager

	// A mirror pod client to create/delete mirror pods.
	// mirror pod client用于创建/删除mirrior pods
	MirrorClient
}

// NewBasicPodManager returns a functional Manager.
// NewBasicPodManager返回一个functional Manager
func NewBasicPodManager(client MirrorClient, secretManager secret.Manager, configMapManager configmap.Manager) Manager {
	pm := &basicManager{}
	pm.secretManager = secretManager
	pm.configMapManager = configMapManager
	pm.checkpointManager = checkpoint.GetInstance()
	pm.MirrorClient = client
	// 对pm进行初始化，新建各个map
	pm.SetPods(nil)
	return pm
}

// Set the internal pods based on the new pods.
func (pm *basicManager) SetPods(newPods []*v1.Pod) {
	pm.lock.Lock()
	defer pm.lock.Unlock()

	pm.podByUID = make(map[kubetypes.ResolvedPodUID]*v1.Pod)
	pm.podByFullName = make(map[string]*v1.Pod)
	pm.mirrorPodByUID = make(map[kubetypes.MirrorPodUID]*v1.Pod)
	pm.mirrorPodByFullName = make(map[string]*v1.Pod)
	pm.translationByUID = make(map[kubetypes.MirrorPodUID]kubetypes.ResolvedPodUID)

	pm.updatePodsInternal(newPods...)
}

func (pm *basicManager) AddPod(pod *v1.Pod) {
	// AddPod直接调用UpdatePod
	pm.UpdatePod(pod)
}

func (pm *basicManager) UpdatePod(pod *v1.Pod) {
	pm.lock.Lock()
	defer pm.lock.Unlock()
	pm.updatePodsInternal(pod)
	if pm.checkpointManager != nil {
		// 将pod写入checkpoint manager中
		if err := pm.checkpointManager.WritePod(pod); err != nil {
			glog.Errorf("Error writing checkpoint for pod: %v", pod.GetName())
		}
	}
}

// updatePodsInternal replaces the given pods in the current state of the
// manager, updating the various indices. The caller is assumed to hold the
// lock.
// updatePodsInternal更新当前manager里给定的pods，更新各种索引
func (pm *basicManager) updatePodsInternal(pods ...*v1.Pod) {
	for _, pod := range pods {
		// 在secret manager和configMap manager对pod进行注册
		if pm.secretManager != nil {
			// TODO: Consider detecting only status update and in such case do
			// not register pod, as it doesn't really matter.
			pm.secretManager.RegisterPod(pod)
		}
		if pm.configMapManager != nil {
			// TODO: Consider detecting only status update and in such case do
			// not register pod, as it doesn't really matter.
			pm.configMapManager.RegisterPod(pod)
		}
		// 获取pod的full name
		podFullName := kubecontainer.GetPodFullName(pod)
		// This logic relies on a static pod and its mirror to have the same name.
		// It is safe to type convert here due to the IsMirrorPod guard.
		// 通过pod的annotation里面有没有MirrorPodAnnotationKey来判断是否为mirror pod
		if IsMirrorPod(pod) {
			// 如果是mirror pod
			mirrorPodUID := kubetypes.MirrorPodUID(pod.UID)
			// 更新一些索引值
			pm.mirrorPodByUID[mirrorPodUID] = pod
			pm.mirrorPodByFullName[podFullName] = pod
			if p, ok := pm.podByFullName[podFullName]; ok {
				pm.translationByUID[mirrorPodUID] = kubetypes.ResolvedPodUID(p.UID)
			}
		} else {
			resolvedPodUID := kubetypes.ResolvedPodUID(pod.UID)
			pm.podByUID[resolvedPodUID] = pod
			pm.podByFullName[podFullName] = pod
			// 用podFullName获取mirror pod，再存储mirror pod到resolvedPodUID的映射
			if mirror, ok := pm.mirrorPodByFullName[podFullName]; ok {
				pm.translationByUID[kubetypes.MirrorPodUID(mirror.UID)] = resolvedPodUID
			}
		}
	}
}

func (pm *basicManager) DeletePod(pod *v1.Pod) {
	pm.lock.Lock()
	defer pm.lock.Unlock()
	if pm.secretManager != nil {
		pm.secretManager.UnregisterPod(pod)
	}
	if pm.configMapManager != nil {
		pm.configMapManager.UnregisterPod(pod)
	}
	podFullName := kubecontainer.GetPodFullName(pod)
	// It is safe to type convert here due to the IsMirrorPod guard.
	if IsMirrorPod(pod) {
		mirrorPodUID := kubetypes.MirrorPodUID(pod.UID)
		delete(pm.mirrorPodByUID, mirrorPodUID)
		delete(pm.mirrorPodByFullName, podFullName)
		delete(pm.translationByUID, mirrorPodUID)
	} else {
		delete(pm.podByUID, kubetypes.ResolvedPodUID(pod.UID))
		delete(pm.podByFullName, podFullName)
	}
	if pm.checkpointManager != nil {
		if err := pm.checkpointManager.DeletePod(pod); err != nil {
			glog.Errorf("Error deleting checkpoint for pod: %v", pod.GetName())
		}
	}
}

func (pm *basicManager) GetPods() []*v1.Pod {
	pm.lock.RLock()
	defer pm.lock.RUnlock()
	return podsMapToPods(pm.podByUID)
}

func (pm *basicManager) GetPodsAndMirrorPods() ([]*v1.Pod, []*v1.Pod) {
	pm.lock.RLock()
	defer pm.lock.RUnlock()
	pods := podsMapToPods(pm.podByUID)
	mirrorPods := mirrorPodsMapToMirrorPods(pm.mirrorPodByUID)
	return pods, mirrorPods
}

func (pm *basicManager) GetPodByUID(uid types.UID) (*v1.Pod, bool) {
	pm.lock.RLock()
	defer pm.lock.RUnlock()
	pod, ok := pm.podByUID[kubetypes.ResolvedPodUID(uid)] // Safe conversion, map only holds non-mirrors.
	return pod, ok
}

func (pm *basicManager) GetPodByName(namespace, name string) (*v1.Pod, bool) {
	podFullName := kubecontainer.BuildPodFullName(name, namespace)
	return pm.GetPodByFullName(podFullName)
}

func (pm *basicManager) GetPodByFullName(podFullName string) (*v1.Pod, bool) {
	pm.lock.RLock()
	defer pm.lock.RUnlock()
	pod, ok := pm.podByFullName[podFullName]
	return pod, ok
}

func (pm *basicManager) TranslatePodUID(uid types.UID) kubetypes.ResolvedPodUID {
	// It is safe to type convert to a resolved UID because type conversion is idempotent.
	if uid == "" {
		return kubetypes.ResolvedPodUID(uid)
	}

	pm.lock.RLock()
	defer pm.lock.RUnlock()
	if translated, ok := pm.translationByUID[kubetypes.MirrorPodUID(uid)]; ok {
		return translated
	}
	return kubetypes.ResolvedPodUID(uid)
}

func (pm *basicManager) GetUIDTranslations() (podToMirror map[kubetypes.ResolvedPodUID]kubetypes.MirrorPodUID,
	mirrorToPod map[kubetypes.MirrorPodUID]kubetypes.ResolvedPodUID) {
	pm.lock.RLock()
	defer pm.lock.RUnlock()

	podToMirror = make(map[kubetypes.ResolvedPodUID]kubetypes.MirrorPodUID, len(pm.translationByUID))
	mirrorToPod = make(map[kubetypes.MirrorPodUID]kubetypes.ResolvedPodUID, len(pm.translationByUID))
	// Insert empty translation mapping for all static pods.
	for uid, pod := range pm.podByUID {
		if !IsStaticPod(pod) {
			continue
		}
		podToMirror[uid] = ""
	}
	// Fill in translations. Notice that if there is no mirror pod for a
	// static pod, its uid will be translated into empty string "". This
	// is WAI, from the caller side we can know that the static pod doesn't
	// have a corresponding mirror pod instead of using static pod uid directly.
	for k, v := range pm.translationByUID {
		mirrorToPod[k] = v
		podToMirror[v] = k
	}
	return podToMirror, mirrorToPod
}

func (pm *basicManager) getOrphanedMirrorPodNames() []string {
	pm.lock.RLock()
	defer pm.lock.RUnlock()
	var podFullNames []string
	for podFullName := range pm.mirrorPodByFullName {
		if _, ok := pm.podByFullName[podFullName]; !ok {
			podFullNames = append(podFullNames, podFullName)
		}
	}
	return podFullNames
}

func (pm *basicManager) DeleteOrphanedMirrorPods() {
	podFullNames := pm.getOrphanedMirrorPodNames()
	for _, podFullName := range podFullNames {
		pm.MirrorClient.DeleteMirrorPod(podFullName)
	}
}

func (pm *basicManager) IsMirrorPodOf(mirrorPod, pod *v1.Pod) bool {
	// Check name and namespace first.
	if pod.Name != mirrorPod.Name || pod.Namespace != mirrorPod.Namespace {
		return false
	}
	hash, ok := getHashFromMirrorPod(mirrorPod)
	if !ok {
		return false
	}
	return hash == getPodHash(pod)
}

func podsMapToPods(UIDMap map[kubetypes.ResolvedPodUID]*v1.Pod) []*v1.Pod {
	pods := make([]*v1.Pod, 0, len(UIDMap))
	for _, pod := range UIDMap {
		pods = append(pods, pod)
	}
	return pods
}

func mirrorPodsMapToMirrorPods(UIDMap map[kubetypes.MirrorPodUID]*v1.Pod) []*v1.Pod {
	pods := make([]*v1.Pod, 0, len(UIDMap))
	for _, pod := range UIDMap {
		pods = append(pods, pod)
	}
	return pods
}

func (pm *basicManager) GetMirrorPodByPod(pod *v1.Pod) (*v1.Pod, bool) {
	pm.lock.RLock()
	defer pm.lock.RUnlock()
	mirrorPod, ok := pm.mirrorPodByFullName[kubecontainer.GetPodFullName(pod)]
	return mirrorPod, ok
}

func (pm *basicManager) GetPodByMirrorPod(mirrorPod *v1.Pod) (*v1.Pod, bool) {
	pm.lock.RLock()
	defer pm.lock.RUnlock()
	pod, ok := pm.podByFullName[kubecontainer.GetPodFullName(mirrorPod)]
	return pod, ok
}
