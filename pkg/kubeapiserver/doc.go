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

// The kubeapiserver package holds code that is common to both the kube-apiserver
// and the federation-apiserver, but isn't part of a generic API server.
// kubeapiserver包包含了kube-apiserver和federation-apiserver共有的包，但是不是generic API server
// 的一部分
// For instance, the non-delegated authorization options are used by those two
// servers, but no generic API server is likely to use them.
// 比如，non-delegated authorization options会由这两个servers使用，但是非generic API sever
// 可能会使用它们
package kubeapiserver
