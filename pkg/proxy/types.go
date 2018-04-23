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

package proxy

import (
	"fmt"

	"k8s.io/apimachinery/pkg/types"
)

// ProxyProvider is the interface provided by proxier implementations.
type ProxyProvider interface {
	// Sync immediately synchronizes the ProxyProvider's current state to iptables.
	// Sync马上将ProxyProvider的当前状态同步到iptables
	Sync()
	// SyncLoop runs periodic work.
	// This is expected to run as a goroutine or as the main loop of the app.
	// It does not return.
	// SyncLoop阶段性地进行工作
	// 它期望在一个goroutine或者作为一个app的main loop，它并不退出
	SyncLoop()
}

// ServicePortName carries a namespace + name + portname.  This is the unique
// identfier for a load-balanced service.
// ServicePortName包含了一个namespace + name + portname
// 这对于一个load-balanced服务是一个独特的标识符
type ServicePortName struct {
	types.NamespacedName
	Port string
}

func (spn ServicePortName) String() string {
	return fmt.Sprintf("%s:%s", spn.NamespacedName.String(), spn.Port)
}
