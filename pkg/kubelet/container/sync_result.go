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

package container

import (
	"errors"
	"fmt"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
)

// TODO(random-liu): We need to better organize runtime errors for introspection.

// Container Terminated and Kubelet is backing off the restart
var ErrCrashLoopBackOff = errors.New("CrashLoopBackOff")

var (
	// ErrContainerNotFound returned when a container in the given pod with the
	// given container name was not found, amongst those managed by the kubelet.
	ErrContainerNotFound = errors.New("no matching container")
)

var (
	ErrRunContainer     = errors.New("RunContainerError")
	ErrKillContainer    = errors.New("KillContainerError")
	ErrVerifyNonRoot    = errors.New("VerifyNonRootError")
	ErrRunInitContainer = errors.New("RunInitContainerError")
	ErrCreatePodSandbox = errors.New("CreatePodSandboxError")
	ErrConfigPodSandbox = errors.New("ConfigPodSandboxError")
	ErrKillPodSandbox   = errors.New("KillPodSandboxError")
)

var (
	ErrSetupNetwork    = errors.New("SetupNetworkError")
	ErrTeardownNetwork = errors.New("TeardownNetworkError")
)

// SyncAction indicates different kind of actions in SyncPod() and KillPod(). Now there are only actions
// about start/kill container and setup/teardown network.
type SyncAction string

const (
	StartContainer   SyncAction = "StartContainer"
	KillContainer    SyncAction = "KillContainer"
	SetupNetwork     SyncAction = "SetupNetwork"
	TeardownNetwork  SyncAction = "TeardownNetwork"
	InitContainer    SyncAction = "InitContainer"
	CreatePodSandbox SyncAction = "CreatePodSandbox"
	ConfigPodSandbox SyncAction = "ConfigPodSandbox"
	KillPodSandbox   SyncAction = "KillPodSandbox"
)

// SyncResult is the result of sync action.
type SyncResult struct {
	// The associated action of the result
	// result相关的action
	Action SyncAction
	// The target of the action, now the target can only be:
	//  * Container: Target should be container name
	//  * Network: Target is useless now, we just set it as pod full name now
	// action对应的target，现在target只能为Container或者Network
	// 对于Container: Target为container name
	// 对于Network: Target暂时没什么用，我们只是把它设置为pod full name
	Target interface{}
	// Brief error reason
	Error error
	// Human readable error reason
	Message string
}

// NewSyncResult generates new SyncResult with specific Action and Target
// NewSyncResult用给定的Action和Target创建新的SyncResult
func NewSyncResult(action SyncAction, target interface{}) *SyncResult {
	return &SyncResult{Action: action, Target: target}
}

// Fail fails the SyncResult with specific error and message
func (r *SyncResult) Fail(err error, msg string) {
	r.Error, r.Message = err, msg
}

// PodSyncResult is the summary result of SyncPod() and KillPod()
// PodSyncResult包含了SyncPod()和KillPod()的结果
type PodSyncResult struct {
	// Result of different sync actions
	// 不同的sync action的Result
	SyncResults []*SyncResult
	// Error encountered in SyncPod() and KillPod() that is not already included in SyncResults
	// 那些还未包含在SyncResult中的，在SyncPod()和KillPod()中遇到的问题
	SyncError error
}

// AddSyncResult adds multiple SyncResult to current PodSyncResult
func (p *PodSyncResult) AddSyncResult(result ...*SyncResult) {
	p.SyncResults = append(p.SyncResults, result...)
}

// AddPodSyncResult merges a PodSyncResult to current one
func (p *PodSyncResult) AddPodSyncResult(result PodSyncResult) {
	p.AddSyncResult(result.SyncResults...)
	p.SyncError = result.SyncError
}

// Fail fails the PodSyncResult with an error occurred in SyncPod() and KillPod() itself
func (p *PodSyncResult) Fail(err error) {
	p.SyncError = err
}

// Error returns an error summarizing all the errors in PodSyncResult
func (p *PodSyncResult) Error() error {
	errlist := []error{}
	if p.SyncError != nil {
		errlist = append(errlist, fmt.Errorf("failed to SyncPod: %v\n", p.SyncError))
	}
	for _, result := range p.SyncResults {
		if result.Error != nil {
			errlist = append(errlist, fmt.Errorf("failed to %q for %q with %v: %q\n", result.Action, result.Target,
				result.Error, result.Message))
		}
	}
	return utilerrors.NewAggregate(errlist)
}
