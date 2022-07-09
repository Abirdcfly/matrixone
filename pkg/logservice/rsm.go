// Copyright 2021 - 2022 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package logservice

import (
	"encoding/binary"
	"encoding/gob"
	"io"

	sm "github.com/lni/dragonboat/v4/statemachine"
	pb "github.com/matrixorigin/matrixone/pkg/pb/logservice"
)

var (
	binaryEnc = binary.BigEndian
)

const (
	firstLogShardID uint64 = 1
	headerSize             = 2
)

const (
	leaseHolderIDTag uint16 = iota + 0xBF01
	truncatedIndexTag
	userEntryTag
	indexTag
	tsoTag
)

// used to indicate query types
type leaseHolderIDQuery struct{}
type indexQuery struct{}
type truncatedIndexQuery struct{}
type leaseHistoryQuery struct{ index uint64 }

func getAppendCmd(cmd []byte, replicaID uint64) []byte {
	if len(cmd) < headerSize+8 {
		panic("cmd too small")
	}
	binaryEnc.PutUint16(cmd, userEntryTag)
	binaryEnc.PutUint64(cmd[headerSize:], replicaID)
	return cmd
}

func parseCmdTag(cmd []byte) uint16 {
	return binaryEnc.Uint16(cmd)
}

func parseTruncatedIndex(cmd []byte) uint64 {
	return binaryEnc.Uint64(cmd[headerSize:])
}

func parseLeaseHolderID(cmd []byte) uint64 {
	return binaryEnc.Uint64(cmd[headerSize:])
}

func parseTsoUpdateCmd(cmd []byte) uint64 {
	return binaryEnc.Uint64(cmd[headerSize:])
}

func getSetLeaseHolderCmd(leaseHolderID uint64) []byte {
	cmd := make([]byte, headerSize+8)
	binaryEnc.PutUint16(cmd, leaseHolderIDTag)
	binaryEnc.PutUint64(cmd[headerSize:], leaseHolderID)
	return cmd
}

func getSetTruncatedIndexCmd(index uint64) []byte {
	cmd := make([]byte, headerSize+8)
	binaryEnc.PutUint16(cmd, truncatedIndexTag)
	binaryEnc.PutUint64(cmd[headerSize:], index)
	return cmd
}

func getTsoUpdateCmd(count uint64) []byte {
	cmd := make([]byte, headerSize+8)
	binaryEnc.PutUint16(cmd, tsoTag)
	binaryEnc.PutUint64(cmd[headerSize:], count)
	return cmd
}

func isTsoUpdate(cmd []byte) bool {
	if len(cmd) != headerSize+8 {
		return false
	}
	return parseCmdTag(cmd) == tsoTag
}

func isSetLeaseHolderUpdate(cmd []byte) bool {
	return tagMatch(cmd, leaseHolderIDTag)
}

func isSetTruncatedIndexUpdate(cmd []byte) bool {
	return tagMatch(cmd, truncatedIndexTag)
}

func isUserUpdate(cmd []byte) bool {
	if len(cmd) < headerSize+8 {
		return false
	}
	return parseCmdTag(cmd) == userEntryTag
}

func tagMatch(cmd []byte, expectedTag uint16) bool {
	if len(cmd) != headerSize+8 {
		return false
	}
	return parseCmdTag(cmd) == expectedTag
}

type stateMachine struct {
	shardID   uint64
	replicaID uint64
	state     pb.RSMState
}

var _ (sm.IStateMachine) = (*stateMachine)(nil)

func newStateMachine(shardID uint64, replicaID uint64) sm.IStateMachine {
	state := pb.RSMState{
		Tso:          1,
		LeaseHistory: make(map[uint64]uint64),
	}
	return &stateMachine{
		shardID:   shardID,
		replicaID: replicaID,
		state:     state,
	}
}

func (s *stateMachine) truncateLeaseHistory(index uint64) {
	_, index = s.getLeaseHistory(index)
	for key := range s.state.LeaseHistory {
		if key < index {
			delete(s.state.LeaseHistory, key)
		}
	}
}

func (s *stateMachine) getLeaseHistory(index uint64) (uint64, uint64) {
	max := uint64(0)
	lease := uint64(0)
	for key, val := range s.state.LeaseHistory {
		if key >= index {
			continue
		}
		if key > max {
			max = key
			lease = val
		}
	}
	return lease, max
}

func (s *stateMachine) handleSetLeaseHolderID(cmd []byte) sm.Result {
	s.state.LeaseHolderID = parseLeaseHolderID(cmd)
	s.state.LeaseHistory[s.state.Index] = s.state.LeaseHolderID
	return sm.Result{}
}

func (s *stateMachine) handleTruncateIndex(cmd []byte) sm.Result {
	index := parseTruncatedIndex(cmd)
	if index > s.state.TruncatedIndex {
		s.state.TruncatedIndex = index
		s.truncateLeaseHistory(index)
		return sm.Result{}
	}
	return sm.Result{Value: s.state.TruncatedIndex}
}

// handleUserUpdate returns an empty sm.Result on success or it returns a
// sm.Result value with the Value field set to the current lease holder ID
// to indicate rejection by mismatched lease holder ID.
func (s *stateMachine) handleUserUpdate(cmd []byte) sm.Result {
	if s.state.LeaseHolderID != parseLeaseHolderID(cmd) {
		data := make([]byte, 8)
		binaryEnc.PutUint64(data, s.state.LeaseHolderID)
		return sm.Result{Data: data}
	}
	return sm.Result{Value: s.state.Index}
}

func (s *stateMachine) handleTsoUpdate(cmd []byte) sm.Result {
	count := parseTsoUpdateCmd(cmd)
	result := sm.Result{Value: s.state.Tso}
	s.state.Tso += count
	return result
}

func (s *stateMachine) Close() error {
	return nil
}

func (s *stateMachine) Update(e sm.Entry) (sm.Result, error) {
	cmd := e.Cmd
	s.state.Index = e.Index
	if isSetLeaseHolderUpdate(cmd) {
		return s.handleSetLeaseHolderID(cmd), nil
	} else if isSetTruncatedIndexUpdate(cmd) {
		return s.handleTruncateIndex(cmd), nil
	} else if isUserUpdate(cmd) {
		return s.handleUserUpdate(cmd), nil
	} else if isTsoUpdate(cmd) {
		return s.handleTsoUpdate(cmd), nil
	}
	panic("corrupted entry")
}

func (s *stateMachine) Lookup(query interface{}) (interface{}, error) {
	if _, ok := query.(indexQuery); ok {
		return s.state.Index, nil
	} else if _, ok := query.(leaseHolderIDQuery); ok {
		return s.state.LeaseHolderID, nil
	} else if _, ok := query.(truncatedIndexQuery); ok {
		return s.state.TruncatedIndex, nil
	} else if v, ok := query.(leaseHistoryQuery); ok {
		lease, _ := s.getLeaseHistory(v.index)
		return lease, nil
	}
	panic("unknown lookup command type")
}

func (s *stateMachine) SaveSnapshot(w io.Writer,
	_ sm.ISnapshotFileCollection, _ <-chan struct{}) error {
	// FIXME: use gogoproto to marshal the state, need to figure out how to
	// marshal to a io.Writer
	enc := gob.NewEncoder(w)
	return enc.Encode(s.state)
}

func (s *stateMachine) RecoverFromSnapshot(r io.Reader,
	_ []sm.SnapshotFile, _ <-chan struct{}) error {
	dec := gob.NewDecoder(r)
	return dec.Decode(&s.state)
}
