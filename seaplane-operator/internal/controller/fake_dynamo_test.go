/*
Copyright 2026.

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

package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/dynamo"
)

// fakeDynamo records DynamoDB calls made by controllers.
type fakeDynamo struct {
	mu         sync.Mutex
	applyCount int
	readCount  int
	applies    []*dynamo.ApplyDesire
	reads      []*dynamo.ReadDesire

	// Set readStatus to return a specific status from GetReadDesireStatus.
	// Per-docID maps take precedence over the single-value fields.
	readStatus *dynamo.ReadDesireStatus
	// Set applyStatus to return a specific status from GetApplyDesireStatus.
	applyStatus *dynamo.ApplyDesireStatus

	// Per-docID status maps — when set, these take precedence over the
	// single-value fields above, enabling tests to return different statuses
	// for different resources (partial sync, mixed success/failure).
	applyStatuses map[string]*dynamo.ApplyDesireStatus
	readStatuses  map[string]*dynamo.ReadDesireStatus

	// deletedSpecs tracks calls to DeleteDesireSpec (suffix/docID).
	deletedSpecs []string
	// lastPutTime records when the last PutApplyDesire was called, mirroring
	// the real DynamoDB updateTime that kube-applier copies into
	// ObservedDesireUpdateTime.
	lastPutTime time.Time
	// Set applyErr to make UpsertApplyDesire return an error.
	applyErr error
	// Set readErr to make UpsertReadDesire return an error.
	readErr error
	// Per-docID error maps for Get*Status methods.
	applyStatusErrors map[string]error
	readStatusErrors  map[string]error
}

var _ dynamo.DesireClient = (*fakeDynamo)(nil)

func (f *fakeDynamo) UpsertApplyDesire(_ context.Context, _ string, desire *dynamo.ApplyDesire) (dynamo.UpsertResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applyErr != nil {
		return dynamo.UpsertResult{}, f.applyErr
	}
	f.applyCount++
	f.applies = append(f.applies, desire)
	f.lastPutTime = time.Now().UTC().Truncate(time.Second)
	return dynamo.UpsertResult{Changed: true, UpdateTime: f.lastPutTime}, nil
}

func (f *fakeDynamo) UpsertReadDesire(_ context.Context, _ string, desire *dynamo.ReadDesire) (dynamo.UpsertResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return dynamo.UpsertResult{}, f.readErr
	}
	f.readCount++
	f.reads = append(f.reads, desire)
	return dynamo.UpsertResult{Changed: true, UpdateTime: time.Now().UTC().Truncate(time.Second)}, nil
}

func (f *fakeDynamo) GetApplyDesireStatus(_ context.Context, _, docID string) (*dynamo.ApplyDesireStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.applyStatusErrors[docID]; ok {
		return nil, e
	}
	if s, ok := f.applyStatuses[docID]; ok {
		cp := *s
		if cp.ObservedDesireUpdateTime.IsZero() && !f.lastPutTime.IsZero() {
			cp.ObservedDesireUpdateTime = f.lastPutTime
		}
		return &cp, nil
	}
	if f.applyStatus != nil {
		s := *f.applyStatus
		if s.ObservedDesireUpdateTime.IsZero() && !f.lastPutTime.IsZero() {
			s.ObservedDesireUpdateTime = f.lastPutTime
		}
		return &s, nil
	}
	return nil, fmt.Errorf("%w: fake", dynamo.ErrNotFound)
}

func (f *fakeDynamo) GetReadDesireStatus(_ context.Context, _, docID string) (*dynamo.ReadDesireStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.readStatusErrors[docID]; ok {
		return nil, e
	}
	if s, ok := f.readStatuses[docID]; ok {
		return s, nil
	}
	if f.readStatus != nil {
		return f.readStatus, nil
	}
	return nil, fmt.Errorf("%w: fake", dynamo.ErrNotFound)
}

func (f *fakeDynamo) DeleteDesireSpec(_ context.Context, _, suffix, docID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedSpecs = append(f.deletedSpecs, suffix+"/"+docID)
	return nil
}

// countSpecCleanups counts ApplyDesire and ReadDesire spec cleanups in deletedSpecs.
func (f *fakeDynamo) countSpecCleanups() (apply, read int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, spec := range f.deletedSpecs {
		if strings.Contains(spec, "-applydesires") {
			apply++
		}
		if strings.Contains(spec, "-readdesires") {
			read++
		}
	}
	return
}
