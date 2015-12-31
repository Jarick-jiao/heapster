// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package manager

import (
	"fmt"
	"time"

	"k8s.io/heapster/metrics/core"

	"github.com/golang/glog"
)

const (
	DefaultScrapeOffset   = 5 * time.Second
	DefaultMaxParallelism = 3
)

type Manager interface {
	Start()
	Stop()
}

type realManager struct {
	source                 core.MetricsSource
	processors             []core.DataProcessor
	sink                   core.DataSink
	resolution             time.Duration
	scrapeOffset           time.Duration
	stopChan               chan struct{}
	housekeepSemaphoreChan chan struct{}
	housekeepTimeout       time.Duration
}

func NewManager(source core.MetricsSource, processors []core.DataProcessor, sink core.DataSink, resolution time.Duration,
	scrapeOffset time.Duration, maxParallelism int) (Manager, error) {
	manager := realManager{
		source:                 source,
		processors:             processors,
		sink:                   sink,
		resolution:             resolution,
		scrapeOffset:           scrapeOffset,
		stopChan:               make(chan struct{}),
		housekeepSemaphoreChan: make(chan struct{}, maxParallelism),
		housekeepTimeout:       resolution / 2,
	}

	for i := 0; i < maxParallelism; i++ {
		manager.housekeepSemaphoreChan <- struct{}{}
	}

	return &manager, nil
}

func (rm *realManager) Start() {
	go rm.Housekeep()
}

func (rm *realManager) Stop() {
	rm.stopChan <- struct{}{}
}

func (rm *realManager) Housekeep() {
	for {
		// Always try to get the newest metrics
		now := time.Now()
		start := now.Truncate(rm.resolution)
		end := start.Add(rm.resolution)
		timeToNextSync := end.Add(rm.scrapeOffset).Sub(now)

		select {
		case <-time.After(timeToNextSync):
			rm.housekeep(start, end)
		case <-rm.stopChan:
			rm.sink.Stop()
			return
		}
	}
}

func (rm *realManager) housekeep(start, end time.Time) {
	if !start.Before(end) {
		glog.Warningf("Wrong time provided to housekeep start:%s end: %s", start, end)
		return
	}

	select {
	case <-rm.housekeepSemaphoreChan:
		// ok, good to go

	case <-time.After(rm.housekeepTimeout):
		glog.Warningf("Spent too long waiting for housekeeping to start")
		return
	}

	go func(rm *realManager) {
		// should always give back the semaphore
		defer func() { rm.housekeepSemaphoreChan <- struct{}{} }()
		data := rm.source.ScrapeMetrics(start, end)
		for _, p := range rm.processors {
			newData, err := p.Process(data)
			if err == nil {
				data = newData
			} else {
				glog.Fatalf("Error in processor: %v", err)
				// Try to proceed without this processor to push SOMETHING.
				// TODO: evaluate if this approach is better than stopping the whole flow.
			}
		}

		//export time of the last metrics scrape to prometheus
		timeStamp, err := time.Now().UTC().MarshalJSON()
		if err != nil {
			glog.Warningf("failed to get timestamp: %v", err)
		}
		core.LastMetricsTimeStamp.WithLabelValues(string(timeStamp))

		//export time spent in exporting data to sinks (single run, not cumulative)
		startTime := time.Now()
		//export data to sinks
		rm.sink.ExportData(data)
		duration := fmt.Sprintf("%s", time.Now().Sub(startTime))
		core.ExportingDurations.WithLabelValues(duration)

	}(rm)
}
