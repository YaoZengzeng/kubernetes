/*
Copyright 2017 The Kubernetes Authors.

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

package pager

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

const defaultPageSize = 500
const defaultPageBufferSize = 10

// ListPageFunc returns a list object for the given list options.
// ListPageFunc为给定的list options返回一个list object
type ListPageFunc func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error)

// SimplePageFunc adapts a context-less list function into one that accepts a context.
// SimplePageFunc将一个context-less的list函数适配到能够接受一个context
func SimplePageFunc(fn func(opts metav1.ListOptions) (runtime.Object, error)) ListPageFunc {
	return func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return fn(opts)
	}
}

// ListPager assists client code in breaking large list queries into multiple
// smaller chunks of PageSize or smaller. PageFn is expected to accept a
// metav1.ListOptions that supports paging and return a list. The pager does
// not alter the field or label selectors on the initial options list.
// ListPager帮助client code将大的list queries划分为多个小的PageSize的chunks或者更小
// PageFn期望接受一个metav1.ListOptions能够支持paging并且返回一个list
type ListPager struct {
	PageSize int64
	PageFn   ListPageFunc

	FullListIfExpired bool

	// Number of pages to buffer
	PageBufferSize int32
}

// New creates a new pager from the provided pager function using the default
// options. It will fall back to a full list if an expiration error is encountered
// as a last resort.
// New用默认的配置从给定的pager function创建一个新的pager，如果上一次报错是expiration error
// 则它会回到一个full list的状态
func New(fn ListPageFunc) *ListPager {
	return &ListPager{
		PageSize:          defaultPageSize,
		PageFn:            fn,
		FullListIfExpired: true,
		PageBufferSize:    defaultPageBufferSize,
	}
}

// TODO: introduce other types of paging functions - such as those that retrieve from a list
// of namespaces.

// List returns a single list object, but attempts to retrieve smaller chunks from the
// server to reduce the impact on the server. If the chunk attempt fails, it will load
// the full list instead. The Limit field on options, if unset, will default to the page size.
// List返回单个的list object，但是试着从server获取更小的chunks来减小对server的压力，如果尝试获取chunk失败
// 它会转而载入完整的list，options中的Limit字段会默认设置为page size
func (p *ListPager) List(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
	if options.Limit == 0 {
		options.Limit = p.PageSize
	}
	var list *metainternalversion.List
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		obj, err := p.PageFn(ctx, options)
		if err != nil {
			if !errors.IsResourceExpired(err) || !p.FullListIfExpired {
				return nil, err
			}
			// the list expired while we were processing, fall back to a full list
			// 我们在处理的时候list过期了，重新回到full list
			// 可能是server的配置发生了变化
			options.Limit = 0
			options.Continue = ""
			return p.PageFn(ctx, options)
		}
		// 从对象中解析出ListAccessor
		m, err := meta.ListAccessor(obj)
		if err != nil {
			return nil, fmt.Errorf("returned object must be a list: %v", err)
		}

		// exit early and return the object we got if we haven't processed any pages
		// 如果我们没有处理任何的pages则提早退出并且返回我们已经获取的对象
		if len(m.GetContinue()) == 0 && list == nil {
			return obj, nil
		}

		// initialize the list and fill its contents
		// 初始化list并且用它的内容进行填充
		if list == nil {
			list = &metainternalversion.List{Items: make([]runtime.Object, 0, options.Limit+1)}
			list.ResourceVersion = m.GetResourceVersion()
			list.SelfLink = m.GetSelfLink()
		}
		if err := meta.EachListItem(obj, func(obj runtime.Object) error {
			// 扩展list的Items对象
			list.Items = append(list.Items, obj)
			return nil
		}); err != nil {
			return nil, err
		}

		// if we have no more items, return the list
		// 如果没有更多的items，则直接返回list
		if len(m.GetContinue()) == 0 {
			return list, nil
		}

		// set the next loop up
		// 开始下一次循环
		options.Continue = m.GetContinue()
	}
}

// EachListItem fetches runtime.Object items using this ListPager and invokes fn on each item. If
// fn returns an error, processing stops and that error is returned. If fn does not return an error,
// any error encountered while retrieving the list from the server is returned. If the context
// cancels or times out, the context error is returned. Since the list is retrieved in paginated
// chunks, an "Expired" error (metav1.StatusReasonExpired) may be returned if the pagination list
// requests exceed the expiration limit of the apiserver being called.
//
// Items are retrieved in chunks from the server to reduce the impact on the server with up to
// ListPager.PageBufferSize chunks buffered concurrently in the background.
func (p *ListPager) EachListItem(ctx context.Context, options metav1.ListOptions, fn func(obj runtime.Object) error) error {
	return p.eachListChunkBuffered(ctx, options, func(obj runtime.Object) error {
		return meta.EachListItem(obj, fn)
	})
}

// eachListChunkBuffered fetches runtimeObject list chunks using this ListPager and invokes fn on
// each list chunk.  If fn returns an error, processing stops and that error is returned. If fn does
// not return an error, any error encountered while retrieving the list from the server is
// returned. If the context cancels or times out, the context error is returned. Since the list is
// retrieved in paginated chunks, an "Expired" error (metav1.StatusReasonExpired) may be returned if
// the pagination list requests exceed the expiration limit of the apiserver being called.
//
// Up to ListPager.PageBufferSize chunks are buffered concurrently in the background.
func (p *ListPager) eachListChunkBuffered(ctx context.Context, options metav1.ListOptions, fn func(obj runtime.Object) error) error {
	if p.PageBufferSize < 0 {
		return fmt.Errorf("ListPager.PageBufferSize must be >= 0, got %d", p.PageBufferSize)
	}

	// Ensure background goroutine is stopped if this call exits before all list items are
	// processed. Cancelation error from this deferred cancel call is never returned to caller;
	// either the list result has already been sent to bgResultC or the fn error is returned and
	// the cancelation error is discarded.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	chunkC := make(chan runtime.Object, p.PageBufferSize)
	bgResultC := make(chan error, 1)
	go func() {
		defer utilruntime.HandleCrash()

		var err error
		defer func() {
			close(chunkC)
			bgResultC <- err
		}()
		err = p.eachListChunk(ctx, options, func(chunk runtime.Object) error {
			select {
			case chunkC <- chunk: // buffer the chunk, this can block
			case <-ctx.Done():
				return ctx.Err()
			}
			return nil
		})
	}()

	for o := range chunkC {
		err := fn(o)
		if err != nil {
			return err // any fn error should be returned immediately
		}
	}
	// promote the results of our background goroutine to the foreground
	return <-bgResultC
}

// eachListChunk fetches runtimeObject list chunks using this ListPager and invokes fn on each list
// chunk. If fn returns an error, processing stops and that error is returned. If fn does not return
// an error, any error encountered while retrieving the list from the server is returned. If the
// context cancels or times out, the context error is returned. Since the list is retrieved in
// paginated chunks, an "Expired" error (metav1.StatusReasonExpired) may be returned if the
// pagination list requests exceed the expiration limit of the apiserver being called.
func (p *ListPager) eachListChunk(ctx context.Context, options metav1.ListOptions, fn func(obj runtime.Object) error) error {
	if options.Limit == 0 {
		options.Limit = p.PageSize
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		obj, err := p.PageFn(ctx, options)
		if err != nil {
			return err
		}
		m, err := meta.ListAccessor(obj)
		if err != nil {
			return fmt.Errorf("returned object must be a list: %v", err)
		}
		if err := fn(obj); err != nil {
			return err
		}
		// if we have no more items, return.
		if len(m.GetContinue()) == 0 {
			return nil
		}
		// set the next loop up
		options.Continue = m.GetContinue()
	}
}
