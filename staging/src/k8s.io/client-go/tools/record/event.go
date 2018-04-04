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

package record

import (
	"fmt"
	"math/rand"
	"time"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/clock"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/watch"
	restclient "k8s.io/client-go/rest"
	ref "k8s.io/client-go/tools/reference"

	"net/http"

	"github.com/golang/glog"
)

const maxTriesPerEvent = 12

var defaultSleepDuration = 10 * time.Second

const maxQueuedEvents = 1000

// EventSink knows how to store events (client.Client implements it.)
// EventSink must respect the namespace that will be embedded in 'event'.
// It is assumed that EventSink will return the same sorts of errors as
// pkg/client's REST client.
// EventSink知道如何存储events（client.Client实现了它）
// EventSink必须遵守内嵌在'event'中的namespace
type EventSink interface {
	Create(event *v1.Event) (*v1.Event, error)
	Update(event *v1.Event) (*v1.Event, error)
	Patch(oldEvent *v1.Event, data []byte) (*v1.Event, error)
}

// EventRecorder knows how to record events on behalf of an EventSource.
// EventRecorder知道如何代表一个EventSource记录event
type EventRecorder interface {
	// Event constructs an event from the given information and puts it in the queue for sending.
	// 'object' is the object this event is about. Event will make a reference-- or you may also
	// pass a reference to the object directly.
	// Event根据给定信息构建一个event并且将它放入一个队列里等待发送
	// 'type' of this event, and can be one of Normal, Warning. New types could be added in future
	// 'reason' is the reason this event is generated. 'reason' should be short and unique; it
	// should be in UpperCamelCase format (starting with a capital letter). "reason" will be used
	// to automate handling of events, so imagine people writing switch statements to handle them.
	// You want to make that easy.
	// 'message' is intended to be human readable.
	//
	// The resulting event will be created in the same namespace as the reference object.
	// 创建的event最终会和引用的object处在同一个namespace
	Event(object runtime.Object, eventtype, reason, message string)

	// Eventf is just like Event, but with Sprintf for the message field.
	Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{})

	// PastEventf is just like Eventf, but with an option to specify the event's 'timestamp' field.
	PastEventf(object runtime.Object, timestamp metav1.Time, eventtype, reason, messageFmt string, args ...interface{})
}

// EventBroadcaster knows how to receive events and send them to any EventSink, watcher, or log.
// EventBroadcaster知道如何接受事件并且将它们发送给任何的EventSink，watcher以及log
type EventBroadcaster interface {
	// StartEventWatcher starts sending events received from this EventBroadcaster to the given
	// event handler function. The return value can be ignored or used to stop recording, if
	// desired.
	// StartEventWatcher将从EventBroadcaster收到的event发送到指定的eventHandler
	// 返回值可以忽略或者用来停止记录
	StartEventWatcher(eventHandler func(*v1.Event)) watch.Interface

	// StartRecordingToSink starts sending events received from this EventBroadcaster to the given
	// sink. The return value can be ignored or used to stop recording, if desired.
	// StartRecoringToSink开始将从EventBroadcaster收到的event发送到给定的sink
	StartRecordingToSink(sink EventSink) watch.Interface

	// StartLogging starts sending events received from this EventBroadcaster to the given logging
	// function. The return value can be ignored or used to stop recording, if desired.
	StartLogging(logf func(format string, args ...interface{})) watch.Interface

	// NewRecorder returns an EventRecorder that can be used to send events to this EventBroadcaster
	// with the event source set to the given event source.
	// NewRecorder返回一个EventRecorder，它可以用来给EventBroadcaster发送event
	// 并且将event source设置为给定的event source
	NewRecorder(scheme *runtime.Scheme, source v1.EventSource) EventRecorder
}

// Creates a new event broadcaster.
func NewBroadcaster() EventBroadcaster {
	return &eventBroadcasterImpl{watch.NewBroadcaster(maxQueuedEvents, watch.DropIfChannelFull), defaultSleepDuration}
}

func NewBroadcasterForTests(sleepDuration time.Duration) EventBroadcaster {
	return &eventBroadcasterImpl{watch.NewBroadcaster(maxQueuedEvents, watch.DropIfChannelFull), sleepDuration}
}

type eventBroadcasterImpl struct {
	*watch.Broadcaster
	sleepDuration time.Duration
}

// StartRecordingToSink starts sending events received from the specified eventBroadcaster to the given sink.
// The return value can be ignored or used to stop recording, if desired.
// StartRecordingToSink开始发送从给定的eventBroadcaster中收到的event发送到给定的sink
// 返回值可以被忽略或者用于停止recording
// TODO: make me an object with parameterizable queue length and retry interval
func (eventBroadcaster *eventBroadcasterImpl) StartRecordingToSink(sink EventSink) watch.Interface {
	// The default math/rand package functions aren't thread safe, so create a
	// new Rand object for each StartRecording call.
	randGen := rand.New(rand.NewSource(time.Now().UnixNano()))
	eventCorrelator := NewEventCorrelator(clock.RealClock{})
	return eventBroadcaster.StartEventWatcher(
		func(event *v1.Event) {
			recordToSink(sink, event, eventCorrelator, randGen, eventBroadcaster.sleepDuration)
		})
}

func recordToSink(sink EventSink, event *v1.Event, eventCorrelator *EventCorrelator, randGen *rand.Rand, sleepDuration time.Duration) {
	// Make a copy before modification, because there could be multiple listeners.
	// Events are safe to copy like this.
	// 在对event进行修改之前先拷贝，因为可能有多个listener
	// 这样的拷贝方式对于Event是安全的
	eventCopy := *event
	event = &eventCopy
	result, err := eventCorrelator.EventCorrelate(event)
	if err != nil {
		utilruntime.HandleError(err)
	}
	if result.Skip {
		return
	}
	tries := 0
	for {
		// 根据result.Event.Count是否大于1来确定是否更新一个已经存在的event
		if recordEvent(sink, result.Event, result.Patch, result.Event.Count > 1, eventCorrelator) {
			break
		}
		tries++
		// 每个event最多尝试maxTriesPerEvent，即12次
		if tries >= maxTriesPerEvent {
			glog.Errorf("Unable to write event '%#v' (retry limit exceeded!)", event)
			break
		}
		// Randomize the first sleep so that various clients won't all be
		// synced up if the master goes down.
		// 初始化第一次睡觉时间，这样各个client就不会同时进行同步，当master挂掉的时候
		if tries == 1 {
			time.Sleep(time.Duration(float64(sleepDuration) * randGen.Float64()))
		} else {
			time.Sleep(sleepDuration)
		}
	}
}

func isKeyNotFoundError(err error) bool {
	statusErr, _ := err.(*errors.StatusError)

	if statusErr != nil && statusErr.Status().Code == http.StatusNotFound {
		return true
	}

	return false
}

// recordEvent attempts to write event to a sink. It returns true if the event
// was successfully recorded or discarded, false if it should be retried.
// If updateExistingEvent is false, it creates a new event, otherwise it updates
// existing event.
// recordEvent会试着将event写入sink，它会返回true，如果event被成功地记录或丢弃
// 如果需要重试的话，返回false
// 如果updateExistingEvent为false，它就会创建一个新的event，否则会更新一个已经存在的event
func recordEvent(sink EventSink, event *v1.Event, patch []byte, updateExistingEvent bool, eventCorrelator *EventCorrelator) bool {
	var newEvent *v1.Event
	var err error
	if updateExistingEvent {
		// 如果是更新一个已经存在的event，则用patch进行更新
		newEvent, err = sink.Patch(event, patch)
	}
	// Update can fail because the event may have been removed and it no longer exists.
	// Update可能会失败，因为event可能被移除了
	// 则创建一个新的event
	if !updateExistingEvent || (updateExistingEvent && isKeyNotFoundError(err)) {
		// Making sure that ResourceVersion is empty on creation
		// 在创建的时候，确保ResourceVersion为空
		event.ResourceVersion = ""
		newEvent, err = sink.Create(event)
	}
	if err == nil {
		// we need to update our event correlator with the server returned state to handle name/resourceversion
		// 我们需要用server返回的state更新event correlator用来处理name或者resourceversion
		eventCorrelator.UpdateState(newEvent)
		return true
	}

	// If we can't contact the server, then hold everything while we keep trying.
	// Otherwise, something about the event is malformed and we should abandon it.
	// 如果我们不能和server进行交互，则我们保持所有数据并且继续尝试
	// 否则，和event相关的一些东西已经异常了，我们需要避免它
	switch err.(type) {
	case *restclient.RequestConstructionError:
		// We will construct the request the same next time, so don't keep trying.
		glog.Errorf("Unable to construct event '%#v': '%v' (will not retry!)", event, err)
		return true
	case *errors.StatusError:
		if errors.IsAlreadyExists(err) {
			glog.V(5).Infof("Server rejected event '%#v': '%v' (will not retry!)", event, err)
		} else {
			glog.Errorf("Server rejected event '%#v': '%v' (will not retry!)", event, err)
		}
		return true
	case *errors.UnexpectedObjectError:
		// We don't expect this; it implies the server's response didn't match a
		// known pattern. Go ahead and retry.
	default:
		// This case includes actual http transport errors. Go ahead and retry.
	}
	// 写入event失败
	glog.Errorf("Unable to write event: '%v' (may retry after sleeping)", err)
	return false
}

// StartLogging starts sending events received from this EventBroadcaster to the given logging function.
// The return value can be ignored or used to stop recording, if desired.
func (eventBroadcaster *eventBroadcasterImpl) StartLogging(logf func(format string, args ...interface{})) watch.Interface {
	return eventBroadcaster.StartEventWatcher(
		func(e *v1.Event) {
			// 调用log函数对事件进行记录
			logf("Event(%#v): type: '%v' reason: '%v' %v", e.InvolvedObject, e.Type, e.Reason, e.Message)
		})
}

// StartEventWatcher starts sending events received from this EventBroadcaster to the given event handler function.
// The return value can be ignored or used to stop recording, if desired.
// StartEventWatcher开始发送从EventBroadcaster收集来的event到给定的event handler函数
func (eventBroadcaster *eventBroadcasterImpl) StartEventWatcher(eventHandler func(*v1.Event)) watch.Interface {
	// 调用获取watcher，可以从中获取eventBroadcaster获取的各种事件
	watcher := eventBroadcaster.Watch()
	go func() {
		defer utilruntime.HandleCrash()
		for {
			// 从watcher的channel中获取各类事件
			watchEvent, open := <-watcher.ResultChan()
			if !open {
				return
			}
			event, ok := watchEvent.Object.(*v1.Event)
			if !ok {
				// This is all local, so there's no reason this should
				// ever happen.
				continue
			}
			// 最终调用hanler进行处理
			eventHandler(event)
		}
	}()
	return watcher
}

// NewRecorder returns an EventRecorder that records events with the given event source.
func (eventBroadcaster *eventBroadcasterImpl) NewRecorder(scheme *runtime.Scheme, source v1.EventSource) EventRecorder {
	return &recorderImpl{scheme, source, eventBroadcaster.Broadcaster, clock.RealClock{}}
}

type recorderImpl struct {
	scheme *runtime.Scheme
	source v1.EventSource
	// 可直接调用Broadcaster的方法
	*watch.Broadcaster
	clock clock.Clock
}

func (recorder *recorderImpl) generateEvent(object runtime.Object, timestamp metav1.Time, eventtype, reason, message string) {
	ref, err := ref.GetReference(recorder.scheme, object)
	if err != nil {
		glog.Errorf("Could not construct reference to: '%#v' due to: '%v'. Will not report event: '%v' '%v' '%v'", object, err, eventtype, reason, message)
		return
	}

	if !validateEventType(eventtype) {
		glog.Errorf("Unsupported event type: '%v'", eventtype)
		return
	}

	event := recorder.makeEvent(ref, eventtype, reason, message)
	event.Source = recorder.source

	go func() {
		// NOTE: events should be a non-blocking operation
		defer utilruntime.HandleCrash()
		recorder.Action(watch.Added, event)
	}()
}

func validateEventType(eventtype string) bool {
	switch eventtype {
	// 目前的eventtype只有normal和warning两种类型
	case v1.EventTypeNormal, v1.EventTypeWarning:
		return true
	}
	return false
}

func (recorder *recorderImpl) Event(object runtime.Object, eventtype, reason, message string) {
	recorder.generateEvent(object, metav1.Now(), eventtype, reason, message)
}

func (recorder *recorderImpl) Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	recorder.Event(object, eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

func (recorder *recorderImpl) PastEventf(object runtime.Object, timestamp metav1.Time, eventtype, reason, messageFmt string, args ...interface{}) {
	recorder.generateEvent(object, timestamp, eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

func (recorder *recorderImpl) makeEvent(ref *v1.ObjectReference, eventtype, reason, message string) *v1.Event {
	t := metav1.Time{Time: recorder.clock.Now()}
	namespace := ref.Namespace
	if namespace == "" {
		namespace = metav1.NamespaceDefault
	}
	return &v1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%v.%x", ref.Name, t.UnixNano()),
			Namespace: namespace,
		},
		InvolvedObject: *ref,
		Reason:         reason,
		Message:        message,
		FirstTimestamp: t,
		LastTimestamp:  t,
		Count:          1,
		Type:           eventtype,
	}
}
