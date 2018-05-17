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

// Reads the pod configuration from an HTTP GET response.
package config

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	api "k8s.io/kubernetes/pkg/apis/core"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"

	"github.com/golang/glog"
	"k8s.io/apimachinery/pkg/types"
)

type sourceURL struct {
	url         string
	header      http.Header
	nodeName    types.NodeName
	updates     chan<- interface{}
	data        []byte
	failureLogs int
	client      *http.Client
}

func NewSourceURL(url string, header http.Header, nodeName types.NodeName, period time.Duration, updates chan<- interface{}) {
	config := &sourceURL{
		url:      url,
		header:   header,
		nodeName: nodeName,
		updates:  updates,
		data:     nil,
		// Timing out requests leads to retries. This client is only used to
		// read the manifest URL passed to kubelet.
		client: &http.Client{Timeout: 10 * time.Second},
	}
	glog.V(1).Infof("Watching URL %s", url)
	// 定期从http获取pod信息
	go wait.Until(config.run, period, wait.NeverStop)
}

func (s *sourceURL) run() {
	// 定期从URL拉取pod信息
	if err := s.extractFromURL(); err != nil {
		// Don't log this multiple times per minute. The first few entries should be
		// enough to get the point across.
		if s.failureLogs < 3 {
			glog.Warningf("Failed to read pods from URL: %v", err)
		} else if s.failureLogs == 3 {
			glog.Warningf("Failed to read pods from URL. Dropping verbosity of this message to V(4): %v", err)
		} else {
			glog.V(4).Infof("Failed to read pods from URL: %v", err)
		}
		s.failureLogs++
	} else {
		if s.failureLogs > 0 {
			glog.Info("Successfully read pods from URL.")
			// 将failureLogs清空
			s.failureLogs = 0
		}
	}
}

func (s *sourceURL) applyDefaults(pod *api.Pod) error {
	return applyDefaults(pod, s.url, false, s.nodeName)
}

func (s *sourceURL) extractFromURL() error {
	req, err := http.NewRequest("GET", s.url, nil)
	if err != nil {
		return err
	}
	req.Header = s.header
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%v: %v", s.url, resp.Status)
	}
	if len(data) == 0 {
		// Emit an update with an empty PodList to allow HTTPSource to be marked as seen
		s.updates <- kubetypes.PodUpdate{Pods: []*v1.Pod{}, Op: kubetypes.SET, Source: kubetypes.HTTPSource}
		return fmt.Errorf("zero-length data received from %v", s.url)
	}
	// Short circuit if the data has not changed since the last time it was read.
	// 如果这次获取的数据和上次获取的数据相同，则直接返回
	if bytes.Compare(data, s.data) == 0 {
		return nil
	}
	s.data = data

	// First try as it is a single pod.
	// 首先尝试解析是否为单个的pod
	parsed, pod, singlePodErr := tryDecodeSinglePod(data, s.applyDefaults)
	if parsed {
		if singlePodErr != nil {
			// It parsed but could not be used.
			return singlePodErr
		}
		// 同样设置为SET类型的podUpdate
		s.updates <- kubetypes.PodUpdate{Pods: []*v1.Pod{pod}, Op: kubetypes.SET, Source: kubetypes.HTTPSource}
		return nil
	}

	// That didn't work, so try a list of pods.
	// 尝试解析pod list
	parsed, podList, multiPodErr := tryDecodePodList(data, s.applyDefaults)
	if parsed {
		if multiPodErr != nil {
			// It parsed but could not be used.
			return multiPodErr
		}
		pods := make([]*v1.Pod, 0)
		for i := range podList.Items {
			pods = append(pods, &podList.Items[i])
		}
		s.updates <- kubetypes.PodUpdate{Pods: pods, Op: kubetypes.SET, Source: kubetypes.HTTPSource}
		return nil
	}

	return fmt.Errorf("%v: received '%v', but couldn't parse as "+
		"single (%v) or multiple pods (%v).\n",
		s.url, string(data), singlePodErr, multiPodErr)
}
