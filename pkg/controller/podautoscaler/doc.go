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

// Package podautoscaler contains logic for autoscaling number of
// pods based on metrics observed.
// podautoscaler包含基于发现的metrics指标来自动扩缩容pods数目的逻辑
package podautoscaler // import "k8s.io/kubernetes/pkg/controller/podautoscaler"
