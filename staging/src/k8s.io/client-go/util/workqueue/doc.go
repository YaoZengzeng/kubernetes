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

// Package workqueue provides a simple queue that supports the following
// features:
// workqueue提供的队列具有以下特性：
//  * 公平：队列的条目是按它们添加的顺序被处理的
//	* Stingy: 单个的条目不会被同时处理多次，如果一个条目在它被处理前多次添加，最终它只会被处理一次
//	* 多个生产者以及多个消费者，特别地，它允许一个条目正在被处理的时候重新入队
//	* 关闭时候的通知
//  * Fair: items processed in the order in which they are added.
//  * Stingy: a single item will not be processed multiple times concurrently,
//      and if an item is added multiple times before it can be processed, it
//      will only be processed once.
//  * Multiple consumers and producers. In particular, it is allowed for an
//      item to be reenqueued while it is being processed.
//  * Shutdown notifications.
package workqueue
