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

package kubelet

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"

	cadvisorapi "github.com/google/cadvisor/info/v1"
	cadvisorapiv2 "github.com/google/cadvisor/info/v2"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/clock"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientset "k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/certificate"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/client-go/util/integer"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/cloudprovider"
	"k8s.io/kubernetes/pkg/features"
	internalapi "k8s.io/kubernetes/pkg/kubelet/apis/cri"
	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/kubeletconfig"
	kubeletconfigv1alpha1 "k8s.io/kubernetes/pkg/kubelet/apis/kubeletconfig/v1alpha1"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	kubeletcertificate "k8s.io/kubernetes/pkg/kubelet/certificate"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/configmap"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/dockershim"
	dockerremote "k8s.io/kubernetes/pkg/kubelet/dockershim/remote"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	"k8s.io/kubernetes/pkg/kubelet/gpu"
	"k8s.io/kubernetes/pkg/kubelet/gpu/nvidia"
	"k8s.io/kubernetes/pkg/kubelet/images"
	"k8s.io/kubernetes/pkg/kubelet/kubeletconfig"
	"k8s.io/kubernetes/pkg/kubelet/kuberuntime"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/kubernetes/pkg/kubelet/network"
	"k8s.io/kubernetes/pkg/kubelet/network/dns"
	"k8s.io/kubernetes/pkg/kubelet/pleg"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	"k8s.io/kubernetes/pkg/kubelet/preemption"
	"k8s.io/kubernetes/pkg/kubelet/prober"
	proberesults "k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/remote"
	"k8s.io/kubernetes/pkg/kubelet/rkt"
	"k8s.io/kubernetes/pkg/kubelet/secret"
	"k8s.io/kubernetes/pkg/kubelet/server"
	serverstats "k8s.io/kubernetes/pkg/kubelet/server/stats"
	"k8s.io/kubernetes/pkg/kubelet/server/streaming"
	"k8s.io/kubernetes/pkg/kubelet/stats"
	"k8s.io/kubernetes/pkg/kubelet/status"
	"k8s.io/kubernetes/pkg/kubelet/sysctl"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/kubernetes/pkg/kubelet/util/queue"
	"k8s.io/kubernetes/pkg/kubelet/util/sliceutils"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager"
	"k8s.io/kubernetes/pkg/security/apparmor"
	utildbus "k8s.io/kubernetes/pkg/util/dbus"
	kubeio "k8s.io/kubernetes/pkg/util/io"
	utilipt "k8s.io/kubernetes/pkg/util/iptables"
	"k8s.io/kubernetes/pkg/util/mount"
	nodeutil "k8s.io/kubernetes/pkg/util/node"
	"k8s.io/kubernetes/pkg/util/oom"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/plugin/pkg/scheduler/algorithm/predicates"
	utilexec "k8s.io/utils/exec"
)

const (
	// Max amount of time to wait for the container runtime to come up.
	maxWaitForContainerRuntime = 30 * time.Second

	// nodeStatusUpdateRetry specifies how many times kubelet retries when posting node status failed.
	nodeStatusUpdateRetry = 5

	// ContainerLogsDir is the location of container logs.
	ContainerLogsDir = "/var/log/containers"

	// MaxContainerBackOff is the max backoff period, exported for the e2e test
	// MaxContainerBackOff是最大的回退间隔
	MaxContainerBackOff = 300 * time.Second

	// Capacity of the channel for storing pods to kill. A small number should
	// suffice because a goroutine is dedicated to check the channel and does
	// not block on anything else.
	podKillingChannelCapacity = 50

	// Period for performing global cleanup tasks.
	// 用于global cleanup tasks的时间间隔
	housekeepingPeriod = time.Second * 2

	// Period for performing eviction monitoring.
	// TODO ensure this is in sync with internal cadvisor housekeeping.
	evictionMonitoringPeriod = time.Second * 10

	// The path in containers' filesystems where the hosts file is mounted.
	etcHostsPath = "/etc/hosts"

	// Capacity of the channel for receiving pod lifecycle events. This number
	// is a bit arbitrary and may be adjusted in the future.
	// 用于接收pod lifecycle events的channel的capacity
	// 这个数字的设置有些随意并且可能在以后修改
	plegChannelCapacity = 1000

	// Generic PLEG relies on relisting for discovering container events.
	// A longer period means that kubelet will take longer to detect container
	// changes and to update pod status. On the other hand, a shorter period
	// will cause more frequent relisting (e.g., container runtime operations),
	// leading to higher cpu usage.
	// 一般的PLEG依赖relisting来发现container events
	// 一个长的检测时间间隔意味着kubelet会用更长的时间检测container changes以及更新pod status
	// 另外，一个更短的检测时间间隔会导致更频繁的relisting（容器运行时的操作），会导致更高的cpu使用率
	// Note that even though we set the period to 1s, the relisting itself can
	// take more than 1s to finish if the container runtime responds slowly
	// and/or when there are many container changes in one cycle.
	// 虽然我们将时间间隔设置为1秒，但是relisting本身的用时可能会多于一秒
	// 因为容器运行时的反应可能比较慢，或者在一个周期内有太多的changes
	plegRelistPeriod = time.Second * 1

	// backOffPeriod is the period to back off when pod syncing results in an
	// error. It is also used as the base period for the exponential backoff
	// container restarts and image pulls.
	// backOffPeriod是回退的时间间隔，当pod的syncing results处于error状态的话
	// 同时这也能作为容器重启以及拉取镜像的指数回退的base period
	backOffPeriod = time.Second * 10

	// ContainerGCPeriod is the period for performing container garbage collection.
	ContainerGCPeriod = time.Minute
	// ImageGCPeriod is the period for performing image garbage collection.
	ImageGCPeriod = 5 * time.Minute

	// Minimum number of dead containers to keep in a pod
	minDeadContainerInPod = 1
)

// SyncHandler is an interface implemented by Kubelet, for testability
// Kubelet实现了SyncHandler这个接口
type SyncHandler interface {
	HandlePodAdditions(pods []*v1.Pod)
	HandlePodUpdates(pods []*v1.Pod)
	HandlePodRemoves(pods []*v1.Pod)
	HandlePodReconcile(pods []*v1.Pod)
	HandlePodSyncs(pods []*v1.Pod)
	HandlePodCleanups() error
}

// Option is a functional option type for Kubelet
type Option func(*Kubelet)

// Bootstrap is a bootstrapping interface for kubelet, targets the initialization protocol
// Bootstrap是一个kubelet的bootstrapping interface，用于初始化
type Bootstrap interface {
	GetConfiguration() kubeletconfiginternal.KubeletConfiguration
	BirthCry()
	StartGarbageCollection()
	ListenAndServe(address net.IP, port uint, tlsOptions *server.TLSOptions, auth server.AuthInterface, enableDebuggingHandlers, enableContentionProfiling bool)
	ListenAndServeReadOnly(address net.IP, port uint)
	Run(<-chan kubetypes.PodUpdate)
	RunOnce(<-chan kubetypes.PodUpdate) ([]RunPodResult, error)
}

// Builder creates and initializes a Kubelet instance
// Builder创建并且初始化一个Kubelet实例，其实是一个Bootstrap接口
type Builder func(kubeCfg *kubeletconfiginternal.KubeletConfiguration,
	kubeDeps *Dependencies,
	crOptions *config.ContainerRuntimeOptions,
	containerRuntime string,
	runtimeCgroups string,
	hostnameOverride string,
	nodeIP string,
	providerID string,
	cloudProvider string,
	certDirectory string,
	rootDirectory string,
	registerNode bool,
	registerWithTaints []api.Taint,
	allowedUnsafeSysctls []string,
	containerized bool,
	remoteRuntimeEndpoint string,
	remoteImageEndpoint string,
	experimentalMounterPath string,
	experimentalKernelMemcgNotification bool,
	experimentalCheckNodeCapabilitiesBeforeMount bool,
	experimentalNodeAllocatableIgnoreEvictionThreshold bool,
	minimumGCAge metav1.Duration,
	maxPerPodContainerCount int32,
	maxContainerCount int32,
	masterServiceNamespace string,
	registerSchedulable bool,
	nonMasqueradeCIDR string,
	keepTerminatedPodVolumes bool,
	nodeLabels map[string]string,
	seccompProfileRoot string,
	bootstrapCheckpointPath string) (Bootstrap, error)

// Dependencies is a bin for things we might consider "injected dependencies" -- objects constructed
// at runtime that are necessary for running the Kubelet. This is a temporary solution for grouping
// these objects while we figure out a more comprehensive dependency injection story for the Kubelet.
// Dependencies是一种“依赖注入”，包含了那些在运行时期间构造，用于运行kubelet的对象
// 以后可能会有更好的方案
type Dependencies struct {
	// TODO(mtaufen): KubeletBuilder:
	//                Mesos currently uses this as a hook to let them make their own call to
	//                let them wrap the KubeletBootstrap that CreateAndInitKubelet returns with
	//                their own KubeletBootstrap. It's a useful hook. I need to think about what
	//                a nice home for it would be. There seems to be a trend, between this and
	//                the Options fields below, of providing hooks where you can add extra functionality
	//                to the Kubelet for your solution. Maybe we should centralize these sorts of things?
	Builder Builder

	// TODO(mtaufen): ContainerRuntimeOptions and Options:
	//                Arrays of functions that can do arbitrary things to the Kubelet and the Runtime
	//                seem like a difficult path to trace when it's time to debug something.
	//                I'm leaving these fields here for now, but there is likely an easier-to-follow
	//                way to support their intended use cases. E.g. ContainerRuntimeOptions
	//                is used by Mesos to set an environment variable in containers which has
	//                some connection to their container GC. It seems that Mesos intends to use
	//                Options to add additional node conditions that are updated as part of the
	//                Kubelet lifecycle (see https://github.com/kubernetes/kubernetes/pull/21521).
	//                We should think about providing more explicit ways of doing these things.
	// ContainerRuntimeOptions是一个函数数组，我们可以用它对kubelet做任何事
	ContainerRuntimeOptions []kubecontainer.Option
	Options                 []Option

	// Injected Dependencies
	Auth                    server.AuthInterface
	CAdvisorInterface       cadvisor.Interface
	Cloud                   cloudprovider.Interface
	ContainerManager        cm.ContainerManager
	DockerClientConfig      *dockershim.ClientConfig
	EventClient             v1core.EventsGetter
	HeartbeatClient         v1core.CoreV1Interface
	KubeClient              clientset.Interface
	ExternalKubeClient      clientset.Interface
	Mounter                 mount.Interface
	NetworkPlugins          []network.NetworkPlugin
	OOMAdjuster             *oom.OOMAdjuster
	OSInterface             kubecontainer.OSInterface
	PodConfig               *config.PodConfig
	Recorder                record.EventRecorder
	Writer                  kubeio.Writer
	VolumePlugins           []volume.VolumePlugin
	DynamicPluginProber     volume.DynamicPluginProber
	TLSOptions              *server.TLSOptions
	KubeletConfigController *kubeletconfig.Controller
}

// makePodSourceConfig creates a config.PodConfig from the given
// KubeletConfiguration or returns an error.
// makePodSourceConfig根据给定的KubeletConfiguration创建一个config.PodConfig
// 或者返回error
func makePodSourceConfig(kubeCfg *kubeletconfiginternal.KubeletConfiguration, kubeDeps *Dependencies, nodeName types.NodeName, bootstrapCheckpointPath string) (*config.PodConfig, error) {
	manifestURLHeader := make(http.Header)
	if len(kubeCfg.ManifestURLHeader) > 0 {
		for k, v := range kubeCfg.ManifestURLHeader {
			for i := range v {
				manifestURLHeader.Add(k, v[i])
			}
		}
	}

	// source of all configuration
	cfg := config.NewPodConfig(config.PodConfigNotificationIncremental, kubeDeps.Recorder)

	// define file config source
	// 从文件中获取pod配置
	if kubeCfg.PodManifestPath != "" {
		glog.Infof("Adding manifest path: %v", kubeCfg.PodManifestPath)
		config.NewSourceFile(kubeCfg.PodManifestPath, nodeName, kubeCfg.FileCheckFrequency.Duration, cfg.Channel(kubetypes.FileSource))
	}

	// define url config source
	// 从url中获取pod配置
	if kubeCfg.ManifestURL != "" {
		glog.Infof("Adding manifest url %q with HTTP header %v", kubeCfg.ManifestURL, manifestURLHeader)
		config.NewSourceURL(kubeCfg.ManifestURL, manifestURLHeader, nodeName, kubeCfg.HTTPCheckFrequency.Duration, cfg.Channel(kubetypes.HTTPSource))
	}

	// Restore from the checkpoint path
	// NOTE: This MUST happen before creating the apiserver source
	// below, or the checkpoint would override the source of truth.
	// 从checkpoint path中恢复，这必须在创建apiserver source之前
	// 否则checkpoint会掩盖真实的pod配置

	var updatechannel chan<- interface{}
	// 如果bootstrapCheckpointPath不为空
	if bootstrapCheckpointPath != "" {
		glog.Infof("Adding checkpoint path: %v", bootstrapCheckpointPath)
		updatechannel = cfg.Channel(kubetypes.ApiserverSource)
		// 从checkpoint中返回pod配置
		err := cfg.Restore(bootstrapCheckpointPath, updatechannel)
		if err != nil {
			return nil, err
		}
	}

	// 监听apiserver
	if kubeDeps.KubeClient != nil {
		glog.Infof("Watching apiserver")
		if updatechannel == nil {
			// 创建source为apiserver的channel
			updatechannel = cfg.Channel(kubetypes.ApiserverSource)
		}
		config.NewSourceApiserver(kubeDeps.KubeClient, nodeName, updatechannel)
	}
	return cfg, nil
}

func getRuntimeAndImageServices(remoteRuntimeEndpoint string, remoteImageEndpoint string, runtimeRequestTimeout metav1.Duration) (internalapi.RuntimeService, internalapi.ImageManagerService, error) {
	// 创建远程的runtime service
	rs, err := remote.NewRemoteRuntimeService(remoteRuntimeEndpoint, runtimeRequestTimeout.Duration)
	if err != nil {
		return nil, nil, err
	}
	// 创建远程的image service
	is, err := remote.NewRemoteImageService(remoteImageEndpoint, runtimeRequestTimeout.Duration)
	if err != nil {
		return nil, nil, err
	}
	return rs, is, err
}

// NewMainKubelet instantiates a new Kubelet object along with all the required internal modules.
// No initialization of Kubelet and its modules should happen here.
// NewMainKubelet实例化一个新的Kubelet对象以及所有它需要的内部对象
func NewMainKubelet(kubeCfg *kubeletconfiginternal.KubeletConfiguration,
	kubeDeps *Dependencies,
	crOptions *config.ContainerRuntimeOptions,
	containerRuntime string,
	runtimeCgroups string,
	hostnameOverride string,
	nodeIP string,
	providerID string,
	cloudProvider string,
	certDirectory string,
	rootDirectory string,
	registerNode bool,
	registerWithTaints []api.Taint,
	allowedUnsafeSysctls []string,
	containerized bool,
	remoteRuntimeEndpoint string,
	remoteImageEndpoint string,
	experimentalMounterPath string,
	experimentalKernelMemcgNotification bool,
	experimentalCheckNodeCapabilitiesBeforeMount bool,
	experimentalNodeAllocatableIgnoreEvictionThreshold bool,
	minimumGCAge metav1.Duration,
	maxPerPodContainerCount int32,
	maxContainerCount int32,
	masterServiceNamespace string,
	registerSchedulable bool,
	nonMasqueradeCIDR string,
	keepTerminatedPodVolumes bool,
	nodeLabels map[string]string,
	seccompProfileRoot string,
	bootstrapCheckpointPath string) (*Kubelet, error) {
	if rootDirectory == "" {
		return nil, fmt.Errorf("invalid root directory %q", rootDirectory)
	}
	if kubeCfg.SyncFrequency.Duration <= 0 {
		return nil, fmt.Errorf("invalid sync frequency %d", kubeCfg.SyncFrequency.Duration)
	}

	if kubeCfg.MakeIPTablesUtilChains {
		if kubeCfg.IPTablesMasqueradeBit > 31 || kubeCfg.IPTablesMasqueradeBit < 0 {
			return nil, fmt.Errorf("iptables-masquerade-bit is not valid. Must be within [0, 31]")
		}
		if kubeCfg.IPTablesDropBit > 31 || kubeCfg.IPTablesDropBit < 0 {
			return nil, fmt.Errorf("iptables-drop-bit is not valid. Must be within [0, 31]")
		}
		if kubeCfg.IPTablesDropBit == kubeCfg.IPTablesMasqueradeBit {
			return nil, fmt.Errorf("iptables-masquerade-bit and iptables-drop-bit must be different")
		}
	}

	hostname := nodeutil.GetHostname(hostnameOverride)
	// Query the cloud provider for our node name, default to hostname
	nodeName := types.NodeName(hostname)
	cloudIPs := []net.IP{}
	cloudNames := []string{}
	// 如果cloud provider不为空
	if kubeDeps.Cloud != nil {
		var err error
		instances, ok := kubeDeps.Cloud.Instances()
		if !ok {
			return nil, fmt.Errorf("failed to get instances from cloud provider")
		}

		nodeName, err = instances.CurrentNodeName(hostname)
		if err != nil {
			return nil, fmt.Errorf("error fetching current instance name from cloud provider: %v", err)
		}

		glog.V(2).Infof("cloud provider determined current node name to be %s", nodeName)

		if utilfeature.DefaultFeatureGate.Enabled(features.RotateKubeletServerCertificate) {
			nodeAddresses, err := instances.NodeAddresses(nodeName)
			if err != nil {
				return nil, fmt.Errorf("failed to get the addresses of the current instance from the cloud provider: %v", err)
			}
			for _, nodeAddress := range nodeAddresses {
				switch nodeAddress.Type {
				case v1.NodeExternalIP, v1.NodeInternalIP:
					ip := net.ParseIP(nodeAddress.Address)
					if ip != nil && !ip.IsLoopback() {
						cloudIPs = append(cloudIPs, ip)
					}
				case v1.NodeExternalDNS, v1.NodeInternalDNS, v1.NodeHostName:
					cloudNames = append(cloudNames, nodeAddress.Address)
				}
			}
		}

	}

	// 如果PodConfig为空，则对其进行设置
	// 所有的pod配置都从PodConfig中来
	if kubeDeps.PodConfig == nil {
		var err error
		kubeDeps.PodConfig, err = makePodSourceConfig(kubeCfg, kubeDeps, nodeName, bootstrapCheckpointPath)
		if err != nil {
			return nil, err
		}
	}

	containerGCPolicy := kubecontainer.ContainerGCPolicy{
		MinAge:             minimumGCAge.Duration,
		MaxPerPodContainer: int(maxPerPodContainerCount),
		MaxContainers:      int(maxContainerCount),
	}

	daemonEndpoints := &v1.NodeDaemonEndpoints{
		KubeletEndpoint: v1.DaemonEndpoint{Port: kubeCfg.Port},
	}

	imageGCPolicy := images.ImageGCPolicy{
		MinAge:               kubeCfg.ImageMinimumGCAge.Duration,
		HighThresholdPercent: int(kubeCfg.ImageGCHighThresholdPercent),
		LowThresholdPercent:  int(kubeCfg.ImageGCLowThresholdPercent),
	}

	enforceNodeAllocatable := kubeCfg.EnforceNodeAllocatable
	if experimentalNodeAllocatableIgnoreEvictionThreshold {
		// Do not provide kubeCfg.EnforceNodeAllocatable to eviction threshold parsing if we are not enforcing Evictions
		enforceNodeAllocatable = []string{}
	}
	thresholds, err := eviction.ParseThresholdConfig(enforceNodeAllocatable, kubeCfg.EvictionHard, kubeCfg.EvictionSoft, kubeCfg.EvictionSoftGracePeriod, kubeCfg.EvictionMinimumReclaim)
	if err != nil {
		return nil, err
	}
	evictionConfig := eviction.Config{
		PressureTransitionPeriod: kubeCfg.EvictionPressureTransitionPeriod.Duration,
		MaxPodGracePeriodSeconds: int64(kubeCfg.EvictionMaxPodGracePeriod),
		Thresholds:               thresholds,
		KernelMemcgNotification:  experimentalKernelMemcgNotification,
	}

	// 使用reflector把ListWatch得到的服务信息实时同步到serviceIndexer中
	serviceIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	if kubeDeps.KubeClient != nil {
		serviceLW := cache.NewListWatchFromClient(kubeDeps.KubeClient.CoreV1().RESTClient(), "services", metav1.NamespaceAll, fields.Everything())
		r := cache.NewReflector(serviceLW, &v1.Service{}, serviceIndexer, 0)
		go r.Run(wait.NeverStop)
	}
	serviceLister := corelisters.NewServiceLister(serviceIndexer)

	// 使用reflector把ListWatch得到的节点信息实时同步到nodeIndexer中
	nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	if kubeDeps.KubeClient != nil {
		fieldSelector := fields.Set{api.ObjectNameField: string(nodeName)}.AsSelector()
		nodeLW := cache.NewListWatchFromClient(kubeDeps.KubeClient.CoreV1().RESTClient(), "nodes", metav1.NamespaceAll, fieldSelector)
		r := cache.NewReflector(nodeLW, &v1.Node{}, nodeIndexer, 0)
		go r.Run(wait.NeverStop)
	}
	nodeInfo := &predicates.CachedNodeInfo{NodeLister: corelisters.NewNodeLister(nodeIndexer)}

	// TODO: get the real node object of ourself,
	// and use the real node name and UID.
	// TODO: what is namespace for node?
	nodeRef := &v1.ObjectReference{
		Kind:      "Node",
		Name:      string(nodeName),
		UID:       types.UID(nodeName),
		Namespace: "",
	}

	containerRefManager := kubecontainer.NewRefManager()

	oomWatcher := NewOOMWatcher(kubeDeps.CAdvisorInterface, kubeDeps.Recorder)

	clusterDNS := make([]net.IP, 0, len(kubeCfg.ClusterDNS))
	for _, ipEntry := range kubeCfg.ClusterDNS {
		ip := net.ParseIP(ipEntry)
		if ip == nil {
			glog.Warningf("Invalid clusterDNS ip '%q'", ipEntry)
		} else {
			clusterDNS = append(clusterDNS, ip)
		}
	}
	httpClient := &http.Client{}
	parsedNodeIP := net.ParseIP(nodeIP)

	// PodConfig并没有直接包含在Kubelet对象中
	klet := &Kubelet{
		hostname:                       hostname,
		nodeName:                       nodeName,
		kubeClient:                     kubeDeps.KubeClient,
		heartbeatClient:                kubeDeps.HeartbeatClient,
		rootDirectory:                  rootDirectory,
		resyncInterval:                 kubeCfg.SyncFrequency.Duration,
		sourcesReady:                   config.NewSourcesReady(kubeDeps.PodConfig.SeenAllSources),
		registerNode:                   registerNode,
		registerWithTaints:             registerWithTaints,
		registerSchedulable:            registerSchedulable,
		// 创建dnsConfigurer
		dnsConfigurer:                  dns.NewConfigurer(kubeDeps.Recorder, nodeRef, parsedNodeIP, clusterDNS, kubeCfg.ClusterDomain, kubeCfg.ResolverConfig),
		serviceLister:                  serviceLister,
		nodeInfo:                       nodeInfo,
		masterServiceNamespace:         masterServiceNamespace,
		streamingConnectionIdleTimeout: kubeCfg.StreamingConnectionIdleTimeout.Duration,
		recorder:                       kubeDeps.Recorder,
		cadvisor:                       kubeDeps.CAdvisorInterface,
		cloud:                          kubeDeps.Cloud,
		autoDetectCloudProvider:   (kubeletconfigv1alpha1.AutoDetectCloudProvider == cloudProvider),
		externalCloudProvider:     cloudprovider.IsExternal(cloudProvider),
		providerID:                providerID,
		nodeRef:                   nodeRef,
		nodeLabels:                nodeLabels,
		nodeStatusUpdateFrequency: kubeCfg.NodeStatusUpdateFrequency.Duration,
		os:                   kubeDeps.OSInterface,
		oomWatcher:           oomWatcher,
		cgroupsPerQOS:        kubeCfg.CgroupsPerQOS,
		cgroupRoot:           kubeCfg.CgroupRoot,
		mounter:              kubeDeps.Mounter,
		writer:               kubeDeps.Writer,
		maxPods:              int(kubeCfg.MaxPods),
		podsPerCore:          int(kubeCfg.PodsPerCore),
		syncLoopMonitor:      atomic.Value{},
		daemonEndpoints:      daemonEndpoints,
		containerManager:     kubeDeps.ContainerManager,
		containerRuntimeName: containerRuntime,
		nodeIP:               parsedNodeIP,
		clock:                clock.RealClock{},
		enableControllerAttachDetach:            kubeCfg.EnableControllerAttachDetach,
		iptClient:                               utilipt.New(utilexec.New(), utildbus.New(), utilipt.ProtocolIpv4),
		makeIPTablesUtilChains:                  kubeCfg.MakeIPTablesUtilChains,
		iptablesMasqueradeBit:                   int(kubeCfg.IPTablesMasqueradeBit),
		iptablesDropBit:                         int(kubeCfg.IPTablesDropBit),
		experimentalHostUserNamespaceDefaulting: utilfeature.DefaultFeatureGate.Enabled(features.ExperimentalHostUserNamespaceDefaultingGate),
		keepTerminatedPodVolumes:                keepTerminatedPodVolumes,
	}

	// 创建secret manager
	secretManager := secret.NewCachingSecretManager(
		kubeDeps.KubeClient, secret.GetObjectTTLFromNodeFunc(klet.GetNode))
	klet.secretManager = secretManager

	// 创建configmap manager
	configMapManager := configmap.NewCachingConfigMapManager(
		kubeDeps.KubeClient, configmap.GetObjectTTLFromNodeFunc(klet.GetNode))
	klet.configMapManager = configMapManager

	if klet.experimentalHostUserNamespaceDefaulting {
		glog.Infof("Experimental host user namespace defaulting is enabled.")
	}

	hairpinMode, err := effectiveHairpinMode(kubeletconfiginternal.HairpinMode(kubeCfg.HairpinMode), containerRuntime, crOptions.NetworkPluginName)
	if err != nil {
		// This is a non-recoverable error. Returning it up the callstack will just
		// lead to retries of the same failure, so just fail hard.
		glog.Fatalf("Invalid hairpin mode: %v", err)
	}
	glog.Infof("Hairpin mode set to %q", hairpinMode)

	plug, err := network.InitNetworkPlugin(kubeDeps.NetworkPlugins, crOptions.NetworkPluginName, &criNetworkHost{&networkHost{klet}, &network.NoopPortMappingGetter{}}, hairpinMode, nonMasqueradeCIDR, int(crOptions.NetworkPluginMTU))
	if err != nil {
		return nil, err
	}
	klet.networkPlugin = plug

	machineInfo, err := klet.GetCachedMachineInfo()
	if err != nil {
		return nil, err
	}

	imageBackOff := flowcontrol.NewBackOff(backOffPeriod, MaxContainerBackOff)

	klet.livenessManager = proberesults.NewManager()

	klet.podCache = kubecontainer.NewCache()
	// podManager is also responsible for keeping secretManager and configMapManager contents up-to-date.
	// podManager负责保持secretManager和configMapManager的内容是最新的
	klet.podManager = kubepod.NewBasicPodManager(kubepod.NewBasicMirrorClient(klet.kubeClient), secretManager, configMapManager)

	// 如果没有指定remoteImageEndpoint，则将其设置为和remoteRuntimeEndpoint相同
	if remoteRuntimeEndpoint != "" {
		// remoteImageEndpoint is same as remoteRuntimeEndpoint if not explicitly specified
		if remoteImageEndpoint == "" {
			remoteImageEndpoint = remoteRuntimeEndpoint
		}
	}

	// TODO: These need to become arguments to a standalone docker shim.
	// 这些应该作为传递给standalone docker shim的参数
	pluginSettings := dockershim.NetworkPluginSettings{
		HairpinMode:       hairpinMode,
		NonMasqueradeCIDR: nonMasqueradeCIDR,
		PluginName:        crOptions.NetworkPluginName,
		PluginConfDir:     crOptions.CNIConfDir,
		PluginBinDir:      crOptions.CNIBinDir,
		MTU:               int(crOptions.NetworkPluginMTU),
	}

	// ResourceAnalyzer提供了关于节点资源损耗的数据
	klet.resourceAnalyzer = serverstats.NewResourceAnalyzer(klet, kubeCfg.VolumeStatsAggPeriod.Duration)

	// Remote runtime shim just cannot talk back to kubelet, so it doesn't
	// support bandwidth shaping or hostports till #35457. To enable legacy
	// features, replace with networkHost.
	var nl *NoOpLegacyHost
	pluginSettings.LegacyRuntimeHost = nl

	// rktnetes cannot be run with CRI.
	// rktnetes不能以CRI方式启动
	if containerRuntime != kubetypes.RktContainerRuntime {
		// kubelet defers to the runtime shim to setup networking. Setting
		// this to nil will prevent it from trying to invoke the plugin.
		// It's easier to always probe and initialize plugins till cri
		// becomes the default.
		// kubelet会让runtime shim去设置networking
		// 将其设置为nil可以防止对插件进行调用，在cri变为默认配置之前，总是去探测并且
		// 初始化插件是更为简单的方法
		klet.networkPlugin = nil
		// if left at nil, that means it is unneeded
		var legacyLogProvider kuberuntime.LegacyLogProvider

		switch containerRuntime {
		// 如果运行时为docker
		case kubetypes.DockerContainerRuntime:
			// Create and start the CRI shim running as a grpc server.
			// 创建并且启动CRI shim作为grpc server
			streamingConfig := getStreamingConfig(kubeCfg, kubeDeps)
			ds, err := dockershim.NewDockerService(kubeDeps.DockerClientConfig, crOptions.PodSandboxImage, streamingConfig,
				&pluginSettings, runtimeCgroups, kubeCfg.CgroupDriver, crOptions.DockershimRootDirectory,
				crOptions.DockerDisableSharedPID)
			if err != nil {
				return nil, err
			}
			// 启动docker shime
			if err := ds.Start(); err != nil {
				return nil, err
			}
			// For now, the CRI shim redirects the streaming requests to the
			// kubelet, which handles the requests using DockerService..
			klet.criHandler = ds

			// The unix socket for kubelet <-> dockershim communication.
			glog.V(5).Infof("RemoteRuntimeEndpoint: %q, RemoteImageEndpoint: %q",
				remoteRuntimeEndpoint,
				remoteImageEndpoint)
			glog.V(2).Infof("Starting the GRPC server for the docker CRI shim.")
			// 启动docker CRI shim的GRPC server
			server := dockerremote.NewDockerServer(remoteRuntimeEndpoint, ds)
			if err := server.Start(); err != nil {
				return nil, err
			}

			// Create dockerLegacyService when the logging driver is not supported.
			// 创建dockerLegacyService，如果不支持logging driver的话
			supported, err := ds.IsCRISupportedLogDriver()
			if err != nil {
				return nil, err
			}
			if !supported {
				klet.dockerLegacyService = ds.NewDockerLegacyService()
				legacyLogProvider = dockershim.NewLegacyLogProvider(klet.dockerLegacyService)
			}
		case kubetypes.RemoteContainerRuntime:
			// 对于远程的CRI不做任何操作
			// No-op.
			break
		default:
			return nil, fmt.Errorf("unsupported CRI runtime: %q", containerRuntime)
		}
		// 获取runtime service和image service，并将它们封装进generic runtime manager中
		runtimeService, imageService, err := getRuntimeAndImageServices(remoteRuntimeEndpoint, remoteImageEndpoint, kubeCfg.RuntimeRequestTimeout)
		if err != nil {
			return nil, err
		}
		klet.runtimeService = runtimeService
		// 将runtime service和image service都封装进generic runtime manager中
		runtime, err := kuberuntime.NewKubeGenericRuntimeManager(
			kubecontainer.FilterEventRecorder(kubeDeps.Recorder),
			klet.livenessManager,
			seccompProfileRoot,
			containerRefManager,
			machineInfo,
			klet,
			kubeDeps.OSInterface,
			klet,
			httpClient,
			imageBackOff,
			kubeCfg.SerializeImagePulls,
			float32(kubeCfg.RegistryPullQPS),
			int(kubeCfg.RegistryBurst),
			kubeCfg.CPUCFSQuota,
			runtimeService,
			imageService,
			kubeDeps.ContainerManager.InternalContainerLifecycle(),
			legacyLogProvider,
		)
		if err != nil {
			return nil, err
		}
		// containerRuntime和runner都满足了CRI的所有接口
		klet.containerRuntime = runtime
		klet.runner = runtime

		// 如果container stats是使用cadvisor返回容器数据，则返回true
		// 通过CRI返回容器数据则返回false
		if cadvisor.UsingLegacyCadvisorStats(containerRuntime, remoteRuntimeEndpoint) {
			klet.StatsProvider = stats.NewCadvisorStatsProvider(
				klet.cadvisor,
				klet.resourceAnalyzer,
				klet.podManager,
				klet.runtimeCache,
				klet.containerRuntime)
		} else {
			// Kubelet包含了StatsProvider的所有方法
			klet.StatsProvider = stats.NewCRIStatsProvider(
				klet.cadvisor,
				klet.resourceAnalyzer,
				klet.podManager,
				klet.runtimeCache,
				runtimeService,
				imageService)
		}
	} else {
		// rkt uses the legacy, non-CRI, integration. Configure it the old way.
		// TODO: Include hairpin mode settings in rkt?
		conf := &rkt.Config{
			Path:            crOptions.RktPath,
			Stage1Image:     crOptions.RktStage1Image,
			InsecureOptions: "image,ondisk",
		}
		runtime, err := rkt.New(
			crOptions.RktAPIEndpoint,
			conf,
			klet,
			kubeDeps.Recorder,
			containerRefManager,
			klet,
			klet.livenessManager,
			httpClient,
			klet.networkPlugin,
			hairpinMode == kubeletconfiginternal.HairpinVeth,
			utilexec.New(),
			kubecontainer.RealOS{},
			imageBackOff,
			kubeCfg.SerializeImagePulls,
			float32(kubeCfg.RegistryPullQPS),
			int(kubeCfg.RegistryBurst),
			kubeCfg.RuntimeRequestTimeout.Duration,
		)
		if err != nil {
			return nil, err
		}
		klet.containerRuntime = runtime
		klet.runner = kubecontainer.DirectStreamingRunner(runtime)
		klet.StatsProvider = stats.NewCadvisorStatsProvider(
			klet.cadvisor,
			klet.resourceAnalyzer,
			klet.podManager,
			klet.runtimeCache,
			klet.containerRuntime)
	}

	// 创建通用的Pod Lifecycle Event Generator
	// 每1秒relist一次
	klet.pleg = pleg.NewGenericPLEG(klet.containerRuntime, plegChannelCapacity, plegRelistPeriod, klet.podCache, clock.RealClock{})
	// maxWaitForContainerRuntime是等待容器运行时启动的时间
	klet.runtimeState = newRuntimeState(maxWaitForContainerRuntime)
	klet.runtimeState.addHealthCheck("PLEG", klet.pleg.Healthy)
	klet.updatePodCIDR(kubeCfg.PodCIDR)

	// setup containerGC
	// 创建container GC
	containerGC, err := kubecontainer.NewContainerGC(klet.containerRuntime, containerGCPolicy, klet.sourcesReady)
	if err != nil {
		return nil, err
	}
	klet.containerGC = containerGC
	klet.containerDeletor = newPodContainerDeletor(klet.containerRuntime, integer.IntMax(containerGCPolicy.MaxPerPodContainer, minDeadContainerInPod))

	// setup imageManager
	// 设置image manager
	imageManager, err := images.NewImageGCManager(klet.containerRuntime, klet.StatsProvider, kubeDeps.Recorder, nodeRef, imageGCPolicy)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize image manager: %v", err)
	}
	klet.imageManager = imageManager

	// 创建status manager
	klet.statusManager = status.NewManager(klet.kubeClient, klet.podManager, klet)

	if utilfeature.DefaultFeatureGate.Enabled(features.RotateKubeletServerCertificate) && kubeDeps.TLSOptions != nil {
		var ips []net.IP
		cfgAddress := net.ParseIP(kubeCfg.Address)
		if cfgAddress == nil || cfgAddress.IsUnspecified() {
			localIPs, err := allLocalIPsWithoutLoopback()
			if err != nil {
				return nil, err
			}
			ips = localIPs
		} else {
			ips = []net.IP{cfgAddress}
		}

		ips = append(ips, cloudIPs...)
		names := append([]string{klet.GetHostname(), hostnameOverride}, cloudNames...)
		klet.serverCertificateManager, err = kubeletcertificate.NewKubeletServerCertificateManager(klet.kubeClient, kubeCfg, klet.nodeName, ips, names, certDirectory)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize certificate manager: %v", err)
		}
		kubeDeps.TLSOptions.Config.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			cert := klet.serverCertificateManager.Current()
			if cert == nil {
				return nil, fmt.Errorf("no certificate available")
			}
			return cert, nil
		}
	}

	// 创建probe manager
	klet.probeManager = prober.NewManager(
		klet.statusManager,
		klet.livenessManager,
		klet.runner,
		containerRefManager,
		kubeDeps.Recorder)

	// 创建volume plugin manager
	klet.volumePluginMgr, err =
		NewInitializedVolumePluginMgr(klet, secretManager, configMapManager, kubeDeps.VolumePlugins, kubeDeps.DynamicPluginProber)
	if err != nil {
		return nil, err
	}

	// If the experimentalMounterPathFlag is set, we do not want to
	// check node capabilities since the mount path is not the default
	if len(experimentalMounterPath) != 0 {
		experimentalCheckNodeCapabilitiesBeforeMount = false
		// Replace the nameserver in containerized-mounter's rootfs/etc/resolve.conf with kubelet.ClusterDNS
		// so that service name could be resolved
		klet.dnsConfigurer.SetupDNSinContainerizedMounter(experimentalMounterPath)
	}

	// setup volumeManager
	// 设置volume manager
	klet.volumeManager = volumemanager.NewVolumeManager(
		kubeCfg.EnableControllerAttachDetach,
		nodeName,
		klet.podManager,
		klet.statusManager,
		klet.kubeClient,
		klet.volumePluginMgr,
		klet.containerRuntime,
		kubeDeps.Mounter,
		klet.getPodsDir(),
		kubeDeps.Recorder,
		experimentalCheckNodeCapabilitiesBeforeMount,
		keepTerminatedPodVolumes)

	// 创建runtime cache，直接调用的kubelet.containerRuntime
	runtimeCache, err := kubecontainer.NewRuntimeCache(klet.containerRuntime)
	if err != nil {
		return nil, err
	}
	klet.runtimeCache = runtimeCache
	klet.reasonCache = NewReasonCache()
	klet.workQueue = queue.NewBasicWorkQueue(klet.clock)
	// 创建pod workers
	klet.podWorkers = newPodWorkers(klet.syncPod, kubeDeps.Recorder, klet.workQueue, klet.resyncInterval, backOffPeriod, klet.podCache)

	klet.backOff = flowcontrol.NewBackOff(backOffPeriod, MaxContainerBackOff)
	klet.podKillingCh = make(chan *kubecontainer.PodPair, podKillingChannelCapacity)
	klet.setNodeStatusFuncs = klet.defaultNodeStatusFuncs()

	// setup eviction manager
	// 设置eviction manager
	evictionManager, evictionAdmitHandler := eviction.NewManager(klet.resourceAnalyzer, evictionConfig, killPodNow(klet.podWorkers, kubeDeps.Recorder), klet.imageManager, klet.containerGC, kubeDeps.Recorder, nodeRef, klet.clock)

	klet.evictionManager = evictionManager
	klet.admitHandlers.AddPodAdmitHandler(evictionAdmitHandler)

	// add sysctl admission
	runtimeSupport, err := sysctl.NewRuntimeAdmitHandler(klet.containerRuntime)
	if err != nil {
		return nil, err
	}
	safeWhitelist, err := sysctl.NewWhitelist(sysctl.SafeSysctlWhitelist(), v1.SysctlsPodAnnotationKey)
	if err != nil {
		return nil, err
	}
	// Safe, whitelisted sysctls can always be used as unsafe sysctls in the spec
	// Hence, we concatenate those two lists.
	safeAndUnsafeSysctls := append(sysctl.SafeSysctlWhitelist(), allowedUnsafeSysctls...)
	unsafeWhitelist, err := sysctl.NewWhitelist(safeAndUnsafeSysctls, v1.UnsafeSysctlsPodAnnotationKey)
	if err != nil {
		return nil, err
	}
	klet.admitHandlers.AddPodAdmitHandler(runtimeSupport)
	klet.admitHandlers.AddPodAdmitHandler(safeWhitelist)
	klet.admitHandlers.AddPodAdmitHandler(unsafeWhitelist)

	// enable active deadline handler
	activeDeadlineHandler, err := newActiveDeadlineHandler(klet.statusManager, kubeDeps.Recorder, klet.clock)
	if err != nil {
		return nil, err
	}
	klet.AddPodSyncLoopHandler(activeDeadlineHandler)
	klet.AddPodSyncHandler(activeDeadlineHandler)

	criticalPodAdmissionHandler := preemption.NewCriticalPodAdmissionHandler(klet.GetActivePods, killPodNow(klet.podWorkers, kubeDeps.Recorder), kubeDeps.Recorder)
	klet.admitHandlers.AddPodAdmitHandler(lifecycle.NewPredicateAdmitHandler(klet.getNodeAnyWay, criticalPodAdmissionHandler, klet.containerManager.UpdatePluginResources))
	// apply functional Option's
	for _, opt := range kubeDeps.Options {
		opt(klet)
	}

	klet.appArmorValidator = apparmor.NewValidator(containerRuntime)
	klet.softAdmitHandlers.AddPodAdmitHandler(lifecycle.NewAppArmorAdmitHandler(klet.appArmorValidator))
	klet.softAdmitHandlers.AddPodAdmitHandler(lifecycle.NewNoNewPrivsAdmitHandler(klet.containerRuntime))
	if utilfeature.DefaultFeatureGate.Enabled(features.Accelerators) {
		if containerRuntime == kubetypes.DockerContainerRuntime {
			// 只有在运行时为docker的时候，才会创建gpu manager
			if klet.gpuManager, err = nvidia.NewNvidiaGPUManager(klet, kubeDeps.DockerClientConfig); err != nil {
				return nil, err
			}
		} else {
			glog.Errorf("Accelerators feature is supported with docker runtime only. Disabling this feature internally.")
		}
	}
	// Set GPU manager to a stub implementation if it is not enabled or cannot be supported.
	if klet.gpuManager == nil {
		klet.gpuManager = gpu.NewGPUManagerStub()
	}
	// Finally, put the most recent version of the config on the Kubelet, so
	// people can see how it was configured.
	// 将最新的配置信息存入kubelet,这样就能看到它是怎么配置的了
	klet.kubeletConfiguration = *kubeCfg
	return klet, nil
}

type serviceLister interface {
	List(labels.Selector) ([]*v1.Service, error)
}

// Kubelet is the main kubelet implementation.
type Kubelet struct {
	// kubelet的配置保存在kubeletConfiguration中
	kubeletConfiguration kubeletconfiginternal.KubeletConfiguration

	hostname        string
	nodeName        types.NodeName
	runtimeCache    kubecontainer.RuntimeCache
	kubeClient      clientset.Interface
	heartbeatClient v1core.CoreV1Interface
	iptClient       utilipt.Interface
	rootDirectory   string

	// podWorkers handle syncing Pods in response to events.
	// podWorkers处理events用于同步pods
	podWorkers PodWorkers

	// resyncInterval is the interval between periodic full reconciliations of
	// pods on this node.
	resyncInterval time.Duration

	// sourcesReady records the sources seen by the kubelet, it is thread-safe.
	// sourcesReady中记录了kubelet可见的sources
	sourcesReady config.SourcesReady

	// podManager is a facade that abstracts away the various sources of pods
	// this Kubelet services.
	// podManager抽象了kubelet能够服务的各个来源的pods
	podManager kubepod.Manager

	// Needed to observe and respond to situations that could impact node stability
	// evictionManager用于观察并且对那些能够影响节点稳定性的事件作出回应
	evictionManager eviction.Manager

	// Optional, defaults to /logs/ from /var/log
	logServer http.Handler
	// Optional, defaults to simple Docker implementation
	// 可选项，默认为简单的Docker实现
	runner kubecontainer.ContainerCommandRunner

	// cAdvisor used for container information.
	cadvisor cadvisor.Interface

	// Set to true to have the node register itself with the apiserver.
	// 如果节点已经注册到api server中的话，设置为true
	registerNode bool
	// List of taints to add to a node object when the kubelet registers itself.
	// 当kubelet注册自己时，添加到node object中的一系列taints
	registerWithTaints []api.Taint
	// Set to true to have the node register itself as schedulable.
	// 如果node将自己设置为可调度的，则设置为true
	registerSchedulable bool
	// for internal book keeping; access only from within registerWithApiserver
	registrationCompleted bool

	// dnsConfigurer is used for setting up DNS resolver configuration when launching pods.
	// dnsConfigurer用于在启动pods时设置DNS resolver configuration
	dnsConfigurer *dns.Configurer

	// masterServiceNamespace is the namespace that the master service is exposed in.
	masterServiceNamespace string
	// serviceLister knows how to list services
	// serviceLister知道怎么list services
	serviceLister serviceLister
	// nodeInfo knows how to get information about the node for this kubelet.
	// nodeInfo知道如何为该kubelet获取node的信息
	nodeInfo predicates.NodeInfo

	// a list of node labels to register
	nodeLabels map[string]string

	// Last timestamp when runtime responded on ping.
	// Mutex is used to protect this value.
	// 运行时回应ping的最新timestamp
	runtimeState *runtimeState

	// Volume plugins.
	volumePluginMgr *volume.VolumePluginMgr

	// Network plugin.
	networkPlugin network.NetworkPlugin

	// Handles container probing.
	probeManager prober.Manager
	// Manages container health check results.
	livenessManager proberesults.Manager

	// How long to keep idle streaming command execution/port forwarding
	// connections open before terminating them
	streamingConnectionIdleTimeout time.Duration

	// The EventRecorder to use
	recorder record.EventRecorder

	// Policy for handling garbage collection of dead containers.
	containerGC kubecontainer.ContainerGC

	// Manager for image garbage collection.
	imageManager images.ImageGCManager

	// Secret manager.
	secretManager secret.Manager

	// ConfigMap manager.
	configMapManager configmap.Manager

	// Cached MachineInfo returned by cadvisor.
	// cadvisor返回的Cached MachineInfo
	machineInfo *cadvisorapi.MachineInfo

	//Cached RootFsInfo returned by cadvisor
	// cadvisor返回的Cached RootFsInfo
	rootfsInfo *cadvisorapiv2.FsInfo

	// Handles certificate rotations.
	serverCertificateManager certificate.Manager

	// Syncs pods statuses with apiserver; also used as a cache of statuses.
	// 和apiserver同步pod statuses；同时也可以作为statuses的cache
	statusManager status.Manager

	// VolumeManager runs a set of asynchronous loops that figure out which
	// volumes need to be attached/mounted/unmounted/detached based on the pods
	// scheduled on this node and makes it so.
	// VolumeManager运行一系列的asynchronous loops，它们能够知道哪些volumes需要
	// 被attached/mounted/unmounted/detached，基于pod被调度到本节点的情况，并将其
	// 实现
	volumeManager volumemanager.VolumeManager

	// Cloud provider interface.
	cloud cloudprovider.Interface
	// DEPRECATED: auto detecting cloud providers goes against the initiative
	// for out-of-tree cloud providers as we'll now depend on cAdvisor integrations
	// with cloud providers instead of in the core repo.
	// More details here: https://github.com/kubernetes/kubernetes/issues/50986
	autoDetectCloudProvider bool
	// Indicates that the node initialization happens in an external cloud controller
	externalCloudProvider bool
	// Reference to this node.
	// 本node的引用
	nodeRef *v1.ObjectReference

	// The name of the container runtime
	containerRuntimeName string

	// Container runtime.
	// 容器运行时
	containerRuntime kubecontainer.Runtime

	// Container runtime service (needed by container runtime Start()).
	// TODO(CD): try to make this available without holding a reference in this
	//           struct. For example, by adding a getter to generic runtime.
	runtimeService internalapi.RuntimeService

	// reasonCache caches the failure reason of the last creation of all containers, which is
	// used for generating ContainerStatus.
	// reasonCache缓存了所有容器最新的creation失败的原因，这可以用来产生ContainerStatus
	reasonCache *ReasonCache

	// nodeStatusUpdateFrequency specifies how often kubelet posts node status to master.
	// nodeStatusUpdateFrequency指定了kubelet向master汇报节点状态的频率
	// Note: be cautious when changing the constant, it must work with nodeMonitorGracePeriod
	// in nodecontroller. There are several constraints:
	// 它必须和nodecontroller的nodeMonitorGracePeriod一致
	// 1. nodeMonitorGracePeriod must be N times more than nodeStatusUpdateFrequency, where
	//    N means number of retries allowed for kubelet to post node status. It is pointless
	//    to make nodeMonitorGracePeriod be less than nodeStatusUpdateFrequency, since there
	//    will only be fresh values from Kubelet at an interval of nodeStatusUpdateFrequency.
	//    The constant must be less than podEvictionTimeout.
	// 2. nodeStatusUpdateFrequency needs to be large enough for kubelet to generate node
	//    status. Kubelet may fail to update node status reliably if the value is too small,
	//    as it takes time to gather all necessary node information.
	// nodeStatusUpdateFrequency必须足够大，大到足以让kubelet产生node status
	// 如果值太小，kubelet可能不能可靠地更新节点状态，因为收集必要的节点信息需要时间
	nodeStatusUpdateFrequency time.Duration

	// Generates pod events.
	// 产生pod events
	pleg pleg.PodLifecycleEventGenerator

	// Store kubecontainer.PodStatus for all pods.
	podCache kubecontainer.Cache

	// os is a facade for various syscalls that need to be mocked during testing.
	os kubecontainer.OSInterface

	// Watcher of out of memory events.
	oomWatcher OOMWatcher

	// Monitor resource usage
	// 监控容器使用情况
	resourceAnalyzer serverstats.ResourceAnalyzer

	// Whether or not we should have the QOS cgroup hierarchy for resource management
	// 是否有QOS cgroup hierarchy用于资源管理
	cgroupsPerQOS bool

	// If non-empty, pass this to the container runtime as the root cgroup.
	cgroupRoot string

	// Mounter to use for volumes.
	// Mounter用于volumes
	mounter mount.Interface

	// Writer interface to use for volumes.
	// volumes使用的Writer interface
	writer kubeio.Writer

	// Manager of non-Runtime containers.
	// 管理non-Runtime容器的manager
	containerManager cm.ContainerManager

	// Maximum Number of Pods which can be run by this Kubelet
	// 通过该kubelet能运行的最大数量的Pods
	maxPods int

	// Monitor Kubelet's sync loop
	// 监视Kubelet的sync loop
	syncLoopMonitor atomic.Value

	// Container restart Backoff
	// 容器重启的Backoff
	backOff *flowcontrol.Backoff

	// Channel for sending pods to kill.
	// 用于传送需要被kill的pods的Channel
	podKillingCh chan *kubecontainer.PodPair

	// Information about the ports which are opened by daemons on Node running this Kubelet server.
	daemonEndpoints *v1.NodeDaemonEndpoints

	// A queue used to trigger pod workers.
	// 用于触发pod workers的队列
	workQueue queue.WorkQueue

	// oneTimeInitializer is used to initialize modules that are dependent on the runtime to be up.
	// oneTimeInitializer用于初始化那些依赖于容器运行时先启动的模块
	oneTimeInitializer sync.Once

	// If non-nil, use this IP address for the node
	nodeIP net.IP

	// If non-nil, this is a unique identifier for the node in an external database, eg. cloudprovider
	providerID string

	// clock is an interface that provides time related functionality in a way that makes it
	// easy to test the code.
	clock clock.Clock

	// handlers called during the tryUpdateNodeStatus cycle
	// 在tryUpdatedNodeStatus中被调用的handlers
	setNodeStatusFuncs []func(*v1.Node) error

	// TODO: think about moving this to be centralized in PodWorkers in follow-on.
	// the list of handlers to call during pod admission.
	// 在pod admission期间需要调用的一系列handlers
	admitHandlers lifecycle.PodAdmitHandlers

	// softAdmithandlers are applied to the pod after it is admitted by the Kubelet, but before it is
	// run. A pod rejected by a softAdmitHandler will be left in a Pending state indefinitely. If a
	// rejected pod should not be recreated, or the scheduler is not aware of the rejection rule, the
	// admission rule should be applied by a softAdmitHandler.
	// softAdmithandlers会在pod被Kubelet接收，但在真正运行之前被调用
	// 被softAdmitHandler拒绝的pod会始终处于Pending状态，如果一个rejected pod不应该被重新创建，或者scheduler没有意识到
	// rejection rule，admission rule应该由softAdmitHandler进行应用
	softAdmitHandlers lifecycle.PodAdmitHandlers

	// the list of handlers to call during pod sync loop.
	// 在pod sync loop中应该被调用的一系列handlers
	lifecycle.PodSyncLoopHandlers

	// the list of handlers to call during pod sync.
	// 在pod sync中被调用的一系列handlers
	lifecycle.PodSyncHandlers

	// the number of allowed pods per core
	// 每个core上允许的pod的数目
	podsPerCore int

	// enableControllerAttachDetach indicates the Attach/Detach controller
	// should manage attachment/detachment of volumes scheduled to this node,
	// and disable kubelet from executing any attach/detach operations
	// enableControllerAttachDetach表示Attach/Detach controller是否应该管理
	// 被调度到本节点的volumes的attachment/detachment
	// 并且防止kubelet执行任何的attach/detach操作
	enableControllerAttachDetach bool

	// trigger deleting containers in a pod
	// 触发一个pod中删除容器的操作
	containerDeletor *podContainerDeletor

	// config iptables util rules
	makeIPTablesUtilChains bool

	// The bit of the fwmark space to mark packets for SNAT.
	// 用于标记SNAT的packets
	iptablesMasqueradeBit int

	// The bit of the fwmark space to mark packets for dropping.
	// 用于标记被丢弃的packets
	iptablesDropBit int

	// The AppArmor validator for checking whether AppArmor is supported.
	appArmorValidator apparmor.Validator

	// The handler serving CRI streaming calls (exec/attach/port-forward).
	// 用于支持CRI streaming calls的handler
	criHandler http.Handler

	// experimentalHostUserNamespaceDefaulting sets userns=true when users request host namespaces (pid, ipc, net),
	// are using non-namespaced capabilities (mknod, sys_time, sys_module), the pod contains a privileged container,
	// or using host path volumes.
	// This should only be enabled when the container runtime is performing user remapping AND if the
	// experimental behavior is desired.
	experimentalHostUserNamespaceDefaulting bool

	// GPU Manager
	gpuManager gpu.GPUManager

	// dockerLegacyService contains some legacy methods for backward compatibility.
	// It should be set only when docker is using non json-file logging driver.
	// dockerLegacyService包含了一些向后兼容的legacy methods
	// 这只有在docker使用non json-file logging driver时才会被设置
	dockerLegacyService dockershim.DockerLegacyService

	// StatsProvider provides the node and the container stats.
	*stats.StatsProvider

	// containerized should be set to true if the kubelet is running in a container
	// 表示kubelet是否应该运行在容器中
	containerized bool

	// This flag, if set, instructs the kubelet to keep volumes from terminated pods mounted to the node.
	// This can be useful for debugging volume related issues.
	keepTerminatedPodVolumes bool // DEPRECATED
}

func allLocalIPsWithoutLoopback() ([]net.IP, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("could not list network interfaces: %v", err)
	}
	var ips []net.IP
	for _, i := range interfaces {
		addresses, err := i.Addrs()
		if err != nil {
			return nil, fmt.Errorf("could not list the addresses for network interface %v: %v", i, err)
		}
		for _, address := range addresses {
			switch v := address.(type) {
			case *net.IPNet:
				if !v.IP.IsLoopback() {
					ips = append(ips, v.IP)
				}
			}
		}
	}
	return ips, nil
}

// setupDataDirs creates:
// 1.  the root directory
// 2.  the pods directory
// 3.  the plugins directory
func (kl *Kubelet) setupDataDirs() error {
	kl.rootDirectory = path.Clean(kl.rootDirectory)
	if err := os.MkdirAll(kl.getRootDir(), 0750); err != nil {
		return fmt.Errorf("error creating root directory: %v", err)
	}
	if err := kl.mounter.MakeRShared(kl.getRootDir()); err != nil {
		return fmt.Errorf("error configuring root directory: %v", err)
	}
	if err := os.MkdirAll(kl.getPodsDir(), 0750); err != nil {
		return fmt.Errorf("error creating pods directory: %v", err)
	}
	if err := os.MkdirAll(kl.getPluginsDir(), 0750); err != nil {
		return fmt.Errorf("error creating plugins directory: %v", err)
	}
	return nil
}

// StartGarbageCollection starts garbage collection threads.
// StartGarbageCollection启动GC线程
func (kl *Kubelet) StartGarbageCollection() {
	loggedContainerGCFailure := false
	// 启动goroutine，开始container的GC
	go wait.Until(func() {
		if err := kl.containerGC.GarbageCollect(); err != nil {
			glog.Errorf("Container garbage collection failed: %v", err)
			// container的GC失败，发送ContainerGCFailed事件
			kl.recorder.Eventf(kl.nodeRef, v1.EventTypeWarning, events.ContainerGCFailed, err.Error())
			loggedContainerGCFailure = true
		} else {
			var vLevel glog.Level = 4
			if loggedContainerGCFailure {
				vLevel = 1
				loggedContainerGCFailure = false
			}

			glog.V(vLevel).Infof("Container garbage collection succeeded")
		}
	}, ContainerGCPeriod, wait.NeverStop)

	prevImageGCFailed := false
	// 启动goroutine，开始image的GC
	go wait.Until(func() {
		if err := kl.imageManager.GarbageCollect(); err != nil {
			if prevImageGCFailed {
				glog.Errorf("Image garbage collection failed multiple times in a row: %v", err)
				// Only create an event for repeated failures
				// 只在重复的失败之后才发送ImageGCFailed事件
				kl.recorder.Eventf(kl.nodeRef, v1.EventTypeWarning, events.ImageGCFailed, err.Error())
			} else {
				glog.Errorf("Image garbage collection failed once. Stats initialization may not have completed yet: %v", err)
			}
			prevImageGCFailed = true
		} else {
			var vLevel glog.Level = 4
			if prevImageGCFailed {
				vLevel = 1
				prevImageGCFailed = false
			}

			glog.V(vLevel).Infof("Image garbage collection succeeded")
		}
	}, ImageGCPeriod, wait.NeverStop)
}

// initializeModules will initialize internal modules that do not require the container runtime to be up.
// Note that the modules here must not depend on modules that are not initialized here.
// initializeModules会初始化那些不依赖于container runtime已经启动的internal modules
// 这里的module不能依赖于那些还没有初始化的module
func (kl *Kubelet) initializeModules() error {
	// Prometheus metrics.
	metrics.Register(kl.runtimeCache)

	// Setup filesystem directories.
	if err := kl.setupDataDirs(); err != nil {
		return err
	}

	// If the container logs directory does not exist, create it.
	if _, err := os.Stat(ContainerLogsDir); err != nil {
		if err := kl.os.MkdirAll(ContainerLogsDir, 0755); err != nil {
			glog.Errorf("Failed to create directory %q: %v", ContainerLogsDir, err)
		}
	}

	// Start the image manager.
	// 启动image manager
	kl.imageManager.Start()

	// Start the certificate manager.
	if utilfeature.DefaultFeatureGate.Enabled(features.RotateKubeletServerCertificate) {
		kl.serverCertificateManager.Start()
	}

	// Start container manager.
	// 启动容器manager
	node, err := kl.getNodeAnyWay()
	if err != nil {
		return fmt.Errorf("Kubelet failed to get node info: %v", err)
	}

	if err := kl.containerManager.Start(node, kl.GetActivePods, kl.sourcesReady, kl.statusManager, kl.runtimeService); err != nil {
		return fmt.Errorf("Failed to start ContainerManager %v", err)
	}

	// Start out of memory watcher.
	if err := kl.oomWatcher.Start(kl.nodeRef); err != nil {
		return fmt.Errorf("Failed to start OOM watcher %v", err)
	}

	// Initialize GPUs
	// 初始化GPU
	if err := kl.gpuManager.Start(); err != nil {
		glog.Errorf("Failed to start gpuManager %v", err)
	}

	// Start resource analyzer
	kl.resourceAnalyzer.Start()

	return nil
}

// initializeRuntimeDependentModules will initialize internal modules that require the container runtime to be up.
// initializeRuntimeDependentModules会初始化那些需要等待容器运行时开始运行之后才能运行的模块
func (kl *Kubelet) initializeRuntimeDependentModules() {
	// 启动cadvisor
	if err := kl.cadvisor.Start(); err != nil {
		// Fail kubelet and rely on the babysitter to retry starting kubelet.
		// TODO(random-liu): Add backoff logic in the babysitter
		glog.Fatalf("Failed to start cAdvisor %v", err)
	}
	// eviction manager must start after cadvisor because it needs to know if the container runtime has a dedicated imagefs
	// eviction manager必须在cadvisor启动之后才能启动，因为它需要知道容器运行时是否有自己的imagefs
	kl.evictionManager.Start(kl.cadvisor, kl.GetActivePods, kl.podResourcesAreReclaimed, kl.containerManager, evictionMonitoringPeriod)
}

// Run starts the kubelet reacting to config updates
// Run启动kubelet,并对config update做出反应
func (kl *Kubelet) Run(updates <-chan kubetypes.PodUpdate) {
	if kl.logServer == nil {
		kl.logServer = http.StripPrefix("/logs/", http.FileServer(http.Dir("/var/log/")))
	}
	if kl.kubeClient == nil {
		// 没有定义api server，因此不会发送node status update
		glog.Warning("No api server defined - no node status update will be sent.")
	}

	// 创建目录，启动image manager, container manager, oomWatcher
	// gpu manager以及resource analyzer等等
	if err := kl.initializeModules(); err != nil {
		kl.recorder.Eventf(kl.nodeRef, v1.EventTypeWarning, events.KubeletSetupFailed, err.Error())
		glog.Fatal(err)
	}

	// Start volume manager
	go kl.volumeManager.Run(kl.sourcesReady, wait.NeverStop)

	if kl.kubeClient != nil {
		// Start syncing node status immediately, this may set up things the runtime needs to run.
		// 开始同步节点状态，这可能会设置runtime运行需要的东西
		go wait.Until(kl.syncNodeStatus, kl.nodeStatusUpdateFrequency, wait.NeverStop)
	}
	go wait.Until(kl.syncNetworkStatus, 30*time.Second, wait.NeverStop)
	go wait.Until(kl.updateRuntimeUp, 5*time.Second, wait.NeverStop)

	// Start loop to sync iptables util rules
	if kl.makeIPTablesUtilChains {
		go wait.Until(kl.syncNetworkUtil, 1*time.Minute, wait.NeverStop)
	}

	// Start a goroutine responsible for killing pods (that are not properly
	// handled by pod workers).
	// 创建一个goroutine负责killing pods（那些没有被pod workers正确处理的pods）
	go wait.Until(kl.podKiller, 1*time.Second, wait.NeverStop)

	// Start gorouting responsible for checking limits in resolv.conf
	if kl.dnsConfigurer.ResolverConfig != "" {
		go wait.Until(func() { kl.dnsConfigurer.CheckLimitsForResolvConf() }, 30*time.Second, wait.NeverStop)
	}

	// Start component sync loops.
	// 启动组件的sync loop
	kl.statusManager.Start()
	kl.probeManager.Start()

	// Start the pod lifecycle event generator.
	// 启动pod lifecycle的事件发生器
	kl.pleg.Start()
	kl.syncLoop(updates, kl)
}

// GetKubeClient returns the Kubernetes client.
// TODO: This is currently only required by network plugins. Replace
// with more specific methods.
func (kl *Kubelet) GetKubeClient() clientset.Interface {
	return kl.kubeClient
}

// syncPod is the transaction script for the sync of a single pod.
//
// Arguments:
//
// o - the SyncPodOptions for this invocation
//
// The workflow is:
// * If the pod is being created, record pod worker start latency
// * 如果pod是被创建的，记录pod worker启动的时延
// * Call generateAPIPodStatus to prepare an v1.PodStatus for the pod
// * 调用generateAPIPodStatus为pod准备一个v1.PodStatus
// * If the pod is being seen as running for the first time, record pod
//   start latency
// * 如果pod第一次被看到处于running状态，则记录pod的启动时间
// * Update the status of the pod in the status manager
// * 更新status manager中的pod状态
// * Kill the pod if it should not be running
// * 杀死pod，如果它不能运行的话
// * Create a mirror pod if the pod is a static pod, and does not
//   already have a mirror pod
// * 如果pod是一个static pod并且还没有mirror pod，则创建一个mirror pod
// * Create the data directories for the pod if they do not exist
// * 创建data目录，如果pod还没有的话
// * Wait for volumes to attach/mount
// * 等待volume的attach以及mount
// * Fetch the pull secrets for the pod
// * 获取pod的pull secrets
// * Call the container runtime's SyncPod callback
// * 调用容器运行时的SyncPod回调函数
// * Update the traffic shaping for the pod's ingress and egress limits
// * 更新traffic shaping用于pod的ingress以及egress limits
//
// If any step of this workflow errors, the error is returned, and is repeated
// on the next syncPod call.
// 如果workflow中的任何一步发生了错误，返回error，并且在下一次调用syncPod时重新运行
//
// This operation writes all events that are dispatched in order to provide
// the most accurate information possible about an error situation to aid debugging.
// Callers should not throw an event if this operation returns an error.
// 只有在syncPod方法内，才会调用statusManager的SetPodStatus方法
func (kl *Kubelet) syncPod(o syncPodOptions) error {
	// pull out the required options
	pod := o.pod
	mirrorPod := o.mirrorPod
	// podStatus直接从syncPodOptions中获取
	podStatus := o.podStatus
	updateType := o.updateType

	// if we want to kill a pod, do it now!
	// 如果我们的目标是kill pod，那么立即执行
	if updateType == kubetypes.SyncPodKill {
		killPodOptions := o.killPodOptions
		// 如果是为了kill pod，则一定要在syncPodOptions中设置killPodOptions
		if killPodOptions == nil || killPodOptions.PodStatusFunc == nil {
			return fmt.Errorf("kill pod options are required if update type is kill")
		}
		// 创建符合apiserver要求的pod status
		apiPodStatus := killPodOptions.PodStatusFunc(pod, podStatus)
		// 设置status manager的pod status
		kl.statusManager.SetPodStatus(pod, apiPodStatus)
		// we kill the pod with the specified grace period since this is a termination
		// 我们根据给定的grace period来kill pod，因为这是一个termination
		if err := kl.killPod(pod, nil, podStatus, killPodOptions.PodTerminationGracePeriodSecondsOverride); err != nil {
			kl.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedToKillPod, "error killing pod: %v", err)
			// there was an error killing the pod, so we return that error directly
			utilruntime.HandleError(err)
			return err
		}
		return nil
	}

	// Latency measurements for the main workflow are relative to the
	// first time the pod was seen by the API server.
	// 记录真正执行pod与apiserver第一次看到该pod的时间间隔
	// api server第一次见到pod，会给它打一个ConfigFirstSeenAnnotationKey这个tag
	var firstSeenTime time.Time
	if firstSeenTimeStr, ok := pod.Annotations[kubetypes.ConfigFirstSeenAnnotationKey]; ok {
		firstSeenTime = kubetypes.ConvertToTimestamp(firstSeenTimeStr).Get()
	}

	// Record pod worker start latency if being created
	// TODO: make pod workers record their own latencies
	// 如果pod是被创建的，则记录pod worker的启动时延
	if updateType == kubetypes.SyncPodCreate {
		if !firstSeenTime.IsZero() {
			// This is the first time we are syncing the pod. Record the latency
			// since kubelet first saw the pod if firstSeenTime is set.
			// 记录kubelet第一次同步pod的时间
			metrics.PodWorkerStartLatency.Observe(metrics.SinceInMicroseconds(firstSeenTime))
		} else {
			glog.V(3).Infof("First seen time not recorded for pod %q", pod.UID)
		}
	}

	// Generate final API pod status with pod and status manager status
	// 根据pod manager以及status manager产生最终的API pod
	// 对于Create来说，podStatus只有Pod ID这一个有效字段
	apiPodStatus := kl.generateAPIPodStatus(pod, podStatus)
	// The pod IP may be changed in generateAPIPodStatus if the pod is using host network. (See #24576)
	// TODO(random-liu): After writing pod spec into container labels, check whether pod is using host network, and
	// set pod IP to hostIP directly in runtime.GetPodStatus
	// 如果pod使用host network，pod IP在generateAPIPodStatus可能会改变
	podStatus.IP = apiPodStatus.PodIP

	// Record the time it takes for the pod to become running.
	// 记录pod变为running的时间间隔
	existingStatus, ok := kl.statusManager.GetPodStatus(pod.UID)
	if !ok || existingStatus.Phase == v1.PodPending && apiPodStatus.Phase == v1.PodRunning &&
		!firstSeenTime.IsZero() {
		metrics.PodStartLatency.Observe(metrics.SinceInMicroseconds(firstSeenTime))
	}

	runnable := kl.canRunPod(pod)
	if !runnable.Admit {
		// Pod不能执行
		// Pod is not runnable; update the Pod and Container statuses to why.
		apiPodStatus.Reason = runnable.Reason
		apiPodStatus.Message = runnable.Message
		// Waiting containers are not creating.
		// 将正在等待的容器都设置为"Blocked"
		const waitingReason = "Blocked"
		// 设置各个container的apiPodStatus的waiting reason
		for _, cs := range apiPodStatus.InitContainerStatuses {
			if cs.State.Waiting != nil {
				cs.State.Waiting.Reason = waitingReason
			}
		}
		for _, cs := range apiPodStatus.ContainerStatuses {
			if cs.State.Waiting != nil {
				cs.State.Waiting.Reason = waitingReason
			}
		}
	}

	// Update status in the status manager
	// 更新status manager中的状态
	kl.statusManager.SetPodStatus(pod, apiPodStatus)

	// Kill pod if it should not be running
	// 如果pod不能运行，则kill pod
	if !runnable.Admit || pod.DeletionTimestamp != nil || apiPodStatus.Phase == v1.PodFailed {
		var syncErr error
		if err := kl.killPod(pod, nil, podStatus, nil); err != nil {
			kl.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedToKillPod, "error killing pod: %v", err)
			syncErr = fmt.Errorf("error killing pod: %v", err)
			utilruntime.HandleError(syncErr)
		} else {
			if !runnable.Admit {
				// There was no error killing the pod, but the pod cannot be run.
				// Return an error to signal that the sync loop should back off.
				// 在kill pod的过程中没有发生error，但是pod就是不能被运行
				// 返回一个error，告诉sync loop需要回退
				syncErr = fmt.Errorf("pod cannot be run: %s", runnable.Message)
			}
		}
		return syncErr
	}

	// If the network plugin is not ready, only start the pod if it uses the host network
	// 如果network plugin没有准备好，则只启动使用host network的pod
	if rs := kl.runtimeState.networkErrors(); len(rs) != 0 && !kubecontainer.IsHostNetworkPod(pod) {
		kl.recorder.Eventf(pod, v1.EventTypeWarning, events.NetworkNotReady, "network is not ready: %v", rs)
		return fmt.Errorf("network is not ready: %v", rs)
	}

	// Create Cgroups for the pod and apply resource parameters
	// to them if cgroups-per-qos flag is enabled.
	// 如果使能了cgroups-per-qos，则为每个pod创建cgroups并且添加resouce parameter
	// 否则设置为podContainerManagerNoop, 每次调用都做空操作
	pcm := kl.containerManager.NewPodContainerManager()
	// If pod has already been terminated then we need not create
	// or update the pod's cgroup
	// 如果pod已经被terminated，那么我们就不需要创建或更新pod的cgroup
	if !kl.podIsTerminated(pod) {
		// When the kubelet is restarted with the cgroups-per-qos
		// flag enabled, all the pod's running containers
		// should be killed intermittently and brought back up
		// under the qos cgroup hierarchy.
		// Check if this is the pod's first sync
		// 当kubelet被重启，且cgroups-per-qos使能的话，所有pod正在运行的
		// 容器都应该被kill并且在qos cgroup hierarchy下被重启
		firstSync := true
		for _, containerStatus := range apiPodStatus.ContainerStatuses {
			if containerStatus.State.Running != nil {
				firstSync = false
				break
			}
		}
		// Don't kill containers in pod if pod's cgroups already
		// exists or the pod is running for the first time
		// 不要杀死pod中container，如果pod的cgroups已经存在了
		// 或者pod是第一次运行
		podKilled := false
		if !pcm.Exists(pod) && !firstSync {
			// 如果pod的cgroup不存在并且pod并不是第一次运行
			if err := kl.killPod(pod, nil, podStatus, nil); err == nil {
				podKilled = true
			}
		}
		// Create and Update pod's Cgroups
		// Don't create cgroups for run once pod if it was killed above
		// The current policy is not to restart the run once pods when
		// the kubelet is restarted with the new flag as run once pods are
		// expected to run only once and if the kubelet is restarted then
		// they are not expected to run again.
		// 对于run once的pod，如果在上面被kill掉的话，就不要重启了
		// We don't create and apply updates to cgroup if its a run once pod and was killed above
		// 我们不再创建或者更新cgroup，如果pod只运行一次，或者在上面被kill了
		if !(podKilled && pod.Spec.RestartPolicy == v1.RestartPolicyNever) {
			if !pcm.Exists(pod) {
				// 如果pod不在PodContainerManager中，更新qos cgroups
				if err := kl.containerManager.UpdateQOSCgroups(); err != nil {
					glog.V(2).Infof("Failed to update QoS cgroups while syncing pod: %v", err)
				}
				// 确保pod在PodContainerManager中
				if err := pcm.EnsureExists(pod); err != nil {
					kl.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedToCreatePodContainer, "unable to ensure pod container exists: %v", err)
					return fmt.Errorf("failed to ensure that the pod: %v cgroups exist and are correctly applied: %v", pod.UID, err)
				}
			}
		}
	}

	// Create Mirror Pod for Static Pod if it doesn't already exist
	// 为static pod创建mirror pod，如果它不存在的话
	// source不为api server的即为static pod
	if kubepod.IsStaticPod(pod) {
		podFullName := kubecontainer.GetPodFullName(pod)
		deleted := false
		// 如果mirrorPod已经存在了，检测其是否合法
		if mirrorPod != nil {
			if mirrorPod.DeletionTimestamp != nil || !kl.podManager.IsMirrorPodOf(mirrorPod, pod) {
				// The mirror pod is semantically different from the static pod. Remove
				// it. The mirror pod will get recreated later.
				// 如果mirror pod和static pod在语义上是不同的，则删除mirror pod，并重建一个
				glog.Warningf("Deleting mirror pod %q because it is outdated", format.Pod(mirrorPod))
				if err := kl.podManager.DeleteMirrorPod(podFullName); err != nil {
					glog.Errorf("Failed deleting mirror pod %q: %v", format.Pod(mirrorPod), err)
				} else {
					deleted = true
				}
			}
		}
		// 如果不存在mirrorPod或者已经被删除了
		if mirrorPod == nil || deleted {
			node, err := kl.GetNode()
			if err != nil || node.DeletionTimestamp != nil {
				// node已经从集群中移除了
				glog.V(4).Infof("No need to create a mirror pod, since node %q has been removed from the cluster", kl.nodeName)
			} else {
				glog.V(4).Infof("Creating a mirror pod for static pod %q", format.Pod(pod))
				// 为static pod创建mirror pod，其实就是对pod进行复制，并添加一个annotation，说明是
				// mirror pod，再调用apiserver的client进行创建
				if err := kl.podManager.CreateMirrorPod(pod); err != nil {
					glog.Errorf("Failed creating a mirror pod for %q: %v", format.Pod(pod), err)
				}
			}
		}
	}

	// Make data directories for the pod
	// 创建pod的data directory
	if err := kl.makePodDataDirs(pod); err != nil {
		kl.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedToMakePodDataDirectories, "error making pod data directories: %v", err)
		glog.Errorf("Unable to make pod data directories for pod %q: %v", format.Pod(pod), err)
		return err
	}

	// Volume manager will not mount volumes for terminated pods
	// Volume manager不会为terminated pod进行mount volumes
	if !kl.podIsTerminated(pod) {
		// Wait for volumes to attach/mount
		// 等待volume的attach/mount
		if err := kl.volumeManager.WaitForAttachAndMount(pod); err != nil {
			kl.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedMountVolume, "Unable to mount volumes for pod %q: %v", format.Pod(pod), err)
			glog.Errorf("Unable to mount volumes for pod %q: %v; skipping pod", format.Pod(pod), err)
			return err
		}
	}

	// Fetch the pull secrets for the pod
	// 获取pod的pull secrets
	pullSecrets := kl.getPullSecretsForPod(pod)

	// Call the container runtime's SyncPod callback
	// 调用容器运行时的SyncPod callback
	result := kl.containerRuntime.SyncPod(pod, apiPodStatus, podStatus, pullSecrets, kl.backOff)
	kl.reasonCache.Update(pod.UID, result)
	if err := result.Error(); err != nil {
		// Do not record an event here, as we keep all event logging for sync pod failures
		// local to container runtime so we get better errors
		return err
	}

	return nil
}

// Get pods which should be resynchronized. Currently, the following pod should be resynchronized:
//   * pod whose work is ready.
//   * internal modules that request sync of a pod.
// 获取需要重新同步的pods，当前状态下，以下这些pod都需要重新同步的：
//	 * pod的工作已经ready
//	 * 内部模块需要对这些pod进行同步
func (kl *Kubelet) getPodsToSync() []*v1.Pod {
	allPods := kl.podManager.GetPods()
	podUIDs := kl.workQueue.GetWork()
	podUIDSet := sets.NewString()
	for _, podUID := range podUIDs {
		podUIDSet.Insert(string(podUID))
	}
	var podsToSync []*v1.Pod
	for _, pod := range allPods {
		if podUIDSet.Has(string(pod.UID)) {
			// The work of the pod is ready
			// 如果pod已经处于work queue中，则加入podsToSync
			podsToSync = append(podsToSync, pod)
			continue
		}
		for _, podSyncLoopHandler := range kl.PodSyncLoopHandlers {
			// 否则调用podSyncLoopHandler的ShouldSync函数进行检测
			// 如果需要同步，也加入podsToSync中
			if podSyncLoopHandler.ShouldSync(pod) {
				podsToSync = append(podsToSync, pod)
				break
			}
		}
	}
	return podsToSync
}

// deletePod deletes the pod from the internal state of the kubelet by:
// 1.  stopping the associated pod worker asynchronously
// 2.  signaling to kill the pod by sending on the podKillingCh channel
// deletePod从kubelet的internal state中删除pod，通过：
// 1.  异步地停止相关的pod worker
// 2.  通过给podKillingCh发消息来kill pod
//
// deletePod returns an error if not all sources are ready or the pod is not
// found in the runtime cache.
// deletePod返回error，如果不是所有的sources都处于ready，或者pod没有在runtime cache中找到
func (kl *Kubelet) deletePod(pod *v1.Pod) error {
	if pod == nil {
		return fmt.Errorf("deletePod does not allow nil pod")
	}
	if !kl.sourcesReady.AllReady() {
		// If the sources aren't ready, skip deletion, as we may accidentally delete pods
		// for sources that haven't reported yet.
		// 如果sources没有准备好，跳过deletion，因为我们可能会意外删除那些还没有report的sources
		return fmt.Errorf("skipping delete because sources aren't ready yet")
	}
	// 将对应的podWorker移除
	kl.podWorkers.ForgetWorker(pod.UID)

	// Runtime cache may not have been updated to with the pod, but it's okay
	// because the periodic cleanup routine will attempt to delete again later.
	// Runtime cache可能不会和pod同步更新，但是这没有问题
	// 因为阶段性的cleanup routine会在以后再次尝试删除
	runningPods, err := kl.runtimeCache.GetPods()
	if err != nil {
		return fmt.Errorf("error listing containers: %v", err)
	}
	runningPod := kubecontainer.Pods(runningPods).FindPod("", pod.UID)
	if runningPod.IsEmpty() {
		return fmt.Errorf("pod not found")
	}
	podPair := kubecontainer.PodPair{APIPod: pod, RunningPod: &runningPod}

	// 将podPair发送给kubelet的podKillingCh
	kl.podKillingCh <- &podPair
	// TODO: delete the mirror pod here?

	// We leave the volume/directory cleanup to the periodic cleanup routine.
	// 我们将volume/directory的cleanup放到阶段性的cleanup routine中去做
	return nil
}

// rejectPod records an event about the pod with the given reason and message,
// and updates the pod to the failed phase in the status manage.
// rejectPod会用给定的reason和message，记录一个pod有关的event
// 并且更新status manager中的pod到failed状态
func (kl *Kubelet) rejectPod(pod *v1.Pod, reason, message string) {
	kl.recorder.Eventf(pod, v1.EventTypeWarning, reason, message)
	kl.statusManager.SetPodStatus(pod, v1.PodStatus{
		Phase:   v1.PodFailed,
		Reason:  reason,
		Message: "Pod " + message})
}

// canAdmitPod determines if a pod can be admitted, and gives a reason if it
// cannot. "pod" is new pod, while "pods" are all admitted pods
// The function returns a boolean value indicating whether the pod
// can be admitted, a brief single-word reason and a message explaining why
// the pod cannot be admitted.
// canAdmitPod确定一个pod是否能被admitted，并且给出一个理由，说明为什么不能
// "pod"是一个新的pod，但是"pods"是所有能被admitted的pods
// 本函数返回一个boolean表明pod是否能被admitted，一个简答的single-word的理由
// 以及一个message说明pod为什么不能被admitted
func (kl *Kubelet) canAdmitPod(pods []*v1.Pod, pod *v1.Pod) (bool, string, string) {
	// the kubelet will invoke each pod admit handler in sequence
	// if any handler rejects, the pod is rejected.
	// kubelet会依次调用每个pod admit handler
	// 如果有任意一个handler拒绝，pod就会拒绝
	// TODO: move out of disk check into a pod admitter
	// TODO: out of resource eviction should have a pod admitter call-out
	attrs := &lifecycle.PodAdmitAttributes{Pod: pod, OtherPods: pods}
	for _, podAdmitHandler := range kl.admitHandlers {
		if result := podAdmitHandler.Admit(attrs); !result.Admit {
			return false, result.Reason, result.Message
		}
	}

	return true, "", ""
}

func (kl *Kubelet) canRunPod(pod *v1.Pod) lifecycle.PodAdmitResult {
	attrs := &lifecycle.PodAdmitAttributes{Pod: pod}
	// Get "OtherPods". Rejected pods are failed, so only include admitted pods that are alive.
	// 只能包括那些还活着的admitted pod
	attrs.OtherPods = kl.filterOutTerminatedPods(kl.podManager.GetPods())

	// 遍历soft admit handlers
	for _, handler := range kl.softAdmitHandlers {
		if result := handler.Admit(attrs); !result.Admit {
			return result
		}
	}

	// TODO: Refactor as a soft admit handler.
	// 检测我们是否有capability来运行pod
	if err := canRunPod(pod); err != nil {
		return lifecycle.PodAdmitResult{
			Admit:   false,
			Reason:  "Forbidden",
			Message: err.Error(),
		}
	}

	return lifecycle.PodAdmitResult{Admit: true}
}

// syncLoop is the main loop for processing changes. It watches for changes from
// three channels (file, apiserver, and http) and creates a union of them. For
// any new change seen, will run a sync against desired state and running state. If
// no changes are seen to the configuration, will synchronize the last known desired
// state every sync-frequency seconds. Never returns.
// syncLoop是处理变化的主循环，它从三个channel中watch变化（file, apiserver以及http）并且创建一个
// 它们的union。对于发现的任何新变化，它都会在desired state和running state之间执行一次同步
// 如果配置没有发生什么变化，会每隔sync-frequency秒同步一次最后一次知道的desired state
// 从不返回
func (kl *Kubelet) syncLoop(updates <-chan kubetypes.PodUpdate, handler SyncHandler) {
	glog.Info("Starting kubelet main sync loop.")
	// The resyncTicker wakes up kubelet to checks if there are any pod workers
	// that need to be sync'd. A one-second period is sufficient because the
	// sync interval is defaulted to 10s.
	// resyncTicker会唤醒kubelet去检查是否有pod worker需要同步
	// 一秒钟的间隔已经足够了因为sync interval默认为10s
	syncTicker := time.NewTicker(time.Second)
	defer syncTicker.Stop()
	// housekeepingPeriod为2秒
	housekeepingTicker := time.NewTicker(housekeepingPeriod)
	defer housekeepingTicker.Stop()
	plegCh := kl.pleg.Watch()
	for {
		// 遇到错误则加入runtimeState.runtimeErrors中，每次遇到错误，休眠5秒
		if rs := kl.runtimeState.runtimeErrors(); len(rs) != 0 {
			glog.Infof("skipping pod synchronization - %v", rs)
			time.Sleep(5 * time.Second)
			continue
		}

		// 记录一次loop启动的时间以及结束的时间
		kl.syncLoopMonitor.Store(kl.clock.Now())
		if !kl.syncLoopIteration(updates, handler, syncTicker.C, housekeepingTicker.C, plegCh) {
			break
		}
		kl.syncLoopMonitor.Store(kl.clock.Now())
	}
}

// syncLoopIteration reads from various channels and dispatches pods to the
// given handler.
// syncLoopIteration从各个channel读取事件并且分发pods到给定的handler
//
// Arguments:
// 读取config event的channel
// 1.  configCh:       a channel to read config events from
// 可以向handler中分发pods，它会进行处理
// 2.  handler:        the SyncHandler to dispatch pods to
// 可以从syncCh中读取阶段性的sync events
// 3.  syncCh:         a channel to read periodic sync events from
// 可以从houseKeepingCh中读取housekeeping events
// 4.  houseKeepingCh: a channel to read housekeeping events from
// 从plegCh中读取PLEG的更新
// 5.  plegCh:         a channel to read PLEG updates from
//
// Events are also read from the kubelet liveness manager's update channel.
// 事件也从kubelet的liveness manager的update channel中获取
//
// The workflow is to read from one of the channels, handle that event, and
// update the timestamp in the sync loop monitor.
// 工作流程为从其中一个channel读取事件并处理，并且更新sync loop monitor中的时间戳
//
// Here is an appropriate place to note that despite the syntactical
// similarity to the switch statement, the case statements in a select are
// evaluated in a pseudorandom order if there are multiple channels ready to
// read from when the select is evaluated.  In other words, case statements
// are evaluated in random order, and you can not assume that the case
// statements evaluate in order if multiple channels have events.
// select中的case语句的评估都是随机的，当有多个channels有事件发生时，我们不能假设这些case语句是按序评估的
//
// With that in mind, in truly no particular order, the different channels
// are handled as follows:
//
// * configCh: dispatch the pods for the config change to the appropriate
//             handler callback for the event type
// * configCh: 将config change的pods根据事件类型分发到给定的handler回调函数
// * plegCh: update the runtime cache; sync pod
// * plegCh: 更新runtime cache
// * syncCh: sync all pods waiting for sync
// * syncCh: 同步所有等待同步的pods
// * houseKeepingCh: trigger cleanup of pods
// houseKeepingCh: 触发pod的清理工作
// * liveness manager: sync pods that have failed or in which one or more
//                     containers have failed liveness checks
// liveness manager: 同步那些经过liveness检测，已经fail的或者其中的一个或多个container已经fail的pod
func (kl *Kubelet) syncLoopIteration(configCh <-chan kubetypes.PodUpdate, handler SyncHandler,
	syncCh <-chan time.Time, housekeepingCh <-chan time.Time, plegCh <-chan *pleg.PodLifecycleEvent) bool {
	select {
	case u, open := <-configCh:
		// Update from a config source; dispatch it to the right handler
		// callback.
		// 从config source读取更新，并将它分发到正确的回调函数中
		if !open {
			// 当update channel关闭时，退出sync loop
			glog.Errorf("Update channel is closed. Exiting the sync loop.")
			return false
		}

		switch u.Op {
		case kubetypes.ADD:
			// Pods函数返回一个字符串，以人类可读的形式表示一系列的pods
			glog.V(2).Infof("SyncLoop (ADD, %q): %q", u.Source, format.Pods(u.Pods))
			// After restarting, kubelet will get all existing pods through
			// ADD as if they are new pods. These pods will then go through the
			// admission process and *may* be rejected. This can be resolved
			// once we have checkpointing.
			// 在重启之后，kubelet会把所有还存在的pod加入ADD，仿佛它们是新的pod一样
			// 这些pod会经过admission process并且可能被拒绝
			// 这在我们有了checkpointing之后可以被处理
			handler.HandlePodAdditions(u.Pods)
		case kubetypes.UPDATE:
			glog.V(2).Infof("SyncLoop (UPDATE, %q): %q", u.Source, format.PodsWithDeletiontimestamps(u.Pods))
			handler.HandlePodUpdates(u.Pods)
		case kubetypes.REMOVE:
			glog.V(2).Infof("SyncLoop (REMOVE, %q): %q", u.Source, format.Pods(u.Pods))
			handler.HandlePodRemoves(u.Pods)
		case kubetypes.RECONCILE:
			glog.V(4).Infof("SyncLoop (RECONCILE, %q): %q", u.Source, format.Pods(u.Pods))
			handler.HandlePodReconcile(u.Pods)
		case kubetypes.DELETE:
			glog.V(2).Infof("SyncLoop (DELETE, %q): %q", u.Source, format.Pods(u.Pods))
			// DELETE is treated as a UPDATE because of graceful deletion.
			// 因为graceful deletion，将DELETE作为UPDATE
			handler.HandlePodUpdates(u.Pods)
		case kubetypes.RESTORE:
			glog.V(2).Infof("SyncLoop (RESTORE, %q): %q", u.Source, format.Pods(u.Pods))
			// These are pods restored from the checkpoint. Treat them as new
			// pods.
			// 这些pod都是从checkpoint中恢复的，把它们当成新的pod
			handler.HandlePodAdditions(u.Pods)
		case kubetypes.SET:
			// TODO: Do we want to support this?
			glog.Errorf("Kubelet does not support snapshot update")
		}

		if u.Op != kubetypes.RESTORE {
			// If the update type is RESTORE, it means that the update is from
			// the pod checkpoints and may be incomplete. Do not mark the
			// source as ready.
			// 如果update的类型为RESTORE，这意味着update来源于pod checkpoints，因此可能
			// 并不完整，所以不能将source标记为ready

			// Mark the source ready after receiving at least one update from the
			// source. Once all the sources are marked ready, various cleanup
			// routines will start reclaiming resources. It is important that this
			// takes place only after kubelet calls the update handler to process
			// the update to ensure the internal pod cache is up-to-date.
			// 当从source中至少获取一个update之后，将source标记为ready
			// 一旦所有source被标记为ready，各个cleanup routines会开始回收资源
			// 这必须在kubelet调用了update handler去处理update，保证internal cache是最新的之后
			// 进行
			kl.sourcesReady.AddSource(u.Source)
		}
	case e := <-plegCh:
		// 除了ContainerRemoved这一事件之外，其他都是值得的
		// 因为ContainerRemoved不会影响pod的状态
		if isSyncPodWorthy(e) {
			// PLEG event for a pod; sync it.
			// 一个pod的PLEG event；对它进行同步
			if pod, ok := kl.podManager.GetPodByUID(e.ID); ok {
				glog.V(2).Infof("SyncLoop (PLEG): %q, event: %#v", format.Pod(pod), e)
				handler.HandlePodSyncs([]*v1.Pod{pod})
			} else {
				// If the pod no longer exists, ignore the event.
				glog.V(4).Infof("SyncLoop (PLEG): ignore irrelevant event: %#v", e)
			}
		}

		if e.Type == pleg.ContainerDied {
			// 如果PLEG的类型为ContainerDied，则获取container id并删除该container
			if containerID, ok := e.Data.(string); ok {
				kl.cleanUpContainersInPod(e.ID, containerID)
			}
		}
	case <-syncCh:
		// Sync pods waiting for sync
		// 获取等待同步的pods
		podsToSync := kl.getPodsToSync()
		if len(podsToSync) == 0 {
			break
		}
		glog.V(4).Infof("SyncLoop (SYNC): %d pods; %s", len(podsToSync), format.Pods(podsToSync))
		handler.HandlePodSyncs(podsToSync)
	case update := <-kl.livenessManager.Updates():
		if update.Result == proberesults.Failure {
			// The liveness manager detected a failure; sync the pod.
			// liveness manager检测到一个failure，对pod进行同步

			// We should not use the pod from livenessManager, because it is never updated after
			// initialization.
			// 我们不应该使用来自livenessManager的pod，因为它在初始化之后就不再更新了
			// 从pod manager中获取最新的pod
			pod, ok := kl.podManager.GetPodByUID(update.PodUID)
			if !ok {
				// If the pod no longer exists, ignore the update.
				// 如果pod不存在了，忽略更新
				glog.V(4).Infof("SyncLoop (container unhealthy): ignore irrelevant update: %#v", update)
				break
			}
			glog.V(1).Infof("SyncLoop (container unhealthy): %q", format.Pod(pod))
			handler.HandlePodSyncs([]*v1.Pod{pod})
		}
	case <-housekeepingCh:
		if !kl.sourcesReady.AllReady() {
			// If the sources aren't ready or volume manager has not yet synced the states,
			// skip housekeeping, as we may accidentally delete pods from unready sources.
			// 如果sources没有处于ready状态，或者volume manager还没有对states进行同步
			// 则跳过housekeeping，因为我们可能会以外删除来自unready sources的pod
			glog.V(4).Infof("SyncLoop (housekeeping, skipped): sources aren't ready yet.")
		} else {
			glog.V(4).Infof("SyncLoop (housekeeping)")
			// 定期进行housekeeping，在所有source都处于ready的情况下
			if err := handler.HandlePodCleanups(); err != nil {
				glog.Errorf("Failed cleaning pods: %v", err)
			}
		}
	}
	return true
}

// dispatchWork starts the asynchronous sync of the pod in a pod worker.
// If the pod is terminated, dispatchWork
// dispatchWork在一个pod worker中对一个pod进行异步地同步
// 如果pod被终止了，dispatchWork直接返回
func (kl *Kubelet) dispatchWork(pod *v1.Pod, syncType kubetypes.SyncPodType, mirrorPod *v1.Pod, start time.Time) {
	if kl.podIsTerminated(pod) {
		// 如果pod已经处于Terminated状态，则无需再对该pod进行同步
		if pod.DeletionTimestamp != nil {
			// If the pod is in a terminated state, there is no pod worker to
			// handle the work item. Check if the DeletionTimestamp has been
			// set, and force a status update to trigger a pod deletion request
			// to the apiserver.
			// 如果pod已经处于terminated状态，不会有pod worker去处理它
			// 检测DeletionTimestamp是否设置，并且强制进status update从而触发一个
			// pod deletion请求到apiserver
			kl.statusManager.TerminatePod(pod)
		}
		return
	}
	// Run the sync in an async worker.
	// 在一个异步的worker进行同步
	kl.podWorkers.UpdatePod(&UpdatePodOptions{
		Pod:        pod,
		MirrorPod:  mirrorPod,
		UpdateType: syncType,
		OnCompleteFunc: func(err error) {
			if err != nil {
				// Pod worker的延时？
				metrics.PodWorkerLatency.WithLabelValues(syncType.String()).Observe(metrics.SinceInMicroseconds(start))
			}
		},
	})
	// Note the number of containers for new pods.
	if syncType == kubetypes.SyncPodCreate {
		// 如果是创建新pod，统计pod中的容器数目
		metrics.ContainersPerPodCount.Observe(float64(len(pod.Spec.Containers)))
	}
}

// TODO: handle mirror pods in a separate component (issue #17251)
func (kl *Kubelet) handleMirrorPod(mirrorPod *v1.Pod, start time.Time) {
	// Mirror pod ADD/UPDATE/DELETE operations are considered an UPDATE to the
	// corresponding static pod. Send update to the pod worker if the static
	// pod exists.
	// Mirror pod的ADD/UPDATE/DELETE操作都被认为是对应的static pod的UPDATE操作
	// 如果static pod存在的话，给相应的pod worker发送一个update
	if pod, ok := kl.podManager.GetPodByMirrorPod(mirrorPod); ok {
		kl.dispatchWork(pod, kubetypes.SyncPodUpdate, mirrorPod, start)
	}
}

// HandlePodAdditions is the callback in SyncHandler for pods being added from
// a config source.
func (kl *Kubelet) HandlePodAdditions(pods []*v1.Pod) {
	// 记录初始时间
	start := kl.clock.Now()
	// 将pod按创建的时间进行排序
	sort.Sort(sliceutils.PodsByCreationTime(pods))
	for _, pod := range pods {
		existingPods := kl.podManager.GetPods()
		// Always add the pod to the pod manager. Kubelet relies on the pod
		// manager as the source of truth for the desired state. If a pod does
		// not exist in the pod manager, it means that it has been deleted in
		// the apiserver and no action (other than cleanup) is required.
		// 总是先将pod加入pod manager，kubelet将pod manager作为目标状态的source of truth
		// 如果pod不存在在pod manager中，这意味着它已经从apiserver中删除了并且不会再需要任何行为（除了cleanup）
		kl.podManager.AddPod(pod)

		// 对mirror pod进行处理
		if kubepod.IsMirrorPod(pod) {
			kl.handleMirrorPod(pod, start)
			continue
		}

		if !kl.podIsTerminated(pod) {
			// Only go through the admission process if the pod is not
			// terminated.
			// 只有没有terminated的pod才会经过admission process

			// We failed pods that we rejected, so activePods include all admitted
			// pods that are alive.
			// 我们fail了所有我们拒绝的pods，所以activePods中包含了所有我们允许的并且还存活着的
			// pods
			activePods := kl.filterOutTerminatedPods(existingPods)

			// Check if we can admit the pod; if not, reject it.
			// 检测我们是否能接受pod,否则删除它
			if ok, reason, message := kl.canAdmitPod(activePods, pod); !ok {
				kl.rejectPod(pod, reason, message)
				continue
			}
		}
		// 通过pod的full name找到mirror pod，当开始创建pod时，mirror pod为空
		mirrorPod, _ := kl.podManager.GetMirrorPodByPod(pod)
		// 分发pod，进行具体的工作, 类型为SyncPodCreate
		kl.dispatchWork(pod, kubetypes.SyncPodCreate, mirrorPod, start)
		// 加入probeManager对pod进行检测
		kl.probeManager.AddPod(pod)
	}
}

// HandlePodUpdates is the callback in the SyncHandler interface for pods
// being updated from a config source.
// HandlePodUpdates是SyncHandler接口的一个回调函数，用于处理来自config source的更新
func (kl *Kubelet) HandlePodUpdates(pods []*v1.Pod) {
	start := kl.clock.Now()
	for _, pod := range pods {
		// 调用podManager进行update pod
		kl.podManager.UpdatePod(pod)
		if kubepod.IsMirrorPod(pod) {
			kl.handleMirrorPod(pod, start)
			continue
		}
		// TODO: Evaluate if we need to validate and reject updates.

		mirrorPod, _ := kl.podManager.GetMirrorPodByPod(pod)
		kl.dispatchWork(pod, kubetypes.SyncPodUpdate, mirrorPod, start)
	}
}

// HandlePodRemoves is the callback in the SyncHandler interface for pods
// being removed from a config source.
func (kl *Kubelet) HandlePodRemoves(pods []*v1.Pod) {
	start := kl.clock.Now()
	for _, pod := range pods {
		// 删除podManager中的pod
		kl.podManager.DeletePod(pod)
		if kubepod.IsMirrorPod(pod) {
			kl.handleMirrorPod(pod, start)
			continue
		}
		// Deletion is allowed to fail because the periodic cleanup routine
		// will trigger deletion again.
		// Deletion是允许失败的，因为阶段性的cleanup routine会重新触发删除
		if err := kl.deletePod(pod); err != nil {
			glog.V(2).Infof("Failed to delete pod %q, err: %v", format.Pod(pod), err)
		}
		// 从probeManager中移除pod
		kl.probeManager.RemovePod(pod)
	}
}

// HandlePodReconcile is the callback in the SyncHandler interface for pods
// that should be reconciled.
func (kl *Kubelet) HandlePodReconcile(pods []*v1.Pod) {
	for _, pod := range pods {
		// Update the pod in pod manager, status manager will do periodically reconcile according
		// to the pod manager.
		// 更新pod manager中的pod，status manager会定期地根据pod manager进行reconcile
		kl.podManager.UpdatePod(pod)

		// After an evicted pod is synced, all dead containers in the pod can be removed.
		// 在一个evicted pod已经被同步之后，pod中所有的dead containers都会被移除
		if eviction.PodIsEvicted(pod.Status) {
			if podStatus, err := kl.podCache.Get(pod.UID); err == nil {
				kl.containerDeletor.deleteContainersInPod("", podStatus, true)
			}
		}
	}
}

// HandlePodSyncs is the callback in the syncHandler interface for pods
// that should be dispatched to pod workers for sync.
func (kl *Kubelet) HandlePodSyncs(pods []*v1.Pod) {
	start := kl.clock.Now()
	for _, pod := range pods {
		mirrorPod, _ := kl.podManager.GetMirrorPodByPod(pod)
		// 遍历需要同步的pods，并且调用dispatchWork分发到各个worker中
		kl.dispatchWork(pod, kubetypes.SyncPodSync, mirrorPod, start)
	}
}

// LatestLoopEntryTime returns the last time in the sync loop monitor.
func (kl *Kubelet) LatestLoopEntryTime() time.Time {
	val := kl.syncLoopMonitor.Load()
	if val == nil {
		return time.Time{}
	}
	return val.(time.Time)
}

// updateRuntimeUp calls the container runtime status callback, initializing
// the runtime dependent modules when the container runtime first comes up,
// and returns an error if the status check fails.  If the status check is OK,
// update the container runtime uptime in the kubelet runtimeState.
// updateRuntimeUp调用容器的runtime status回调函数，在容器运行时第一次启动时
// 初始化运行时相关的模块，并且返回错误，如果status检查失败的话
// 如果status检查成功，则更新kubelet runtimeState的容器运行时的uptime
func (kl *Kubelet) updateRuntimeUp() {
	s, err := kl.containerRuntime.Status()
	if err != nil {
		glog.Errorf("Container runtime sanity check failed: %v", err)
		return
	}
	// rkt uses the legacy, non-CRI integration. Don't check the runtime
	// conditions for it.
	if kl.containerRuntimeName != kubetypes.RktContainerRuntime {
		if s == nil {
			glog.Errorf("Container runtime status is nil")
			return
		}
		// Periodically log the whole runtime status for debugging.
		// TODO(random-liu): Consider to send node event when optional
		// condition is unmet.
		glog.V(4).Infof("Container runtime status: %v", s)
		networkReady := s.GetRuntimeCondition(kubecontainer.NetworkReady)
		if networkReady == nil || !networkReady.Status {
			glog.Errorf("Container runtime network not ready: %v", networkReady)
			kl.runtimeState.setNetworkState(fmt.Errorf("runtime network not ready: %v", networkReady))
		} else {
			// Set nil if the container runtime network is ready.
			kl.runtimeState.setNetworkState(nil)
		}
		// TODO(random-liu): Add runtime error in runtimeState, and update it
		// when runtime is not ready, so that the information in RuntimeReady
		// condition will be propagated to NodeReady condition.
		runtimeReady := s.GetRuntimeCondition(kubecontainer.RuntimeReady)
		// If RuntimeReady is not set or is false, report an error.
		if runtimeReady == nil || !runtimeReady.Status {
			glog.Errorf("Container runtime not ready: %v", runtimeReady)
			return
		}
	}
	kl.oneTimeInitializer.Do(kl.initializeRuntimeDependentModules)
	kl.runtimeState.setRuntimeSync(kl.clock.Now())
}

// updateCloudProviderFromMachineInfo updates the node's provider ID field
// from the given cadvisor machine info.
func (kl *Kubelet) updateCloudProviderFromMachineInfo(node *v1.Node, info *cadvisorapi.MachineInfo) {
	if info.CloudProvider != cadvisorapi.UnknownProvider &&
		info.CloudProvider != cadvisorapi.Baremetal {
		// The cloud providers from pkg/cloudprovider/providers/* that update ProviderID
		// will use the format of cloudprovider://project/availability_zone/instance_name
		// here we only have the cloudprovider and the instance name so we leave project
		// and availability zone empty for compatibility.
		node.Spec.ProviderID = strings.ToLower(string(info.CloudProvider)) +
			":////" + string(info.InstanceID)
	}
}

// GetConfiguration returns the KubeletConfiguration used to configure the kubelet.
func (kl *Kubelet) GetConfiguration() kubeletconfiginternal.KubeletConfiguration {
	return kl.kubeletConfiguration
}

// BirthCry sends an event that the kubelet has started up.
// BirthCry发送一个event，告知kubelet已经启动了
func (kl *Kubelet) BirthCry() {
	// Make an event that kubelet restarted.
	// 发送StartingKubelet事件
	kl.recorder.Eventf(kl.nodeRef, v1.EventTypeNormal, events.StartingKubelet, "Starting kubelet.")
}

// StreamingConnectionIdleTimeout returns the timeout for streaming connections to the HTTP server.
func (kl *Kubelet) StreamingConnectionIdleTimeout() time.Duration {
	return kl.streamingConnectionIdleTimeout
}

// ResyncInterval returns the interval used for periodic syncs.
func (kl *Kubelet) ResyncInterval() time.Duration {
	return kl.resyncInterval
}

// ListenAndServe runs the kubelet HTTP server.
// ListenAndServe运行一个kubelet HTTP server
func (kl *Kubelet) ListenAndServe(address net.IP, port uint, tlsOptions *server.TLSOptions, auth server.AuthInterface, enableDebuggingHandlers, enableContentionProfiling bool) {
	// host参数其实就是kubelet
	server.ListenAndServeKubeletServer(kl, kl.resourceAnalyzer, address, port, tlsOptions, auth, enableDebuggingHandlers, enableContentionProfiling, kl.containerRuntime, kl.criHandler)
}

// ListenAndServeReadOnly runs the kubelet HTTP server in read-only mode.
func (kl *Kubelet) ListenAndServeReadOnly(address net.IP, port uint) {
	server.ListenAndServeKubeletReadOnlyServer(kl, kl.resourceAnalyzer, address, port, kl.containerRuntime)
}

// Delete the eligible dead container instances in a pod. Depending on the configuration, the latest dead containers may be kept around.
// 删除pod中合格的dead container。根据配置，最后一个dead container应该要保留
func (kl *Kubelet) cleanUpContainersInPod(podID types.UID, exitedContainerID string) {
	if podStatus, err := kl.podCache.Get(podID); err == nil {
		removeAll := false
		if syncedPod, ok := kl.podManager.GetPodByUID(podID); ok {
			// generate the api status using the cached runtime status to get up-to-date ContainerStatuses
			apiPodStatus := kl.generateAPIPodStatus(syncedPod, podStatus)
			// When an evicted or deleted pod has already synced, all containers can be removed.
			// 如果evicted或者deleted pod已经同步了，则其中所有的容器都可以被移除
			removeAll = eviction.PodIsEvicted(syncedPod.Status) || (syncedPod.DeletionTimestamp != nil && notRunning(apiPodStatus.ContainerStatuses))
		}
		kl.containerDeletor.deleteContainersInPod(exitedContainerID, podStatus, removeAll)
	}
}

// isSyncPodWorthy filters out events that are not worthy of pod syncing
func isSyncPodWorthy(event *pleg.PodLifecycleEvent) bool {
	// ContatnerRemoved doesn't affect pod state
	return event.Type != pleg.ContainerRemoved
}

// Gets the streaming server configuration to use with in-process CRI shims.
func getStreamingConfig(kubeCfg *kubeletconfiginternal.KubeletConfiguration, kubeDeps *Dependencies) *streaming.Config {
	config := &streaming.Config{
		// Use a relative redirect (no scheme or host).
		BaseURL: &url.URL{
			Path: "/cri/",
		},
		StreamIdleTimeout:               kubeCfg.StreamingConnectionIdleTimeout.Duration,
		StreamCreationTimeout:           streaming.DefaultConfig.StreamCreationTimeout,
		SupportedRemoteCommandProtocols: streaming.DefaultConfig.SupportedRemoteCommandProtocols,
		SupportedPortForwardProtocols:   streaming.DefaultConfig.SupportedPortForwardProtocols,
	}
	if kubeDeps.TLSOptions != nil {
		config.TLSConfig = kubeDeps.TLSOptions.Config
	}
	return config
}
