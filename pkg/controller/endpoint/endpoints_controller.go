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

package endpoint

import (
	"fmt"
	"reflect"
	"strconv"
	"time"

	"k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubernetes/pkg/api/v1/endpoints"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/util/metrics"

	"github.com/golang/glog"
)

const (
	// maxRetries is the number of times a service will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the
	// sequence of delays between successive queuings of a service.
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	// maxRetries代表了一个service在从队列移除之前会进行重试的次数
	maxRetries = 15

	// An annotation on the Service denoting if the endpoints controller should
	// go ahead and create endpoints for unready pods. This annotation is
	// currently only used by StatefulSets, where we need the pod to be DNS
	// resolvable during initialization and termination. In this situation we
	// create a headless Service just for the StatefulSet, and clients shouldn't
	// be using this Service for anything so unready endpoints don't matter.
	// Endpoints of these Services retain their DNS records and continue
	// receiving traffic for the Service from the moment the kubelet starts all
	// containers in the pod and marks it "Running", till the kubelet stops all
	// containers and deletes the pod from the apiserver.
	// This field is deprecated. v1.Service.PublishNotReadyAddresses will replace it
	// subsequent releases.
	TolerateUnreadyEndpointsAnnotation = "service.alpha.kubernetes.io/tolerate-unready-endpoints"
)

var (
	keyFunc = cache.DeletionHandlingMetaNamespaceKeyFunc
)

// NewEndpointController returns a new *EndpointController.
func NewEndpointController(podInformer coreinformers.PodInformer, serviceInformer coreinformers.ServiceInformer,
	endpointsInformer coreinformers.EndpointsInformer, client clientset.Interface) *EndpointController {
	if client != nil && client.CoreV1().RESTClient().GetRateLimiter() != nil {
		metrics.RegisterMetricAndTrackRateLimiterUsage("endpoint_controller", client.CoreV1().RESTClient().GetRateLimiter())
	}
	e := &EndpointController{
		client:           client,
		queue:            workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "endpoint"),
		workerLoopPeriod: time.Second,
	}

	serviceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: e.enqueueService,
		UpdateFunc: func(old, cur interface{}) {
			e.enqueueService(cur)
		},
		DeleteFunc: e.enqueueService,
	})
	e.serviceLister = serviceInformer.Lister()
	e.servicesSynced = serviceInformer.Informer().HasSynced

	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    e.addPod,
		UpdateFunc: e.updatePod,
		DeleteFunc: e.deletePod,
	})
	e.podLister = podInformer.Lister()
	e.podsSynced = podInformer.Informer().HasSynced

	e.endpointsLister = endpointsInformer.Lister()
	e.endpointsSynced = endpointsInformer.Informer().HasSynced

	return e
}

// EndpointController manages selector-based service endpoints.
// EndpointController管理基于selector的service endpoints
type EndpointController struct {
	client clientset.Interface

	// serviceLister is able to list/get services and is populated by the shared informer passed to
	// NewEndpointController.
	// serviceLister用于list/get service，并且由传递给NewEndpointController的shared informer进行填充
	serviceLister corelisters.ServiceLister
	// servicesSynced returns true if the service shared informer has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	servicesSynced cache.InformerSynced

	// podLister is able to list/get pods and is populated by the shared informer passed to
	// NewEndpointController.
	podLister corelisters.PodLister
	// podsSynced returns true if the pod shared informer has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	podsSynced cache.InformerSynced

	// endpointsLister is able to list/get endpoints and is populated by the shared informer passed to
	// NewEndpointController.
	endpointsLister corelisters.EndpointsLister
	// endpointsSynced returns true if the endpoints shared informer has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	endpointsSynced cache.InformerSynced

	// Services that need to be updated. A channel is inappropriate here,
	// because it allows services with lots of pods to be serviced much
	// more often than services with few pods; it also would cause a
	// service that's inserted multiple times to be processed more than
	// necessary.
	// 那些需要被更新的service，在这里一个channel是不合适的
	// 因为这会让那些有着更多的pods被更多的处理
	// 同时也会让那些插入次数越多的service被更多的处理，而不是那些更需要的
	queue workqueue.RateLimitingInterface

	// workerLoopPeriod is the time between worker runs. The workers process the queue of service and pod changes.
	// worker用于处理队列中的service以及pod的change
	workerLoopPeriod time.Duration
}

// Run will not return until stopCh is closed. workers determines how many
// endpoints will be handled in parallel.
// Run直到stopCh被关闭的时候才返回
// workers决定了有多少个endpoint可以同时处理
func (e *EndpointController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer e.queue.ShutDown()

	glog.Infof("Starting endpoint controller")
	defer glog.Infof("Shutting down endpoint controller")

	if !controller.WaitForCacheSync("endpoint", stopCh, e.podsSynced, e.servicesSynced, e.endpointsSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(e.worker, e.workerLoopPeriod, stopCh)
	}

	go func() {
		defer utilruntime.HandleCrash()
		e.checkLeftoverEndpoints()
	}()

	<-stopCh
}

func (e *EndpointController) getPodServiceMemberships(pod *v1.Pod) (sets.String, error) {
	set := sets.String{}
	// 获取缓存的并且和pod匹配的service
	services, err := e.serviceLister.GetPodServices(pod)
	if err != nil {
		// don't log this error because this function makes pointless
		// errors when no services match.
		return set, nil
	}
	for i := range services {
		key, err := keyFunc(services[i])
		if err != nil {
			return nil, err
		}
		set.Insert(key)
	}
	return set, nil
}

// When a pod is added, figure out what services it will be a member of and
// enqueue them. obj must have *v1.Pod type.
// 当有一个新的pod被添加时，搞清楚它会是哪个service的member并且将它们加入队列
func (e *EndpointController) addPod(obj interface{}) {
	pod := obj.(*v1.Pod)
	services, err := e.getPodServiceMemberships(pod)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Unable to get pod %v/%v's service memberships: %v", pod.Namespace, pod.Name, err))
		return
	}
	for key := range services {
		e.queue.Add(key)
	}
}

func podToEndpointAddress(pod *v1.Pod) *v1.EndpointAddress {
	return &v1.EndpointAddress{
		IP:       pod.Status.PodIP,
		NodeName: &pod.Spec.NodeName,
		TargetRef: &v1.ObjectReference{
			Kind:            "Pod",
			Namespace:       pod.ObjectMeta.Namespace,
			Name:            pod.ObjectMeta.Name,
			UID:             pod.ObjectMeta.UID,
			ResourceVersion: pod.ObjectMeta.ResourceVersion,
		}}
}

func podChanged(oldPod, newPod *v1.Pod) bool {
	// If the pod's deletion timestamp is set, remove endpoint from ready address.
	if newPod.DeletionTimestamp != oldPod.DeletionTimestamp {
		return true
	}
	// If the pod's readiness has changed, the associated endpoint address
	// will move from the unready endpoints set to the ready endpoints.
	// So for the purposes of an endpoint, a readiness change on a pod
	// means we have a changed pod.
	// 当一个pod的readiness发生改变的时候，相关的endpoint address会被从unready endpoints set
	// 移动到ready endpoints set中
	// 因此一个pod的readiness改变了，pod也改变了
	if podutil.IsPodReady(oldPod) != podutil.IsPodReady(newPod) {
		return true
	}
	// Convert the pod to an EndpointAddress, clear inert fields,
	// and see if they are the same.
	newEndpointAddress := podToEndpointAddress(newPod)
	oldEndpointAddress := podToEndpointAddress(oldPod)
	// Ignore the ResourceVersion because it changes
	// with every pod update. This allows the comparison to
	// show equality if all other relevant fields match.
	// 忽略pod的ResourceVersion，因为每次pod update它都会改变
	// 将它们都置为空，就会直接对其他的字段直接进行比较 
	newEndpointAddress.TargetRef.ResourceVersion = ""
	oldEndpointAddress.TargetRef.ResourceVersion = ""
	if reflect.DeepEqual(newEndpointAddress, oldEndpointAddress) {
		// The pod has not changed in any way that impacts the endpoints
		return false
	}
	return true
}

func determineNeededServiceUpdates(oldServices, services sets.String, podChanged bool) sets.String {
	if podChanged {
		// if the labels and pod changed, all services need to be updated
		// 如果labels和pod都改变了，则oldServices和services都需要改变
		services = services.Union(oldServices)
	} else {
		// if only the labels changed, services not common to
		// both the new and old service set (i.e the disjunctive union)
		// need to be updated
		// 如果只有labels改变了，则只有oldServices和services的非交集才需要更新
		services = services.Difference(oldServices).Union(oldServices.Difference(services))
	}
	return services
}

// When a pod is updated, figure out what services it used to be a member of
// and what services it will be a member of, and enqueue the union of these.
// old and cur must be *v1.Pod types.
// 当一个pod更新的时候，搞清楚它曾是哪些service的member以及会成为哪些service的member
// 并将这些联合起来加入队列
// old以及cur都必须为*v1.Pod类型
func (e *EndpointController) updatePod(old, cur interface{}) {
	newPod := cur.(*v1.Pod)
	oldPod := old.(*v1.Pod)
	if newPod.ResourceVersion == oldPod.ResourceVersion {
		// Periodic resync will send update events for all known pods.
		// 阶段性的resync会为所有已知的pods发送update事件
		// Two different versions of the same pod will always have different RVs.
		return
	}

	// pod本身发生了变化，例如readiness，代表的endpoint等等
	podChangedFlag := podChanged(oldPod, newPod)

	// Check if the pod labels have changed, indicating a possibe
	// change in the service membership
	// 检查pod的label是否发生改变，这暗示着可能的service membership的改变
	labelsChanged := false
	if !reflect.DeepEqual(newPod.Labels, oldPod.Labels) ||
		!hostNameAndDomainAreEqual(newPod, oldPod) {
		labelsChanged = true
	}

	// If both the pod and labels are unchanged, no update is needed
	if !podChangedFlag && !labelsChanged {
		return
	}

	services, err := e.getPodServiceMemberships(newPod)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Unable to get pod %v/%v's service memberships: %v", newPod.Namespace, newPod.Name, err))
		return
	}

	if labelsChanged {
		oldServices, err := e.getPodServiceMemberships(oldPod)
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("Unable to get pod %v/%v's service memberships: %v", oldPod.Namespace, oldPod.Name, err))
			return
		}
		// 用于确定哪些service需要更新
		services = determineNeededServiceUpdates(oldServices, services, podChangedFlag)
	}

	for key := range services {
		e.queue.Add(key)
	}
}

func hostNameAndDomainAreEqual(pod1, pod2 *v1.Pod) bool {
	return pod1.Spec.Hostname == pod2.Spec.Hostname &&
		pod1.Spec.Subdomain == pod2.Spec.Subdomain
}

// When a pod is deleted, enqueue the services the pod used to be a member of.
// obj could be an *v1.Pod, or a DeletionFinalStateUnknown marker item.
// 当一个pod被删除的时候，将这个pod曾经是member的service加入队列
// obj可以是一个*v1.Pod，或者是一个DeletionnFinalStateUnkown标记的item
func (e *EndpointController) deletePod(obj interface{}) {
	if _, ok := obj.(*v1.Pod); ok {
		// Enqueue all the services that the pod used to be a member
		// of. This happens to be exactly the same thing we do when a
		// pod is added.
		e.addPod(obj)
		return
	}
	// If we reached here it means the pod was deleted but its final state is unrecorded.
	tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("Couldn't get object from tombstone %#v", obj))
		return
	}
	pod, ok := tombstone.Obj.(*v1.Pod)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("Tombstone contained object that is not a Pod: %#v", obj))
		return
	}
	glog.V(4).Infof("Enqueuing services of deleted pod %s having final state unrecorded", pod.Name)
	e.addPod(pod)
}

// obj could be an *v1.Service, or a DeletionFinalStateUnknown marker item.
func (e *EndpointController) enqueueService(obj interface{}) {
	key, err := keyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %+v: %v", obj, err))
		return
	}

	e.queue.Add(key)
}

// worker runs a worker thread that just dequeues items, processes them, and
// marks them done. You may run as many of these in parallel as you wish; the
// workqueue guarantees that they will not end up processing the same service
// at the same time.
func (e *EndpointController) worker() {
	for e.processNextWorkItem() {
	}
}

func (e *EndpointController) processNextWorkItem() bool {
	eKey, quit := e.queue.Get()
	if quit {
		return false
	}
	defer e.queue.Done(eKey)

	err := e.syncService(eKey.(string))
	e.handleErr(err, eKey)

	return true
}

func (e *EndpointController) handleErr(err error, key interface{}) {
	if err == nil {
		e.queue.Forget(key)
		return
	}

	if e.queue.NumRequeues(key) < maxRetries {
		glog.V(2).Infof("Error syncing endpoints for service %q: %v", key, err)
		e.queue.AddRateLimited(key)
		return
	}

	glog.Warningf("Dropping service %q out of the queue: %v", key, err)
	e.queue.Forget(key)
	utilruntime.HandleError(err)
}

func (e *EndpointController) syncService(key string) error {
	startTime := time.Now()
	defer func() {
		glog.V(4).Infof("Finished syncing service %q endpoints. (%v)", key, time.Now().Sub(startTime))
	}()

	// 从key中获取service所在的namespace和name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	// 根据从key中获取来的namespace和name在serviceLister中找到缓存
	service, err := e.serviceLister.Services(namespace).Get(name)
	if err != nil {
		// Delete the corresponding endpoint, as the service has been deleted.
		// 如果service已经被删除的话，删除对应的endpoint
		// TODO: Please note that this will delete an endpoint when a
		// service is deleted. However, if we're down at the time when
		// the service is deleted, we will miss that deletion, so this
		// doesn't completely solve the problem. See #6877.
		err = e.client.CoreV1().Endpoints(namespace).Delete(name, nil)
		if err != nil && !errors.IsNotFound(err) {
			return err
		}
		return nil
	}

	if service.Spec.Selector == nil {
		// services without a selector receive no endpoints from this controller;
		// these services will receive the endpoints that are created out-of-band via the REST API.
		return nil
	}

	glog.V(5).Infof("About to update endpoints for service %q", key)
	pods, err := e.podLister.Pods(service.Namespace).List(labels.Set(service.Spec.Selector).AsSelectorPreValidated())
	if err != nil {
		// Since we're getting stuff from a local cache, it is
		// basically impossible to get this error.
		return err
	}

	var tolerateUnreadyEndpoints bool
	if v, ok := service.Annotations[TolerateUnreadyEndpointsAnnotation]; ok {
		b, err := strconv.ParseBool(v)
		if err == nil {
			tolerateUnreadyEndpoints = b
		} else {
			utilruntime.HandleError(fmt.Errorf("Failed to parse annotation %v: %v", TolerateUnreadyEndpointsAnnotation, err))
		}
	}

	subsets := []v1.EndpointSubset{}
	var totalReadyEps int = 0
	var totalNotReadyEps int = 0

	for _, pod := range pods {
		if len(pod.Status.PodIP) == 0 {
			glog.V(5).Infof("Failed to find an IP for pod %s/%s", pod.Namespace, pod.Name)
			continue
		}
		if !tolerateUnreadyEndpoints && pod.DeletionTimestamp != nil {
			glog.V(5).Infof("Pod is being deleted %s/%s", pod.Namespace, pod.Name)
			continue
		}

		// 将pod转换为endpoint
		epa := *podToEndpointAddress(pod)

		hostname := pod.Spec.Hostname
		if len(hostname) > 0 && pod.Spec.Subdomain == service.Name && service.Namespace == pod.Namespace {
			epa.Hostname = hostname
		}

		// Allow headless service not to have ports.
		if len(service.Spec.Ports) == 0 {
			if service.Spec.ClusterIP == api.ClusterIPNone {
				epp := v1.EndpointPort{Port: 0, Protocol: v1.ProtocolTCP}
				subsets, totalReadyEps, totalNotReadyEps = addEndpointSubset(subsets, pod, epa, epp, tolerateUnreadyEndpoints)
			}
		} else {
			// 遍历service的所有port
			for i := range service.Spec.Ports {
				servicePort := &service.Spec.Ports[i]

				portName := servicePort.Name
				portProto := servicePort.Protocol
				portNum, err := podutil.FindPort(pod, servicePort)
				if err != nil {
					glog.V(4).Infof("Failed to find port for service %s/%s: %v", service.Namespace, service.Name, err)
					continue
				}

				var readyEps, notReadyEps int
				// 创建endpoint
				epp := v1.EndpointPort{Name: portName, Port: int32(portNum), Protocol: portProto}
				subsets, readyEps, notReadyEps = addEndpointSubset(subsets, pod, epa, epp, tolerateUnreadyEndpoints)
				totalReadyEps = totalReadyEps + readyEps
				totalNotReadyEps = totalNotReadyEps + notReadyEps
			}
		}
	}
	subsets = endpoints.RepackSubsets(subsets)

	// See if there's actually an update here.
	currentEndpoints, err := e.endpointsLister.Endpoints(service.Namespace).Get(service.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			currentEndpoints = &v1.Endpoints{
				ObjectMeta: metav1.ObjectMeta{
					Name:   service.Name,
					Labels: service.Labels,
				},
			}
		} else {
			return err
		}
	}

	// 如果currentEndpoints的ResourceVersion为0
	// 则需要创建endpoint
	createEndpoints := len(currentEndpoints.ResourceVersion) == 0

	if !createEndpoints &&
		apiequality.Semantic.DeepEqual(currentEndpoints.Subsets, subsets) &&
		apiequality.Semantic.DeepEqual(currentEndpoints.Labels, service.Labels) {
		glog.V(5).Infof("endpoints are equal for %s/%s, skipping update", service.Namespace, service.Name)
		return nil
	}
	newEndpoints := currentEndpoints.DeepCopy()
	newEndpoints.Subsets = subsets
	newEndpoints.Labels = service.Labels
	if newEndpoints.Annotations == nil {
		newEndpoints.Annotations = make(map[string]string)
	}

	glog.V(4).Infof("Update endpoints for %v/%v, ready: %d not ready: %d", service.Namespace, service.Name, totalReadyEps, totalNotReadyEps)
	if createEndpoints {
		// No previous endpoints, create them
		// 如果之前没有endpoints，创建它们
		_, err = e.client.CoreV1().Endpoints(service.Namespace).Create(newEndpoints)
	} else {
		// Pre-existing
		// 如果之前已经存在了，就更新
		_, err = e.client.CoreV1().Endpoints(service.Namespace).Update(newEndpoints)
	}
	if err != nil {
		if createEndpoints && errors.IsForbidden(err) {
			// A request is forbidden primarily for two reasons:
			// 一个请求可能因为两个原因被拒绝：
			// 1. namespace is terminating, endpoint creation is not allowed by default.
			// 1. namespace被终止了，endpoint的创建默认是不允许的
			// 2. policy is misconfigured, in which case no service would function anywhere.
			// 2. policy被错误配置了，在这种情况下，没有service能正常运行
			// Given the frequency of 1, we log at a lower level.
			glog.V(5).Infof("Forbidden from creating endpoints: %v", err)
		}
		return err
	}
	return nil
}

// checkLeftoverEndpoints lists all currently existing endpoints and adds their
// service to the queue. This will detect endpoints that exist with no
// corresponding service; these endpoints need to be deleted. We only need to
// do this once on startup, because in steady-state these are detected (but
// some stragglers could have been left behind if the endpoint controller
// reboots).
func (e *EndpointController) checkLeftoverEndpoints() {
	list, err := e.endpointsLister.List(labels.Everything())
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Unable to list endpoints (%v); orphaned endpoints will not be cleaned up. (They're pretty harmless, but you can restart this component if you want another attempt made.)", err))
		return
	}
	for _, ep := range list {
		if _, ok := ep.Annotations[resourcelock.LeaderElectionRecordAnnotationKey]; ok {
			// when there are multiple controller-manager instances,
			// we observe that it will delete leader-election endpoints after 5min
			// and cause re-election
			// so skip the delete here
			// as leader-election only have endpoints without service
			continue
		}
		key, err := keyFunc(ep)
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("Unable to get key for endpoint %#v", ep))
			continue
		}
		e.queue.Add(key)
	}
}

func addEndpointSubset(subsets []v1.EndpointSubset, pod *v1.Pod, epa v1.EndpointAddress,
	epp v1.EndpointPort, tolerateUnreadyEndpoints bool) ([]v1.EndpointSubset, int, int) {
	var readyEps int = 0
	var notReadyEps int = 0
	// 如果能够忍受不ready的endpoints或者pod处于ready状态，则都算ready的endpoint
	if tolerateUnreadyEndpoints || podutil.IsPodReady(pod) {
		subsets = append(subsets, v1.EndpointSubset{
			Addresses: []v1.EndpointAddress{epa},
			Ports:     []v1.EndpointPort{epp},
		})
		readyEps++
	} else if shouldPodBeInEndpoints(pod) {
		glog.V(5).Infof("Pod is out of service: %v/%v", pod.Namespace, pod.Name)
		subsets = append(subsets, v1.EndpointSubset{
			NotReadyAddresses: []v1.EndpointAddress{epa},
			Ports:             []v1.EndpointPort{epp},
		})
		notReadyEps++
	}
	return subsets, readyEps, notReadyEps
}

// shouldPodBeInEndpoints判断pod是否最终死亡
func shouldPodBeInEndpoints(pod *v1.Pod) bool {
	switch pod.Spec.RestartPolicy {
	case v1.RestartPolicyNever:
		return pod.Status.Phase != v1.PodFailed && pod.Status.Phase != v1.PodSucceeded
	case v1.RestartPolicyOnFailure:
		return pod.Status.Phase != v1.PodSucceeded
	default:
		return true
	}
}
