/*
Copyright 2017 The Kubernetes Authors.

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

package stats

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"
	cadvisorfs "github.com/google/cadvisor/fs"

	cadvisorapiv2 "github.com/google/cadvisor/info/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	internalapi "k8s.io/kubernetes/pkg/kubelet/apis/cri"
	runtimeapi "k8s.io/kubernetes/pkg/kubelet/apis/cri/v1alpha1/runtime"
	statsapi "k8s.io/kubernetes/pkg/kubelet/apis/stats/v1alpha1"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	"k8s.io/kubernetes/pkg/kubelet/server/stats"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
)

// criStatsProvider implements the containerStatsProvider interface by getting
// the container stats from CRI.
type criStatsProvider struct {
	// cadvisor is used to get the node root filesystem's stats (such as the
	// capacity/available bytes/inodes) that will be populated in per container
	// filesystem stats.
	cadvisor cadvisor.Interface
	// resourceAnalyzer is used to get the volume stats of the pods.
	resourceAnalyzer stats.ResourceAnalyzer
	// runtimeService is used to get the status and stats of the pods and its
	// managed containers.
	runtimeService internalapi.RuntimeService
	// imageService is used to get the stats of the image filesystem.
	imageService internalapi.ImageManagerService
}

// newCRIStatsProvider returns a containerStatsProvider implementation that
// provides container stats using CRI.
func newCRIStatsProvider(
	cadvisor cadvisor.Interface,
	resourceAnalyzer stats.ResourceAnalyzer,
	runtimeService internalapi.RuntimeService,
	imageService internalapi.ImageManagerService,
) containerStatsProvider {
	return &criStatsProvider{
		cadvisor:         cadvisor,
		resourceAnalyzer: resourceAnalyzer,
		runtimeService:   runtimeService,
		imageService:     imageService,
	}
}

// ListPodStats returns the stats of all the pod-managed containers.
// ListPodStats返回所有pod管理的容器的stats
func (p *criStatsProvider) ListPodStats() ([]statsapi.PodStats, error) {
	// Gets node root filesystem information, which will be used to populate
	// the available and capacity bytes/inodes in container stats.
	// 获取节点的rootfs信息，它可以用来填充container stats中的available以及capacity
	rootFsInfo, err := p.cadvisor.RootFsInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to get rootFs info: %v", err)
	}

	containers, err := p.runtimeService.ListContainers(&runtimeapi.ContainerFilter{})
	if err != nil {
		return nil, fmt.Errorf("failed to list all containers: %v", err)
	}

	// Creates pod sandbox map.
	podSandboxMap := make(map[string]*runtimeapi.PodSandbox)
	// 调用CRI的list sandbox获取信息
	podSandboxes, err := p.runtimeService.ListPodSandbox(&runtimeapi.PodSandboxFilter{})
	if err != nil {
		return nil, fmt.Errorf("failed to list all pod sandboxes: %v", err)
	}
	for _, s := range podSandboxes {
		// 保存list sandbox的结果
		podSandboxMap[s.Id] = s
	}

	// uuidToFsInfo is a map from filesystem UUID to its stats. This will be
	// used as a cache to avoid querying cAdvisor for the filesystem stats with
	// the same UUID many times.
	// uuidToFsInfo是文件系统的UUID到它的status的映射
	// 它会作为缓存从而避免用同样的UUID访问cAdvisor多次用于获取文件系统的stats
	uuidToFsInfo := make(map[runtimeapi.StorageIdentifier]*cadvisorapiv2.FsInfo)

	// sandboxIDToPodStats is a temporary map from sandbox ID to its pod stats.
	// sandboxIDToPodStats是sandbox ID到它的pod stats的临时映射
	sandboxIDToPodStats := make(map[string]*statsapi.PodStats)

	// 获取container stats
	resp, err := p.runtimeService.ListContainerStats(&runtimeapi.ContainerStatsFilter{})
	if err != nil {
		return nil, fmt.Errorf("failed to list all container stats: %v", err)
	}

	containers = removeTerminatedContainer(containers)
	// Creates container map.
	containerMap := make(map[string]*runtimeapi.Container)
	for _, c := range containers {
		// 保存list container的结果
		containerMap[c.Id] = c
	}

	caInfos, err := getCRICadvisorStats(p.cadvisor)
	if err != nil {
		return nil, fmt.Errorf("failed to get container info from cadvisor: %v", err)
	}

	// 遍历ListContainerStats返回的结果
	for _, stats := range resp {
		containerID := stats.Attributes.Id
		container, found := containerMap[containerID]
		if !found {
			glog.Errorf("Unknown id %q in container map.", containerID)
			continue
		}

		podSandboxID := container.PodSandboxId
		// 找到sandbox status
		podSandbox, found := podSandboxMap[podSandboxID]
		if !found {
			glog.Errorf("Unknown id %q in pod sandbox map.", podSandboxID)
			continue
		}

		// Creates the stats of the pod (if not created yet) which the
		// container belongs to.
		// 根据sandbox ID获取pod stats
		ps, found := sandboxIDToPodStats[podSandboxID]
		if !found {
			// 创建pod stats
			// buildPodStats仅仅是一些标识性的元数据的填充
			ps = buildPodStats(podSandbox)
			// Fill stats from cadvisor is available for full set of required pod stats
			// 用从cadvisor获取的数据填充caInfos
			caPodSandbox, found := caInfos[podSandboxID]
			if !found {
				glog.V(4).Info("Unable to find cadvisor stats for sandbox %q", podSandboxID)
			} else {
				// addCadvisorPodStats主要用于填充网络相关的数据
				p.addCadvisorPodStats(ps, &caPodSandbox)
			}
			sandboxIDToPodStats[podSandboxID] = ps
		}
		// 第一个参数stats是从CRI获取的结果
		cs := p.makeContainerStats(stats, container, &rootFsInfo, uuidToFsInfo)
		// If cadvisor stats is available for the container, use it to populate
		// container stats
		// 如果容器的cadvisor stats是可得的，用它来填充container stats
		caStats, caFound := caInfos[containerID]
		if !caFound {
			glog.V(4).Info("Unable to find cadvisor stats for %q", containerID)
		} else {
			// 添加容器的CPU和memory相关的信息
			p.addCadvisorContainerStats(cs, &caStats)
		}
		ps.Containers = append(ps.Containers, *cs)
	}

	result := make([]statsapi.PodStats, 0, len(sandboxIDToPodStats))
	for _, s := range sandboxIDToPodStats {
		// 为每个pod stats添加volume等相关信息
		p.makePodStorageStats(s, &rootFsInfo)
		result = append(result, *s)
	}
	return result, nil
}

// ImageFsStats returns the stats of the image filesystem.
func (p *criStatsProvider) ImageFsStats() (*statsapi.FsStats, error) {
	resp, err := p.imageService.ImageFsInfo()
	if err != nil {
		return nil, err
	}

	// CRI may return the stats of multiple image filesystems but we only
	// return the first one.
	// CRI可能会返回多个image filesystems，但是我们只要第一个
	//
	// TODO(yguo0905): Support returning stats of multiple image filesystems.
	for _, fs := range resp {
		s := &statsapi.FsStats{
			Time:       metav1.NewTime(time.Unix(0, fs.Timestamp)),
			UsedBytes:  &fs.UsedBytes.Value,
			InodesUsed: &fs.InodesUsed.Value,
		}
		imageFsInfo := p.getFsInfo(fs.StorageId)
		if imageFsInfo != nil {
			// The image filesystem UUID is unknown to the local node or
			// there's an error on retrieving the stats. In these cases, we
			// omit those stats and return the best-effort partial result. See
			// https://github.com/kubernetes/heapster/issues/1793.
			s.AvailableBytes = &imageFsInfo.Available
			s.CapacityBytes = &imageFsInfo.Capacity
			s.InodesFree = imageFsInfo.InodesFree
			s.Inodes = imageFsInfo.Inodes
		}
		return s, nil
	}

	return nil, fmt.Errorf("imageFs information is unavailable")
}

// getFsInfo returns the information of the filesystem with the specified
// storageID. If any error occurs, this function logs the error and returns
// nil.
// getFsInfo返回给定的storageID的文件系统的信息
// 如果报错，本函数会记录error并且返回nil
func (p *criStatsProvider) getFsInfo(storageID *runtimeapi.StorageIdentifier) *cadvisorapiv2.FsInfo {
	if storageID == nil {
		glog.V(2).Infof("Failed to get filesystem info: storageID is nil.")
		return nil
	}
	fsInfo, err := p.cadvisor.GetFsInfoByFsUUID(storageID.Uuid)
	if err != nil {
		msg := fmt.Sprintf("Failed to get the info of the filesystem with id %q: %v.", storageID.Uuid, err)
		if err == cadvisorfs.ErrNoSuchDevice {
			glog.V(2).Info(msg)
		} else {
			glog.Error(msg)
		}
		return nil
	}
	return &fsInfo
}

// buildPodStats returns a PodStats that identifies the Pod managing cinfo
func buildPodStats(podSandbox *runtimeapi.PodSandbox) *statsapi.PodStats {
	return &statsapi.PodStats{
		PodRef: statsapi.PodReference{
			Name:      podSandbox.Metadata.Name,
			UID:       podSandbox.Metadata.Uid,
			Namespace: podSandbox.Metadata.Namespace,
		},
		// The StartTime in the summary API is the pod creation time.
		// StartTime是pod创建的时间
		StartTime: metav1.NewTime(time.Unix(0, podSandbox.CreatedAt)),
	}
}

func (p *criStatsProvider) makePodStorageStats(s *statsapi.PodStats, rootFsInfo *cadvisorapiv2.FsInfo) *statsapi.PodStats {
	podUID := types.UID(s.PodRef.UID)
	if vstats, found := p.resourceAnalyzer.GetPodVolumeStats(podUID); found {
		ephemeralStats := make([]statsapi.VolumeStats, len(vstats.EphemeralVolumes))
		copy(ephemeralStats, vstats.EphemeralVolumes)
		s.VolumeStats = append(vstats.EphemeralVolumes, vstats.PersistentVolumes...)
		s.EphemeralStorage = calcEphemeralStorage(s.Containers, ephemeralStats, rootFsInfo)
	}
	return s
}

func (p *criStatsProvider) addCadvisorPodStats(
	ps *statsapi.PodStats,
	caPodSandbox *cadvisorapiv2.ContainerInfo,
) {
	ps.Network = cadvisorInfoToNetworkStats(ps.PodRef.Name, caPodSandbox)
}

func (p *criStatsProvider) makeContainerStats(
	stats *runtimeapi.ContainerStats,
	container *runtimeapi.Container,
	rootFsInfo *cadvisorapiv2.FsInfo,
	uuidToFsInfo map[runtimeapi.StorageIdentifier]*cadvisorapiv2.FsInfo,
) *statsapi.ContainerStats {
	result := &statsapi.ContainerStats{
		Name: stats.Attributes.Metadata.Name,
		// The StartTime in the summary API is the container creation time.
		StartTime: metav1.NewTime(time.Unix(0, container.CreatedAt)),
		// Work around heapster bug. https://github.com/kubernetes/kubernetes/issues/54962
		// TODO(random-liu): Remove this after heapster is updated to newer than 1.5.0-beta.0.
		CPU: &statsapi.CPUStats{
			UsageNanoCores: proto.Uint64(0),
		},
		Memory: &statsapi.MemoryStats{
			RSSBytes: proto.Uint64(0),
		},
		Rootfs: &statsapi.FsStats{},
		Logs: &statsapi.FsStats{
			Time:           metav1.NewTime(rootFsInfo.Timestamp),
			AvailableBytes: &rootFsInfo.Available,
			CapacityBytes:  &rootFsInfo.Capacity,
			InodesFree:     rootFsInfo.InodesFree,
			Inodes:         rootFsInfo.Inodes,
			// UsedBytes and InodesUsed are unavailable from CRI stats.
			// UsedBytes和InodesUsed从CRI stats中无法获取
			//
			// TODO(yguo0905): Get this information from kubelet and
			// populate the two fields here.
		},
		// UserDefinedMetrics is not supported by CRI.
	}
	// 如果从CRI获取的stats的CPU不为空
	if stats.Cpu != nil {
		result.CPU.Time = metav1.NewTime(time.Unix(0, stats.Cpu.Timestamp))
		if stats.Cpu.UsageCoreNanoSeconds != nil {
			// UsageCoreNanoSeconds是从CRI获取的
			result.CPU.UsageCoreNanoSeconds = &stats.Cpu.UsageCoreNanoSeconds.Value
		}
	}
	// 如果从CRI获取的stats的Memory不为空
	if stats.Memory != nil {
		result.Memory.Time = metav1.NewTime(time.Unix(0, stats.Memory.Timestamp))
		if stats.Memory.WorkingSetBytes != nil {
			// WorkingSetBytes也从CRI获取
			result.Memory.WorkingSetBytes = &stats.Memory.WorkingSetBytes.Value
		}
	}
	if stats.WritableLayer != nil {
		result.Rootfs.Time = metav1.NewTime(time.Unix(0, stats.WritableLayer.Timestamp))
		if stats.WritableLayer.UsedBytes != nil {
			result.Rootfs.UsedBytes = &stats.WritableLayer.UsedBytes.Value
		}
		if stats.WritableLayer.InodesUsed != nil {
			result.Rootfs.InodesUsed = &stats.WritableLayer.InodesUsed.Value
		}
	}
	storageID := stats.GetWritableLayer().GetStorageId()
	if storageID != nil {
		imageFsInfo, found := uuidToFsInfo[*storageID]
		if !found {
			imageFsInfo = p.getFsInfo(storageID)
			uuidToFsInfo[*storageID] = imageFsInfo
		}
		if imageFsInfo != nil {
			// The image filesystem UUID is unknown to the local node or there's an
			// error on retrieving the stats. In these cases, we omit those stats
			// and return the best-effort partial result. See
			// https://github.com/kubernetes/heapster/issues/1793.
			result.Rootfs.AvailableBytes = &imageFsInfo.Available
			result.Rootfs.CapacityBytes = &imageFsInfo.Capacity
			result.Rootfs.InodesFree = imageFsInfo.InodesFree
			result.Rootfs.Inodes = imageFsInfo.Inodes
		}
	}

	return result
}

// removeTerminatedContainer returns the specified container but with
// the stats of the terminated containers removed.
func removeTerminatedContainer(containers []*runtimeapi.Container) []*runtimeapi.Container {
	containerMap := make(map[containerID][]*runtimeapi.Container)
	// Sort order by create time
	sort.Slice(containers, func(i, j int) bool {
		return containers[i].CreatedAt < containers[j].CreatedAt
	})
	for _, container := range containers {
		refID := containerID{
			podRef:        buildPodRef(container.Labels),
			containerName: kubetypes.GetContainerName(container.Labels),
		}
		containerMap[refID] = append(containerMap[refID], container)
	}

	result := make([]*runtimeapi.Container, 0)
	for _, refs := range containerMap {
		if len(refs) == 1 {
			result = append(result, refs[0])
			continue
		}
		found := false
		for i := 0; i < len(refs); i++ {
			if refs[i].State == runtimeapi.ContainerState_CONTAINER_RUNNING {
				found = true
				result = append(result, refs[i])
			}
		}
		if !found {
			result = append(result, refs[len(refs)-1])
		}
	}
	return result
}

func (p *criStatsProvider) addCadvisorContainerStats(
	cs *statsapi.ContainerStats,
	caPodStats *cadvisorapiv2.ContainerInfo,
) {
	if caPodStats.Spec.HasCustomMetrics {
		cs.UserDefinedMetrics = cadvisorInfoToUserDefinedMetrics(caPodStats)
	}

	// 从cadvisor中获取CPU和Memory相关的信息
	cpu, memory := cadvisorInfoToCPUandMemoryStats(caPodStats)
	if cpu != nil {
		cs.CPU = cpu
	}
	if memory != nil {
		cs.Memory = memory
	}
}

func getCRICadvisorStats(ca cadvisor.Interface) (map[string]cadvisorapiv2.ContainerInfo, error) {
	stats := make(map[string]cadvisorapiv2.ContainerInfo)
	// 用cadvisor获取容器相关的信息
	infos, err := getCadvisorContainerInfo(ca)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch cadvisor stats: %v", err)
	}
	infos = removeTerminatedContainerInfo(infos)
	for key, info := range infos {
		// On systemd using devicemapper each mount into the container has an
		// associated cgroup. We ignore them to ensure we do not get duplicate
		// entries in our summary. For details on .mount units:
		// http://man7.org/linux/man-pages/man5/systemd.mount.5.html
		if strings.HasSuffix(key, ".mount") {
			continue
		}
		// Build the Pod key if this container is managed by a Pod
		if !isPodManagedContainer(&info) {
			continue
		}
		stats[path.Base(key)] = info
	}
	return stats, nil
}
