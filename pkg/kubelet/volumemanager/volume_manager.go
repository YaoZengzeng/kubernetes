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

package volumemanager

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/container"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/pod"
	"k8s.io/kubernetes/pkg/kubelet/status"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager/cache"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager/populator"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager/reconciler"
	"k8s.io/kubernetes/pkg/util/mount"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/operationexecutor"
	"k8s.io/kubernetes/pkg/volume/util/types"
	"k8s.io/kubernetes/pkg/volume/util/volumehelper"
)

const (
	// reconcilerLoopSleepPeriod is the amount of time the reconciler loop waits
	// between successive executions
	reconcilerLoopSleepPeriod time.Duration = 100 * time.Millisecond

	// reconcilerSyncStatesSleepPeriod is the amount of time the reconciler reconstruct process
	// waits between successive executions
	reconcilerSyncStatesSleepPeriod time.Duration = 3 * time.Minute

	// desiredStateOfWorldPopulatorLoopSleepPeriod is the amount of time the
	// DesiredStateOfWorldPopulator loop waits between successive executions
	desiredStateOfWorldPopulatorLoopSleepPeriod time.Duration = 100 * time.Millisecond

	// desiredStateOfWorldPopulatorGetPodStatusRetryDuration is the amount of
	// time the DesiredStateOfWorldPopulator loop waits between successive pod
	// cleanup calls (to prevent calling containerruntime.GetPodStatus too
	// frequently).
	desiredStateOfWorldPopulatorGetPodStatusRetryDuration time.Duration = 2 * time.Second

	// podAttachAndMountTimeout is the maximum amount of time the
	// WaitForAttachAndMount call will wait for all volumes in the specified pod
	// to be attached and mounted. Even though cloud operations can take several
	// minutes to complete, we set the timeout to 2 minutes because kubelet
	// will retry in the next sync iteration. This frees the associated
	// goroutine of the pod to process newer updates if needed (e.g., a delete
	// request to the pod).
	// Value is slightly offset from 2 minutes to make timeouts due to this
	// constant recognizable.
	podAttachAndMountTimeout time.Duration = 2*time.Minute + 3*time.Second

	// podAttachAndMountRetryInterval is the amount of time the GetVolumesForPod
	// call waits before retrying
	// GetVolumesForPod进行重试的时间间隔
	podAttachAndMountRetryInterval time.Duration = 300 * time.Millisecond

	// waitForAttachTimeout is the maximum amount of time a
	// operationexecutor.Mount call will wait for a volume to be attached.
	// Set to 10 minutes because we've seen attach operations take several
	// minutes to complete for some volume plugins in some cases. While this
	// operation is waiting it only blocks other operations on the same device,
	// other devices are not affected.
	waitForAttachTimeout time.Duration = 10 * time.Minute
)

// VolumeManager runs a set of asynchronous loops that figure out which volumes
// need to be attached/mounted/unmounted/detached based on the pods scheduled on
// this node and makes it so.
// VolumeManager运行一系列的asychronous loops用于搞清根据调度到本节点的pods
// 哪些volumes需要被attached/mounted/unmounted/detached
type VolumeManager interface {
	// Starts the volume manager and all the asynchronous loops that it controls
	// 启动该volume manager以及它控制的所有asychronous loop
	Run(sourcesReady config.SourcesReady, stopCh <-chan struct{})

	// WaitForAttachAndMount processes the volumes referenced in the specified
	// pod and blocks until they are all attached and mounted (reflected in
	// actual state of the world).
	// WaitForAttachAndMount会处理specified pod中引用的volumes并且会blocked，直到所有的
	// volumes都被attached以及mounted
	// An error is returned if all volumes are not attached and mounted within
	// the duration defined in podAttachAndMountTimeout.
	// 如果在给定的podAttachAndMountTimeout内有volumes没被attached或者mounted
	// 就会返回一个error
	WaitForAttachAndMount(pod *v1.Pod) error

	// GetMountedVolumesForPod returns a VolumeMap containing the volumes
	// referenced by the specified pod that are successfully attached and
	// mounted. The key in the map is the OuterVolumeSpecName (i.e.
	// pod.Spec.Volumes[x].Name). It returns an empty VolumeMap if pod has no
	// volumes.
	// GetMountedVolumesForPod返回一个VolumeMap，其中包含了specified pod成功attach
	// 并且mount的volumes
	GetMountedVolumesForPod(podName types.UniquePodName) container.VolumeMap

	// GetExtraSupplementalGroupsForPod returns a list of the extra
	// supplemental groups for the Pod. These extra supplemental groups come
	// from annotations on persistent volumes that the pod depends on.
	GetExtraSupplementalGroupsForPod(pod *v1.Pod) []int64

	// GetVolumesInUse returns a list of all volumes that implement the volume.Attacher
	// interface and are currently in use according to the actual and desired
	// state of the world caches. A volume is considered "in use" as soon as it
	// is added to the desired state of world, indicating it *should* be
	// attached to this node and remains "in use" until it is removed from both
	// the desired state of the world and the actual state of the world, or it
	// has been unmounted (as indicated in actual state of world).
	GetVolumesInUse() []v1.UniqueVolumeName

	// ReconcilerStatesHasBeenSynced returns true only after the actual states in reconciler
	// has been synced at least once after kubelet starts so that it is safe to update mounted
	// volume list retrieved from actual state.
	ReconcilerStatesHasBeenSynced() bool

	// VolumeIsAttached returns true if the given volume is attached to this
	// node.
	VolumeIsAttached(volumeName v1.UniqueVolumeName) bool

	// Marks the specified volume as having successfully been reported as "in
	// use" in the nodes's volume status.
	// 将指定的volume标记为"in use"，在节点的volume status
	MarkVolumesAsReportedInUse(volumesReportedAsInUse []v1.UniqueVolumeName)
}

// NewVolumeManager returns a new concrete instance implementing the
// VolumeManager interface.
// NewVolumeManager返回一个实现了VolumeManager接口的新的详细的实例
//
// kubeClient - kubeClient is the kube API client used by DesiredStateOfWorldPopulator
//   to communicate with the API server to fetch PV and PVC objects
// kubeClient - kubeClient是DesiredStateOfWorldPopulator使用的用于和apiserver交互
// 用于获取PV和PVC对象的kube API client
// volumePluginMgr - the volume plugin manager used to access volume plugins.
//   Must be pre-initialized.
// volumePluginMgr是用于访问volume plugins的volume plugin manager，它必须被先初始化
func NewVolumeManager(
	controllerAttachDetachEnabled bool,
	nodeName k8stypes.NodeName,
	podManager pod.Manager,
	podStatusProvider status.PodStatusProvider,
	kubeClient clientset.Interface,
	volumePluginMgr *volume.VolumePluginMgr,
	kubeContainerRuntime kubecontainer.Runtime,
	mounter mount.Interface,
	kubeletPodsDir string,
	recorder record.EventRecorder,
	checkNodeCapabilitiesBeforeMount bool,
	keepTerminatedPodVolumes bool) VolumeManager {

	vm := &volumeManager{
		kubeClient:          kubeClient,
		volumePluginMgr:     volumePluginMgr,
		desiredStateOfWorld: cache.NewDesiredStateOfWorld(volumePluginMgr),
		actualStateOfWorld:  cache.NewActualStateOfWorld(nodeName, volumePluginMgr),
		operationExecutor: operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
			kubeClient,
			volumePluginMgr,
			recorder,
			checkNodeCapabilitiesBeforeMount,
			util.NewBlockVolumePathHandler())),
	}

	vm.desiredStateOfWorldPopulator = populator.NewDesiredStateOfWorldPopulator(
		kubeClient,
		desiredStateOfWorldPopulatorLoopSleepPeriod,
		desiredStateOfWorldPopulatorGetPodStatusRetryDuration,
		podManager,
		podStatusProvider,
		vm.desiredStateOfWorld,
		kubeContainerRuntime,
		keepTerminatedPodVolumes)

	vm.reconciler = reconciler.NewReconciler(
		kubeClient,
		controllerAttachDetachEnabled,
		reconcilerLoopSleepPeriod,
		reconcilerSyncStatesSleepPeriod,
		waitForAttachTimeout,
		nodeName,
		vm.desiredStateOfWorld,
		vm.actualStateOfWorld,
		vm.desiredStateOfWorldPopulator.HasAddedPods,
		vm.operationExecutor,
		mounter,
		volumePluginMgr,
		kubeletPodsDir)

	return vm
}

// volumeManager implements the VolumeManager interface
type volumeManager struct {
	// kubeClient is the kube API client used by DesiredStateOfWorldPopulator to
	// communicate with the API server to fetch PV and PVC objects
	// kubeClient是由DesiredStateOfWorldPopulator用来和API server进行通信来获取
	// PV和PVC对象的kube API client
	kubeClient clientset.Interface

	// volumePluginMgr is the volume plugin manager used to access volume
	// plugins. It must be pre-initialized.
	// volumePluginMgr是用来访问volume plugins的volume plugin manager
	// 它必须先被初始化
	volumePluginMgr *volume.VolumePluginMgr

	// desiredStateOfWorld is a data structure containing the desired state of
	// the world according to the volume manager: i.e. what volumes should be
	// attached and which pods are referencing the volumes).
	// The data structure is populated by the desired state of the world
	// populator using the kubelet pod manager.
	// 根据volume manager期望达到的状态：哪些volume需要被attached以及哪些pods引用volumes
	desiredStateOfWorld cache.DesiredStateOfWorld

	// actualStateOfWorld is a data structure containing the actual state of
	// the world according to the manager: i.e. which volumes are attached to
	// this node and what pods the volumes are mounted to.
	// The data structure is populated upon successful completion of attach,
	// detach, mount, and unmount actions triggered by the reconciler.
	// 具体有哪些volume被attach到node以及这些volume都attach到哪些pods
	actualStateOfWorld cache.ActualStateOfWorld

	// operationExecutor is used to start asynchronous attach, detach, mount,
	// and unmount operations.
	// operationExecutor用于启动异步的attach, detach, mount以及unmount操作
	operationExecutor operationexecutor.OperationExecutor

	// reconciler runs an asynchronous periodic loop to reconcile the
	// desiredStateOfWorld with the actualStateOfWorld by triggering attach,
	// detach, mount, and unmount operations using the operationExecutor.
	// reconciler运行异步的periodic loop用来通过使用operationExecutor来触发attach
	// detach, mount以及unmount等操作来同步desiredStateOfWorld和actualStateOfWorld
	reconciler reconciler.Reconciler

	// desiredStateOfWorldPopulator runs an asynchronous periodic loop to
	// populate the desiredStateOfWorld using the kubelet PodManager.
	// desiredStateOfWorldPopulator通过使用kubelet PodManager来定期获取
	// desiredStateOfWrold
	desiredStateOfWorldPopulator populator.DesiredStateOfWorldPopulator
}

func (vm *volumeManager) Run(sourcesReady config.SourcesReady, stopCh <-chan struct{}) {
	defer runtime.HandleCrash()

	go vm.desiredStateOfWorldPopulator.Run(sourcesReady, stopCh)
	glog.V(2).Infof("The desired_state_of_world populator starts")

	glog.Infof("Starting Kubelet Volume Manager")
	go vm.reconciler.Run(stopCh)

	<-stopCh
	glog.Infof("Shutting down Kubelet Volume Manager")
}

func (vm *volumeManager) GetMountedVolumesForPod(podName types.UniquePodName) container.VolumeMap {
	podVolumes := make(container.VolumeMap)
	for _, mountedVolume := range vm.actualStateOfWorld.GetMountedVolumesForPod(podName) {
		podVolumes[mountedVolume.OuterVolumeSpecName] = container.VolumeInfo{
			Mounter:           mountedVolume.Mounter,
			BlockVolumeMapper: mountedVolume.BlockVolumeMapper,
			ReadOnly:          mountedVolume.VolumeSpec.ReadOnly,
		}
	}
	return podVolumes
}

func (vm *volumeManager) GetExtraSupplementalGroupsForPod(pod *v1.Pod) []int64 {
	podName := volumehelper.GetUniquePodName(pod)
	supplementalGroups := sets.NewString()

	for _, mountedVolume := range vm.actualStateOfWorld.GetMountedVolumesForPod(podName) {
		if mountedVolume.VolumeGidValue != "" {
			supplementalGroups.Insert(mountedVolume.VolumeGidValue)
		}
	}

	result := make([]int64, 0, supplementalGroups.Len())
	for _, group := range supplementalGroups.List() {
		iGroup, extra := getExtraSupplementalGid(group, pod)
		if !extra {
			continue
		}

		result = append(result, int64(iGroup))
	}

	return result
}

func (vm *volumeManager) GetVolumesInUse() []v1.UniqueVolumeName {
	// Report volumes in desired state of world and actual state of world so
	// that volumes are marked in use as soon as the decision is made that the
	// volume *should* be attached to this node until it is safely unmounted.
	desiredVolumes := vm.desiredStateOfWorld.GetVolumesToMount()
	mountedVolumes := vm.actualStateOfWorld.GetGloballyMountedVolumes()
	volumesToReportInUse := make([]v1.UniqueVolumeName, 0, len(desiredVolumes)+len(mountedVolumes))
	desiredVolumesMap := make(map[v1.UniqueVolumeName]bool, len(desiredVolumes)+len(mountedVolumes))

	for _, volume := range desiredVolumes {
		if volume.PluginIsAttachable {
			if _, exists := desiredVolumesMap[volume.VolumeName]; !exists {
				desiredVolumesMap[volume.VolumeName] = true
				volumesToReportInUse = append(volumesToReportInUse, volume.VolumeName)
			}
		}
	}

	for _, volume := range mountedVolumes {
		if volume.PluginIsAttachable {
			if _, exists := desiredVolumesMap[volume.VolumeName]; !exists {
				volumesToReportInUse = append(volumesToReportInUse, volume.VolumeName)
			}
		}
	}

	sort.Slice(volumesToReportInUse, func(i, j int) bool {
		return string(volumesToReportInUse[i]) < string(volumesToReportInUse[j])
	})
	return volumesToReportInUse
}

func (vm *volumeManager) ReconcilerStatesHasBeenSynced() bool {
	return vm.reconciler.StatesHasBeenSynced()
}

func (vm *volumeManager) VolumeIsAttached(
	volumeName v1.UniqueVolumeName) bool {
	return vm.actualStateOfWorld.VolumeExists(volumeName)
}

func (vm *volumeManager) MarkVolumesAsReportedInUse(
	volumesReportedAsInUse []v1.UniqueVolumeName) {
	vm.desiredStateOfWorld.MarkVolumesReportedInUse(volumesReportedAsInUse)
}

func (vm *volumeManager) WaitForAttachAndMount(pod *v1.Pod) error {
	// 从pod的配置中解析出volume的配置
	expectedVolumes := getExpectedVolumes(pod)
	if len(expectedVolumes) == 0 {
		// No volumes to verify
		return nil
	}

	glog.V(3).Infof("Waiting for volumes to attach and mount for pod %q", format.Pod(pod))
	// GetUniquePodName返回一个唯一的ID用来引用pod
	uniquePodName := volumehelper.GetUniquePodName(pod)

	// Some pods expect to have Setup called over and over again to update.
	// Remount plugins for which this is true. (Atomically updating volumes,
	// like Downward API, depend on this to update the contents of the volume).
	// 有的pods希望Setup能够一次一次地被调用用以更新
	// 像Downward API一样，依赖这个去更新volume的内容
	vm.desiredStateOfWorldPopulator.ReprocessPod(uniquePodName)
	vm.actualStateOfWorld.MarkRemountRequired(uniquePodName)

	err := wait.Poll(
		podAttachAndMountRetryInterval,
		podAttachAndMountTimeout,
		vm.verifyVolumesMountedFunc(uniquePodName, expectedVolumes))

	if err != nil {
		// Timeout expired
		unmountedVolumes :=
			vm.getUnmountedVolumes(uniquePodName, expectedVolumes)
		if len(unmountedVolumes) == 0 {
			return nil
		}

		return fmt.Errorf(
			"timeout expired waiting for volumes to attach/mount for pod %q/%q. list of unattached/unmounted volumes=%v",
			pod.Namespace,
			pod.Name,
			unmountedVolumes)
	}

	glog.V(3).Infof("All volumes are attached and mounted for pod %q", format.Pod(pod))
	return nil
}

// verifyVolumesMountedFunc returns a method that returns true when all expected
// volumes are mounted.
// verifyVolumesMountedFunc返回一个方法，如果所有expected volumes都被mount，就返回true
func (vm *volumeManager) verifyVolumesMountedFunc(podName types.UniquePodName, expectedVolumes []string) wait.ConditionFunc {
	return func() (done bool, err error) {
		// 调用volume manager, 如果unmount的volume数目为0，则说明所有volume都已经被mount
		return len(vm.getUnmountedVolumes(podName, expectedVolumes)) == 0, nil
	}
}

// getUnmountedVolumes fetches the current list of mounted volumes from
// the actual state of the world, and uses it to process the list of
// expectedVolumes. It returns a list of unmounted volumes.
// getUnmountedVolumes从actual state of the world中获取当前已经挂载的volumes
// 并且将它和expectedVolumes进行比较，返回一系列unmounted volumes
func (vm *volumeManager) getUnmountedVolumes(podName types.UniquePodName, expectedVolumes []string) []string {
	mountedVolumes := sets.NewString()
	for _, mountedVolume := range vm.actualStateOfWorld.GetMountedVolumesForPod(podName) {
		mountedVolumes.Insert(mountedVolume.OuterVolumeSpecName)
	}
	return filterUnmountedVolumes(mountedVolumes, expectedVolumes)
}

// filterUnmountedVolumes adds each element of expectedVolumes that is not in
// mountedVolumes to a list of unmountedVolumes and returns it.
func filterUnmountedVolumes(mountedVolumes sets.String, expectedVolumes []string) []string {
	unmountedVolumes := []string{}
	for _, expectedVolume := range expectedVolumes {
		if !mountedVolumes.Has(expectedVolume) {
			unmountedVolumes = append(unmountedVolumes, expectedVolume)
		}
	}
	return unmountedVolumes
}

// getExpectedVolumes returns a list of volumes that must be mounted in order to
// consider the volume setup step for this pod satisfied.
// getExpectedVolumes返回一系列的volumes它们作为pod的volume setup step必须满足
func getExpectedVolumes(pod *v1.Pod) []string {
	expectedVolumes := []string{}
	if pod == nil {
		return expectedVolumes
	}

	// 其实就是从pod.Spec.Volumes中获取
	for _, podVolume := range pod.Spec.Volumes {
		expectedVolumes = append(expectedVolumes, podVolume.Name)
	}

	return expectedVolumes
}

// getExtraSupplementalGid returns the value of an extra supplemental GID as
// defined by an annotation on a volume and a boolean indicating whether the
// volume defined a GID that the pod doesn't already request.
func getExtraSupplementalGid(volumeGidValue string, pod *v1.Pod) (int64, bool) {
	if volumeGidValue == "" {
		return 0, false
	}

	gid, err := strconv.ParseInt(volumeGidValue, 10, 64)
	if err != nil {
		return 0, false
	}

	if pod.Spec.SecurityContext != nil {
		for _, existingGid := range pod.Spec.SecurityContext.SupplementalGroups {
			if gid == int64(existingGid) {
				return 0, false
			}
		}
	}

	return gid, true
}
