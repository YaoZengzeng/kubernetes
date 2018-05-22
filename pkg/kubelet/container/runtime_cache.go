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
	"sync"
	"time"
)

var (
	// TODO(yifan): Maybe set the them as parameters for NewCache().
	// 设置Runtime Cache的更新时间为2秒
	defaultCachePeriod = time.Second * 2
)

type RuntimeCache interface {
	GetPods() ([]*Pod, error)
	ForceUpdateIfOlder(time.Time) error
}

type podsGetter interface {
	GetPods(bool) ([]*Pod, error)
}

// NewRuntimeCache creates a container runtime cache.
// NewRuntimeCache创建一个容器运行时的cache
func NewRuntimeCache(getter podsGetter) (RuntimeCache, error) {
	return &runtimeCache{
		getter: getter,
	}, nil
}

// runtimeCache caches a list of pods. It records a timestamp (cacheTime) right
// before updating the pods, so the timestamp is at most as new as the pods
// (and can be slightly older). The timestamp always moves forward. Callers are
// expected not to modify the pods returned from GetPods.
// runtimeCache缓存了一系列的pods，它在更新pod之前记录了一个timestamp(cacheTime)
// 调用者不应该修改GetPods返回的pods
type runtimeCache struct {
	sync.Mutex
	// The underlying container runtime used to update the cache.
	// 底层的容器运行时用于更新cache
	getter podsGetter
	// Last time when cache was updated.
	// cache上一次更新的时间
	cacheTime time.Time
	// The content of the cache.
	// pods就是cache的内容
	pods []*Pod
}

// GetPods returns the cached pods if they are not outdated; otherwise, it
// retrieves the latest pods and return them.
// GetPods返回缓存的pods，如果它们没有过期的话
// 否则获取最新的pods并返回它们
func (r *runtimeCache) GetPods() ([]*Pod, error) {
	r.Lock()
	defer r.Unlock()
	if time.Since(r.cacheTime) > defaultCachePeriod {
		// 如果超过2秒没有更新了，则调用r.updateCache()进行更新
		if err := r.updateCache(); err != nil {
			return nil, err
		}
	}
	return r.pods, nil
}

func (r *runtimeCache) ForceUpdateIfOlder(minExpectedCacheTime time.Time) error {
	r.Lock()
	defer r.Unlock()
	// 如果保存的cache time比minExpectedCacheTime更早
	// 则强制更新
	if r.cacheTime.Before(minExpectedCacheTime) {
		return r.updateCache()
	}
	return nil
}

func (r *runtimeCache) updateCache() error {
	pods, timestamp, err := r.getPodsWithTimestamp()
	if err != nil {
		return err
	}
	r.pods, r.cacheTime = pods, timestamp
	return nil
}

// getPodsWithTimestamp records a timestamp and retrieves pods from the getter.
// getPodsWithTimestamp记录一个timestamp并且从getter中获取pods
func (r *runtimeCache) getPodsWithTimestamp() ([]*Pod, time.Time, error) {
	// Always record the timestamp before getting the pods to avoid stale pods.
	// 总是在获取pods之前记录timestamp，从而避免stale pods
	timestamp := time.Now()
	pods, err := r.getter.GetPods(false)
	return pods, timestamp, err
}
