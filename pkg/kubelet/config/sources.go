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

// Package config implements the pod configuration readers.
package config

import (
	"sync"

	"k8s.io/apimachinery/pkg/util/sets"
)

// SourcesReadyFn is function that returns true if the specified sources have been seen.
// 如果特定的sources可以被看到的话，SourcesReadyFn函数就会返回true
type SourcesReadyFn func(sourcesSeen sets.String) bool

// SourcesReady tracks the set of configured sources seen by the kubelet.
// SourcesReady会追踪一系列kubelet可见的configured sources
type SourcesReady interface {
	// AddSource adds the specified source to the set of sources managed.
	// AddSrouce将特定的source加入一系列已经被管理的sources
	AddSource(source string)
	// AllReady returns true if the currently configured sources have all been seen.
	// 如果当前配置的sources都可见的话，AllReady就会返回true
	AllReady() bool
}

// NewSourcesReady returns a SourcesReady with the specified function.
func NewSourcesReady(sourcesReadyFn SourcesReadyFn) SourcesReady {
	return &sourcesImpl{
		sourcesSeen:    sets.NewString(),
		sourcesReadyFn: sourcesReadyFn,
	}
}

// sourcesImpl implements SourcesReady.  It is thread-safe.
type sourcesImpl struct {
	// lock protects access to sources seen.
	lock sync.RWMutex
	// set of sources seen.
	sourcesSeen sets.String
	// sourcesReady is a function that evaluates if the sources are ready.
	// sourcesReady是一个用于评估sources是否处于ready的函数
	sourcesReadyFn SourcesReadyFn
}

// Add adds the specified source to the set of sources managed.
func (s *sourcesImpl) AddSource(source string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.sourcesSeen.Insert(source)
}

// AllReady returns true if each configured source is ready.
func (s *sourcesImpl) AllReady() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.sourcesReadyFn(s.sourcesSeen)
}
