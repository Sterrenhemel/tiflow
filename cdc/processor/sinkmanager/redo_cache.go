// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package sinkmanager

import (
	"sort"
	"sync"
	"sync/atomic"

	"github.com/pingcap/tiflow/cdc/model"
	"github.com/pingcap/tiflow/cdc/processor/sourcemanager/engine"
	"github.com/prometheus/client_golang/prometheus"
)

// redoEventCache caches events fetched from EventSortEngine.
type redoEventCache struct {
	capacity  uint64 // it's a constant.
	allocated uint64 // atomically shared in several goroutines.

	mu     sync.Mutex
	tables map[model.TableID]*eventAppender

	metricRedoEventCache prometheus.Gauge
}

// newRedoEventCache creates a redoEventCache instance.
func newRedoEventCache(changefeedID model.ChangeFeedID, capacity uint64) *redoEventCache {
	return &redoEventCache{
		capacity:  capacity,
		allocated: 0,
		tables:    make(map[model.TableID]*eventAppender),

		metricRedoEventCache: RedoEventCache.WithLabelValues(changefeedID.Namespace, changefeedID.ID),
	}
}

// getAppender returns an eventAppender instance which can be used to
// append events into the cache.
func (r *redoEventCache) getAppender(tableID model.TableID) *eventAppender {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, exists := r.tables[tableID]
	if !exists {
		item = &eventAppender{capacity: r.capacity, cache: r}
		r.tables[tableID] = item
	}
	return item
}

// pop some events from the cache.
func (r *redoEventCache) pop(
	tableID model.TableID,
	pushCount *int, // poped events come from how many `pushBatch` calls
	upperBound ...engine.Position,
) ([]*model.RowChangedEvent, uint64, engine.Position) {
	r.mu.Lock()
	item, exists := r.tables[tableID]
	if !exists {
		r.mu.Unlock()
		return nil, 0, engine.Position{}
	}
	r.mu.Unlock()

	item.mu.RLock()
	defer item.mu.RUnlock()
	if len(item.events) == 0 || item.readyCount == 0 {
		return nil, 0, engine.Position{}
	}

	fetchCount := item.readyCount
	if len(upperBound) > 0 {
		fetchCount = sort.Search(item.readyCount, func(i int) bool {
			pos := engine.Position{
				CommitTs: item.events[i].CommitTs,
				StartTs:  item.events[i].StartTs,
			}
			return pos.Compare(upperBound[0]) > 0
		})
		if fetchCount == 0 {
			return nil, 0, engine.Position{}
		}
	}

	events := item.events[0:fetchCount]
	var size uint64 = 0
	for _, x := range item.sizes[0:fetchCount] {
		size += x
	}
	if pushCount != nil {
		for _, x := range item.pushCounts[0:fetchCount] {
			*pushCount += int(x)
		}
	}
	pos := engine.Position{
		CommitTs: item.events[fetchCount-1].CommitTs,
		StartTs:  item.events[fetchCount-1].StartTs,
	}

	item.events = item.events[fetchCount:]
	item.sizes = item.sizes[fetchCount:]
	item.pushCounts = item.pushCounts[fetchCount:]
	if len(item.events) == 0 {
		r.mu.Lock()
		delete(r.tables, tableID)
		r.mu.Unlock()
	} else {
		item.readyCount -= fetchCount
	}

	atomic.AddUint64(&r.allocated, ^(size - 1))
	r.metricRedoEventCache.Sub(float64(size))
	return events, size, pos
}

func (r *redoEventCache) removeTable(tableID model.TableID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, exists := r.tables[tableID]
	if exists {
		item.mu.Lock()
		defer item.mu.Unlock()
		delete(r.tables, tableID)
		item.events = nil
		item.sizes = nil
		item.pushCounts = nil
	}
}

type eventAppender struct {
	capacity uint64
	cache    *redoEventCache

	broken bool

	mu         sync.RWMutex
	events     []*model.RowChangedEvent
	sizes      []uint64
	readyCount int // Count of ready events

	// Several RowChangedEvent can come from one PolymorphicEvent.
	pushCounts []byte
}

func (e *eventAppender) push(
	event *model.RowChangedEvent, size uint64,
	txnFinished bool,
	eventsInSameBatch ...*model.RowChangedEvent,
) bool {
	// At most only one client can call push on a given eventAppender instance,
	// so lock is unnecessary.
	if e.broken {
		return false
	}

	for {
		allocated := atomic.LoadUint64(&e.cache.allocated)
		if allocated >= e.capacity {
			e.broken = true
			return false
		}
		if atomic.CompareAndSwapUint64(&e.cache.allocated, allocated, allocated+size) {
			e.cache.metricRedoEventCache.Add(float64(size))
			break
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, event)
	e.sizes = append(e.sizes, size)
	e.pushCounts = append(e.pushCounts, 1)
	for _, event := range eventsInSameBatch {
		e.events = append(e.events, event)
		e.sizes = append(e.sizes, 0)
		e.pushCounts = append(e.pushCounts, 0)
	}
	if txnFinished {
		e.readyCount = len(e.events)
	}
	return true
}

// All events should come from one PolymorphicEvent.
func (e *eventAppender) pushBatch(events []*model.RowChangedEvent, size uint64, txnFinished bool) bool {
	if len(events) == 0 {
		return true
	}
	return e.push(events[0], size, txnFinished, events[1:]...)
}

func (e *eventAppender) cleanBrokenEvents() (pendingSize uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for i := e.readyCount; i < len(e.events); i++ {
		pendingSize += e.sizes[i]
		e.events[i] = nil
	}

	e.events = e.events[0:e.readyCount]
	e.sizes = e.sizes[0:e.readyCount]
	e.pushCounts = e.pushCounts[0:e.readyCount]

	e.broken = false
	atomic.AddUint64(&e.cache.allocated, ^(pendingSize - 1))
	e.cache.metricRedoEventCache.Sub(float64(pendingSize))

	return
}
