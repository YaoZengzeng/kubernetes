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

package stats

import (
	"fmt"

	"github.com/golang/glog"

	statsapi "k8s.io/kubernetes/pkg/kubelet/apis/stats/v1alpha1"
)

type SummaryProvider interface {
	Get() (*statsapi.Summary, error)
}

// summaryProviderImpl implements the SummaryProvider interface.
type summaryProviderImpl struct {
	provider StatsProvider
}

var _ SummaryProvider = &summaryProviderImpl{}

// NewSummaryProvider returns a SummaryProvider using the stats provided by the
// specified statsProvider.
func NewSummaryProvider(statsProvider StatsProvider) SummaryProvider {
	return &summaryProviderImpl{statsProvider}
}

// Get provides a new Summary with the stats from Kubelet.
// Get用从Kubelet获取的stats提供一个新的Summary
func (sp *summaryProviderImpl) Get() (*statsapi.Summary, error) {
	// TODO(timstclair): Consider returning a best-effort response if any of
	// the following errors occur.
	node, err := sp.provider.GetNode()
	if err != nil {
		return nil, fmt.Errorf("failed to get node info: %v", err)
	}
	// 调用provider的各个方法获取node的stats信息
	nodeConfig := sp.provider.GetNodeConfig()
	rootStats, networkStats, err := sp.provider.GetCgroupStats("/")
	if err != nil {
		return nil, fmt.Errorf("failed to get root cgroup stats: %v", err)
	}
	rootFsStats, err := sp.provider.RootFsStats()
	if err != nil {
		return nil, fmt.Errorf("failed to get rootFs stats: %v", err)
	}
	imageFsStats, err := sp.provider.ImageFsStats()
	if err != nil {
		return nil, fmt.Errorf("failed to get imageFs stats: %v", err)
	}
	// 调用provider的ListPodStats获取pod stats
	podStats, err := sp.provider.ListPodStats()
	if err != nil {
		return nil, fmt.Errorf("failed to list pod stats: %v", err)
	}

	nodeStats := statsapi.NodeStats{
		NodeName:  node.Name,
		CPU:       rootStats.CPU,
		Memory:    rootStats.Memory,
		Network:   networkStats,
		StartTime: rootStats.StartTime,
		Fs:        rootFsStats,
		Runtime:   &statsapi.RuntimeStats{ImageFs: imageFsStats},
	}

	systemContainers := map[string]string{
		statsapi.SystemContainerKubelet: nodeConfig.KubeletCgroupsName,
		statsapi.SystemContainerRuntime: nodeConfig.RuntimeCgroupsName,
		statsapi.SystemContainerMisc:    nodeConfig.SystemCgroupsName,
	}
	for sys, name := range systemContainers {
		// skip if cgroup name is undefined (not all system containers are required)
		if name == "" {
			continue
		}
		// 通过provider的GetCgroupStats获取system container的stats
		s, _, err := sp.provider.GetCgroupStats(name)
		if err != nil {
			glog.Errorf("Failed to get system container stats for %q: %v", name, err)
			continue
		}
		// System containers don't have a filesystem associated with them.
		// System containers没有filesystem与之相关联
		s.Logs, s.Rootfs = nil, nil
		s.Name = sys
		nodeStats.SystemContainers = append(nodeStats.SystemContainers, *s)
	}

	summary := statsapi.Summary{
		Node: nodeStats,
		Pods: podStats,
	}
	return &summary, nil
}
