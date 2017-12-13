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
	"sync"

	"github.com/RoaringBitmap/roaring"
	"github.com/blevesearch/bleve/index"
	"github.com/blevesearch/bleve/index/scorch/segment"
)

var ByteSeparator byte = 0xff

type SegmentDictionarySnapshot struct {
	s *SegmentSnapshot
	d segment.TermDictionary
}

func (s *SegmentDictionarySnapshot) PostingsList(term string, except *roaring.Bitmap) (segment.PostingsList, error) {
	return s.d.PostingsList(term, s.s.deleted)
}

func (s *SegmentDictionarySnapshot) Iterator() segment.DictionaryIterator {
	return s.d.Iterator()
}

func (s *SegmentDictionarySnapshot) PrefixIterator(prefix string) segment.DictionaryIterator {
	return s.d.PrefixIterator(prefix)
}

func (s *SegmentDictionarySnapshot) RangeIterator(start, end string) segment.DictionaryIterator {
	return s.d.RangeIterator(start, end)
}

type SegmentSnapshot struct {
	id      uint64
	segment segment.Segment
	deleted *roaring.Bitmap

	notify     []chan error
	cachedDocs *cachedDocs
}

type cachedFieldDocs struct {
	readyCh chan struct{}     // closed when the cachedFieldDocs.docs is ready to be used.
	err     error             // Non-nil if there was an error when preparing this cachedFieldDocs.
	docs    map[uint64][]byte // Keyed by localDocNum, value is a list of terms delimited by 0xFF.
}

func (s *SegmentSnapshot) DocumentVisitFieldTerms(num uint64, fields []string,
	visitor index.DocumentFieldTermVisitor) error {
	collection := make(map[string][][]byte)
	// collect field indexed values
	for _, field := range fields {
		dict, err := s.Dictionary(field)
		if err != nil {
			return err
		}
		dictItr := dict.Iterator()
		var next *index.DictEntry
		next, err = dictItr.Next()
		for next != nil && err == nil {
			postings, err2 := dict.PostingsList(next.Term, nil)
			if err2 != nil {
				return err2
			}
			postingsItr := postings.Iterator()
			nextPosting, err2 := postingsItr.Next()
			for err2 == nil && nextPosting != nil && nextPosting.Number() <= num {
				if nextPosting.Number() == num {
					// got what we're looking for
					collection[field] = append(collection[field], []byte(next.Term))
				}
				nextPosting, err = postingsItr.Next()
			}
			if err2 != nil {
				return err
			}
			next, err = dictItr.Next()
		}
		if err != nil {
			return err
		}
	}
	// invoke callback
	for field, values := range collection {
		for _, value := range values {
			visitor(field, value)
		}
	}
	return nil
}

func (s *SegmentSnapshot) Count() uint64 {

	rv := s.segment.Count()
	if s.deleted != nil {
		rv -= s.deleted.GetCardinality()
	}
	return rv
}

func (s *SegmentSnapshot) Dictionary(field string) (segment.TermDictionary, error) {
	d, err := s.segment.Dictionary(field)
	if err != nil {
		return nil, err
	}
	return &SegmentDictionarySnapshot{
		s: s,
		d: d,
	}, nil
}

func (s *SegmentSnapshot) DocNumbers(docIDs []string) (*roaring.Bitmap, error) {
	rv, err := s.segment.DocNumbers(docIDs)
	if err != nil {
		return nil, err
	}
	if s.deleted != nil {
		rv.AndNot(s.deleted)
	}
	return rv, nil
}

// DocNumbersLive returns bitsit containing doc numbers for all live docs
func (s *SegmentSnapshot) DocNumbersLive() *roaring.Bitmap {
	rv := roaring.NewBitmap()
	rv.AddRange(0, s.segment.Count())
	if s.deleted != nil {
		rv.AndNot(s.deleted)
	}
	return rv
}

func (s *SegmentSnapshot) Fields() []string {
	return s.segment.Fields()
}

func (cfd *cachedFieldDocs) prepareFields(docNum uint64, field string,
	ss *SegmentSnapshot) error {
	defer close(cfd.readyCh)

	dict, err := ss.segment.Dictionary(field)
	if err != nil {
		cfd.err = err
		return err
	}

	dictItr := dict.Iterator()
	next, err := dictItr.Next()
	for next != nil && err == nil {
		postings, err1 := dict.PostingsList(next.Term, nil)
		if err1 != nil {
			cfd.err = err1
			next, err = dictItr.Next()
			continue
		}

		postingsItr := postings.Iterator()
		nextPosting, err2 := postingsItr.Next()
		for err2 == nil && nextPosting != nil && nextPosting.Number() <= docNum-1 {
			if nextPosting.Number() == docNum-1 {
				// got what we're looking for
				termBytes := append([]byte(next.Term), ByteSeparator)
				cfd.docs[docNum] = append(cfd.docs[docNum], termBytes...)
			}
			nextPosting, err2 = postingsItr.Next()
		}

		if err2 != nil {
			cfd.err = err2
			next, err = dictItr.Next()
			continue
		}

		next, err = dictItr.Next()
	}
	return nil
}

type cachedDocs struct {
	m     sync.Mutex                  // As the cache is asynchronously prepared, need a lock
	cache map[string]*cachedFieldDocs // Keyed by field
}

func (c *cachedDocs) prepareFields(docNum uint64, wantedFields []string,
	ss *SegmentSnapshot) error {
	c.m.Lock()
	if c.cache == nil {
		c.cache = make(map[string]*cachedFieldDocs, len(ss.Fields()))
	}

	for _, field := range wantedFields {
		_, exists := c.cache[field]
		if !exists {
			c.cache[field] = &cachedFieldDocs{
				readyCh: make(chan struct{}),
				docs:    make(map[uint64][]byte),
			}

			go c.cache[field].prepareFields(docNum, field, ss)
		}
	}

	for _, field := range wantedFields {
		cachedFieldDocs := c.cache[field]
		c.m.Unlock()
		<-cachedFieldDocs.readyCh

		if cachedFieldDocs.err != nil {
			return cachedFieldDocs.err
		}
		c.m.Lock()
	}

	c.m.Unlock()
	return nil
}

