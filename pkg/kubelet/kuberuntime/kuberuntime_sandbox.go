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

package kuberuntime

import (
	"fmt"
	"net"
	"net/url"
	"sort"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	kubetypes "k8s.io/apimachinery/pkg/types"
	runtimeapi "k8s.io/kubernetes/pkg/kubelet/apis/cri/v1alpha1/runtime"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
)

// createPodSandbox creates a pod sandbox and returns (podSandBoxID, message, error).
// createPodSandbox创建一个pod sandbox并且返回（podSandBoxID, message, error）
func (m *kubeGenericRuntimeManager) createPodSandbox(pod *v1.Pod, attempt uint32) (string, string, error) {
	// 创建sandbox配置
	podSandboxConfig, err := m.generatePodSandboxConfig(pod, attempt)
	if err != nil {
		message := fmt.Sprintf("GeneratePodSandboxConfig for pod %q failed: %v", format.Pod(pod), err)
		glog.Error(message)
		return "", message, err
	}

	// Create pod logs directory
	// 创建pod的log目录
	err = m.osInterface.MkdirAll(podSandboxConfig.LogDirectory, 0755)
	if err != nil {
		message := fmt.Sprintf("Create pod log directory for pod %q failed: %v", format.Pod(pod), err)
		glog.Errorf(message)
		return "", message, err
	}

	// 调用底层的容器运行时创建sandbox
	podSandBoxID, err := m.runtimeService.RunPodSandbox(podSandboxConfig)
	if err != nil {
		message := fmt.Sprintf("CreatePodSandbox for pod %q failed: %v", format.Pod(pod), err)
		glog.Error(message)
		return "", message, err
	}

	return podSandBoxID, "", nil
}

// generatePodSandboxConfig generates pod sandbox config from v1.Pod.
// generatePodSandboxConfig根据v1.Pod创建pod sandbox config
func (m *kubeGenericRuntimeManager) generatePodSandboxConfig(pod *v1.Pod, attempt uint32) (*runtimeapi.PodSandboxConfig, error) {
	// TODO: deprecating podsandbox resource requirements in favor of the pod level cgroup
	// Refer https://github.com/kubernetes/kubernetes/issues/29871
	podUID := string(pod.UID)
	podSandboxConfig := &runtimeapi.PodSandboxConfig{
		Metadata: &runtimeapi.PodSandboxMetadata{
			Name:      pod.Name,
			Namespace: pod.Namespace,
			Uid:       podUID,
			Attempt:   attempt,
		},
		Labels:      newPodLabels(pod),
		Annotations: newPodAnnotations(pod),
	}

	dnsConfig, err := m.runtimeHelper.GetPodDNS(pod)
	if err != nil {
		return nil, err
	}
	podSandboxConfig.DnsConfig = dnsConfig

	if !kubecontainer.IsHostNetworkPod(pod) {
		// 如果不是host network类型的pod，则添加设置hostname
		// TODO: Add domain support in new runtime interface
		hostname, _, err := m.runtimeHelper.GeneratePodHostNameAndDomain(pod)
		if err != nil {
			return nil, err
		}
		podSandboxConfig.Hostname = hostname
	}

	// 创建默认的logdir为"/var/log/pods/$PODID"
	logDir := buildPodLogsDirectory(pod.UID)
	podSandboxConfig.LogDirectory = logDir

	portMappings := []*runtimeapi.PortMapping{}
	for _, c := range pod.Spec.Containers {
		// 遍历容器的port mapping
		containerPortMappings := kubecontainer.MakePortMappings(&c)

		for idx := range containerPortMappings {
			port := containerPortMappings[idx]
			hostPort := int32(port.HostPort)
			containerPort := int32(port.ContainerPort)
			protocol := toRuntimeProtocol(port.Protocol)
			portMappings = append(portMappings, &runtimeapi.PortMapping{
				HostIp:        port.HostIP,
				HostPort:      hostPort,
				ContainerPort: containerPort,
				Protocol:      protocol,
			})
		}

	}
	if len(portMappings) > 0 {
		podSandboxConfig.PortMappings = portMappings
	}

	lc, err := m.generatePodSandboxLinuxConfig(pod)
	if err != nil {
		return nil, err
	}
	podSandboxConfig.Linux = lc

	return podSandboxConfig, nil
}

// generatePodSandboxLinuxConfig generates LinuxPodSandboxConfig from v1.Pod.
func (m *kubeGenericRuntimeManager) generatePodSandboxLinuxConfig(pod *v1.Pod) (*runtimeapi.LinuxPodSandboxConfig, error) {
	// 获取pod的cgroupParent
	cgroupParent := m.runtimeHelper.GetPodCgroupParent(pod)
	lc := &runtimeapi.LinuxPodSandboxConfig{
		CgroupParent: cgroupParent,
		SecurityContext: &runtimeapi.LinuxSandboxSecurityContext{
			Privileged:         kubecontainer.HasPrivilegedContainer(pod),
			SeccompProfilePath: m.getSeccompProfileFromAnnotations(pod.Annotations, ""),
		},
	}

	sysctls, err := getSysctlsFromAnnotations(pod.Annotations)
	if err != nil {
		return nil, fmt.Errorf("failed to get sysctls from annotations %v for pod %q: %v", pod.Annotations, format.Pod(pod), err)
	}
	lc.Sysctls = sysctls

	if pod.Spec.SecurityContext != nil {
		sc := pod.Spec.SecurityContext
		if sc.RunAsUser != nil {
			lc.SecurityContext.RunAsUser = &runtimeapi.Int64Value{Value: int64(*sc.RunAsUser)}
		}
		lc.SecurityContext.NamespaceOptions = &runtimeapi.NamespaceOption{
			HostNetwork: pod.Spec.HostNetwork,
			HostIpc:     pod.Spec.HostIPC,
			HostPid:     pod.Spec.HostPID,
		}

		if sc.FSGroup != nil {
			lc.SecurityContext.SupplementalGroups = append(lc.SecurityContext.SupplementalGroups, int64(*sc.FSGroup))
		}
		if groups := m.runtimeHelper.GetExtraSupplementalGroupsForPod(pod); len(groups) > 0 {
			lc.SecurityContext.SupplementalGroups = append(lc.SecurityContext.SupplementalGroups, groups...)
		}
		if sc.SupplementalGroups != nil {
			for _, sg := range sc.SupplementalGroups {
				lc.SecurityContext.SupplementalGroups = append(lc.SecurityContext.SupplementalGroups, int64(sg))
			}
		}
		if sc.SELinuxOptions != nil {
			lc.SecurityContext.SelinuxOptions = &runtimeapi.SELinuxOption{
				User:  sc.SELinuxOptions.User,
				Role:  sc.SELinuxOptions.Role,
				Type:  sc.SELinuxOptions.Type,
				Level: sc.SELinuxOptions.Level,
			}
		}
	}

	return lc, nil
}

// getKubeletSandboxes lists all (or just the running) sandboxes managed by kubelet.
func (m *kubeGenericRuntimeManager) getKubeletSandboxes(all bool) ([]*runtimeapi.PodSandbox, error) {
	var filter *runtimeapi.PodSandboxFilter
	if !all {
		readyState := runtimeapi.PodSandboxState_SANDBOX_READY
		filter = &runtimeapi.PodSandboxFilter{
			State: &runtimeapi.PodSandboxStateValue{
				State: readyState,
			},
		}
	}

	resp, err := m.runtimeService.ListPodSandbox(filter)
	if err != nil {
		glog.Errorf("ListPodSandbox failed: %v", err)
		return nil, err
	}

	return resp, nil
}

// determinePodSandboxIP determines the IP address of the given pod sandbox.
func (m *kubeGenericRuntimeManager) determinePodSandboxIP(podNamespace, podName string, podSandbox *runtimeapi.PodSandboxStatus) string {
	if podSandbox.Network == nil {
		glog.Warningf("Pod Sandbox status doesn't have network information, cannot report IP")
		return ""
	}
	ip := podSandbox.Network.Ip
	if len(ip) != 0 && net.ParseIP(ip) == nil {
		// ip could be an empty string if runtime is not responsible for the
		// IP (e.g., host networking).
		// ip可以是一个空字符串，如果运行时不负责IP
		glog.Warningf("Pod Sandbox reported an unparseable IP %v", ip)
		return ""
	}
	return ip
}

// getPodSandboxID gets the sandbox id by podUID and returns ([]sandboxID, error).
// Param state could be nil in order to get all sandboxes belonging to same pod.
func (m *kubeGenericRuntimeManager) getSandboxIDByPodUID(podUID kubetypes.UID, state *runtimeapi.PodSandboxState) ([]string, error) {
	filter := &runtimeapi.PodSandboxFilter{
		LabelSelector: map[string]string{types.KubernetesPodUIDLabel: string(podUID)},
	}
	if state != nil {
		filter.State = &runtimeapi.PodSandboxStateValue{
			State: *state,
		}
	}
	sandboxes, err := m.runtimeService.ListPodSandbox(filter)
	if err != nil {
		glog.Errorf("ListPodSandbox with pod UID %q failed: %v", podUID, err)
		return nil, err
	}

	if len(sandboxes) == 0 {
		return nil, nil
	}

	// Sort with newest first.
	sandboxIDs := make([]string, len(sandboxes))
	sort.Sort(podSandboxByCreated(sandboxes))
	for i, s := range sandboxes {
		sandboxIDs[i] = s.Id
	}

	return sandboxIDs, nil
}

// GetPortForward gets the endpoint the runtime will serve the port-forward request from.
func (m *kubeGenericRuntimeManager) GetPortForward(podName, podNamespace string, podUID kubetypes.UID, ports []int32) (*url.URL, error) {
	sandboxIDs, err := m.getSandboxIDByPodUID(podUID, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to find sandboxID for pod %s: %v", format.PodDesc(podName, podNamespace, podUID), err)
	}
	if len(sandboxIDs) == 0 {
		return nil, fmt.Errorf("failed to find sandboxID for pod %s", format.PodDesc(podName, podNamespace, podUID))
	}
	req := &runtimeapi.PortForwardRequest{
		PodSandboxId: sandboxIDs[0],
		Port:         ports,
	}
	resp, err := m.runtimeService.PortForward(req)
	if err != nil {
		return nil, err
	}
	return url.Parse(resp.Url)
}
