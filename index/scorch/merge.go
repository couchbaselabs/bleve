//  Copyright (c) 2017 Couchbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 		http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package scorch

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RoaringBitmap/roaring"
	"github.com/blevesearch/bleve/index/scorch/mergeplan"
	"github.com/blevesearch/bleve/index/scorch/segment"
	"github.com/blevesearch/bleve/index/scorch/segment/zap"
)

func (s *Scorch) mergerLoop() {
	var lastEpochMergePlanned uint64
OUTER:
	for {
		select {
		case <-s.closeCh:
			break OUTER

		default:
			// check to see if there is a new snapshot to persist
			s.rootLock.RLock()
			ourSnapshot := s.root
			ourSnapshot.AddRef()
			s.rootLock.RUnlock()

			if ourSnapshot.epoch != lastEpochMergePlanned {
				startTime := time.Now()

				// lets get started
				err := s.planMergeAtSnapshot(ourSnapshot)
				if err != nil {
					s.fireAsyncError(fmt.Errorf("merging err: %v", err))
					_ = ourSnapshot.DecRef()
					continue OUTER
				}
				lastEpochMergePlanned = ourSnapshot.epoch

				s.fireEvent(EventKindMergerProgress, time.Since(startTime))

			}
			_ = ourSnapshot.DecRef()

			// tell the persister we're waiting for changes
			// first make a notification chan
			notifyUs := make(notificationChan)

			// give it to the persister
			select {
			case <-s.closeCh:
				break OUTER
			case s.persisterNotifier <- notifyUs:
			}

			// check again
			s.rootLock.RLock()
			ourSnapshot = s.root
			ourSnapshot.AddRef()
			s.rootLock.RUnlock()

			if ourSnapshot.epoch != lastEpochMergePlanned {
				startTime := time.Now()

				// lets get started
				err := s.planMergeAtSnapshot(ourSnapshot)
				if err != nil {
					_ = ourSnapshot.DecRef()
					continue OUTER
				}
				lastEpochMergePlanned = ourSnapshot.epoch

				s.fireEvent(EventKindMergerProgress, time.Since(startTime))
			}
			_ = ourSnapshot.DecRef()

			// now wait for it (but also detect close)
			select {
			case <-s.closeCh:
				break OUTER
			case <-notifyUs:
				// woken up, next loop should pick up work
			}
		}
	}
	s.asyncTasks.Done()
}

func getWorkerCount(taskCount int) int {
	ncpu := runtime.NumCPU()
	if taskCount < ncpu {
		return taskCount
	}
	return ncpu
}

func (s *Scorch) mergeTasks(plan *mergeplan.MergePlan) []error {
	var wg sync.WaitGroup
	size := len(plan.Tasks)
	requestCh := make(chan *mergeplan.MergeTask, size)
	responseCh := make(chan error, size)
	nWorkers := getWorkerCount(size)
	var notifications []notificationChan

	for i := 0; i < nWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for task := range requestCh {
				oldMap := make(map[uint64]*SegmentSnapshot)
				newSegmentID := atomic.AddUint64(&s.nextSegmentID, 1)
				segmentsToMerge := make([]*zap.Segment, 0, len(task.Segments))
				docsToDrop := make([]*roaring.Bitmap, 0, len(task.Segments))
				for _, planSegment := range task.Segments {
					if segSnapshot, ok := planSegment.(*SegmentSnapshot); ok {
						oldMap[segSnapshot.id] = segSnapshot
						if zapSeg, ok := segSnapshot.segment.(*zap.Segment); ok {
							segmentsToMerge = append(segmentsToMerge, zapSeg)
							docsToDrop = append(docsToDrop, segSnapshot.deleted)
						}
					}
				}

				filename := zapFileName(newSegmentID)
				s.markIneligibleForRemoval(filename)
				path := s.path + string(os.PathSeparator) + filename
				newDocNums, err := zap.Merge(segmentsToMerge, docsToDrop, path, 1024)
				if err != nil {
					s.unmarkIneligibleForRemoval(filename)
					responseCh <- fmt.Errorf("merging failed: %v", err)
				}
				segment, err := zap.Open(path)
				if err != nil {
					s.unmarkIneligibleForRemoval(filename)
					responseCh <- err
				}
				sm := &segmentMerge{
					id:            newSegmentID,
					old:           oldMap,
					oldNewDocNums: make(map[uint64][]uint64),
					new:           segment,
					notify:        make(notificationChan),
				}
				notifications = append(notifications, sm.notify)
				for i, segNewDocNums := range newDocNums {
					sm.oldNewDocNums[task.Segments[i].Id()] = segNewDocNums
				}

				// give it to the introducer
				select {
				case <-s.closeCh:
					return
				case s.merges <- sm:
				}
			}

		}()
	}

	// feed the merge workers
	for _, task := range plan.Tasks {
		requestCh <- task
	}
	close(requestCh)
	wg.Wait()
	close(responseCh)

	for _, notification := range notifications {
		select {
		case <-s.closeCh:
			return nil
		case <-notification:
		}
	}

	var errs []error
	for err := range responseCh {
		errs = append(errs, err)
	}
	return errs
}

func (s *Scorch) planMergeAtSnapshot(ourSnapshot *IndexSnapshot) error {
	// build list of zap segments in this snapshot
	var onlyZapSnapshots []mergeplan.Segment
	for _, segmentSnapshot := range ourSnapshot.segment {
		if _, ok := segmentSnapshot.segment.(*zap.Segment); ok {
			onlyZapSnapshots = append(onlyZapSnapshots, segmentSnapshot)
		}
	}

	// give this list to the planner
	resultMergePlan, err := mergeplan.Plan(onlyZapSnapshots, nil)
	if err != nil {
		return fmt.Errorf("merge planning err: %v", err)
	}
	if resultMergePlan == nil {
		// nothing to do
		return nil
	}

	errs := s.mergeTasks(resultMergePlan)
	if len(errs) > 0 {
		var text []string
		for i, err := range errs {
			text = append(text, fmt.Sprintf("#%d: %v\n", i, err))
		}
		return fmt.Errorf("merge: mergeTasks errors: %d, %#v",
			len(errs), text)
	}

	return nil
}

type segmentMerge struct {
	id            uint64
	old           map[uint64]*SegmentSnapshot
	oldNewDocNums map[uint64][]uint64
	new           segment.Segment
	notify        notificationChan
}
