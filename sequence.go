/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package mirbft

import (
	pb "github.com/IBM/mirbft/mirbftpb"
	"go.uber.org/zap"
)

type sequenceState int

const (
	sequenceUninitialized sequenceState = iota
	sequenceAllocated
	sequencePendingRequests
	sequenceReady
	sequencePreprepared
	sequencePrepared
	sequenceCommitted
)

type sequence struct {
	owner nodeID
	seqNo uint64
	epoch uint64

	myConfig      *pb.StateEvent_InitialParameters
	logger        Logger
	networkConfig *pb.NetworkState_Config

	state sequenceState

	persisted *persisted

	// qEntry is unset until after state >= sequencePreprepared
	qEntry *pb.QEntry

	// batch is not set until after state >= sequenceAllocated
	batch []*pb.RequestAck

	// outstandingReqs is not set until after state >= sequenceAllocated and may never be set
	outstandingReqs map[string]struct{}

	// digest is the computed digest of the batch, may not be set until state > sequenceReady
	digest []byte

	prepares map[string]map[nodeID]struct{}
	commits  map[string]map[nodeID]struct{}
}

func newSequence(owner nodeID, epoch, seqNo uint64, persisted *persisted, networkConfig *pb.NetworkState_Config, myConfig *pb.StateEvent_InitialParameters, logger Logger) *sequence {
	return &sequence{
		owner:         owner,
		seqNo:         seqNo,
		epoch:         epoch,
		myConfig:      myConfig,
		logger:        logger,
		networkConfig: networkConfig,
		persisted:     persisted,
		state:         sequenceUninitialized,
		prepares:      map[string]map[nodeID]struct{}{},
		commits:       map[string]map[nodeID]struct{}{},
	}
}

func (s *sequence) advanceState() *Actions {
	actions := &Actions{}
	for {
		oldState := s.state
		switch s.state {
		case sequenceUninitialized:
		case sequenceAllocated:
		case sequencePendingRequests:
			s.checkRequests()
		case sequenceReady:
			if s.digest != nil || len(s.batch) == 0 {
				actions.concat(s.prepare())
			}
		case sequencePreprepared:
			actions.concat(s.checkPrepareQuorum())
		case sequencePrepared:
			s.checkCommitQuorum()
		case sequenceCommitted:
		}
		if s.state == oldState {
			return actions
		}
	}
}

func (s *sequence) allocateAsOwner(clientRequests []*clientRequest) *Actions {
	requestAcks := make([]*pb.RequestAck, len(clientRequests))
	for i, clientRequest := range clientRequests {
		requestAcks[i] = clientRequest.ack
	}

	// TODO, hold onto the clientRequests so that we know who to forward to

	return s.allocate(requestAcks, nil)
}

// allocate reserves this sequence in this epoch for a set of requests.
// If the state machine is not in the uninitialized state, it returns an error.  Otherwise,
// It transitions to preprepared and returns a ValidationRequest message.
func (s *sequence) allocate(requestAcks []*pb.RequestAck, outstandingReqs map[string]struct{}) *Actions {
	if s.state != sequenceUninitialized {
		s.logger.Panic("illegal state for allocate", zap.Int("State", int(s.state)), zap.Uint64("SeqNo", s.seqNo), zap.Uint64("Epoch", s.epoch))
	}

	s.state = sequenceAllocated
	s.batch = requestAcks
	s.outstandingReqs = outstandingReqs

	if len(requestAcks) == 0 {
		// This is a no-op batch, no need to compute a digest
		s.state = sequenceReady
		return s.applyBatchHashResult(nil)
	}

	data := make([][]byte, len(requestAcks))
	for i, ack := range requestAcks {
		data[i] = ack.Digest
	}

	actions := &Actions{
		Hash: []*HashRequest{
			{
				Data: data,

				Origin: &pb.HashResult{
					Type: &pb.HashResult_Batch_{
						Batch: &pb.HashResult_Batch{
							Source:      uint64(s.owner),
							SeqNo:       s.seqNo,
							Epoch:       s.epoch,
							RequestAcks: requestAcks,
						},
					},
				},
			},
		},
	}

	s.state = sequencePendingRequests

	return actions.concat(s.advanceState())
}

func (s *sequence) satisfyOutstanding(fr *pb.RequestAck) *Actions {
	_, ok := s.outstandingReqs[string(fr.Digest)]
	if !ok {
		panic("dev sanity check")
	}

	delete(s.outstandingReqs, string(fr.Digest))

	return s.advanceState()
}

func (s *sequence) checkRequests() {
	if len(s.outstandingReqs) > 0 {
		return
	}

	s.state = sequenceReady
}

func (s *sequence) applyBatchHashResult(digest []byte) *Actions {

	s.digest = digest

	return s.applyPrepareMsg(s.owner, digest)
}

func (s *sequence) prepare() *Actions {
	s.qEntry = &pb.QEntry{
		SeqNo:    s.seqNo,
		Digest:   s.digest,
		Requests: s.batch,
	}

	s.state = sequencePreprepared

	actions := &Actions{}

	if uint64(s.owner) == s.myConfig.Id {
		for _, fr := range s.batch {
			actions.forwardRequest(
				s.networkConfig.Nodes,
				fr,
			)
		}
		actions.send(
			s.networkConfig.Nodes,
			&pb.Msg{
				Type: &pb.Msg_Preprepare{
					Preprepare: &pb.Preprepare{
						SeqNo: s.seqNo,
						Epoch: s.epoch,
						Batch: s.batch,
					},
				},
			},
		)
	} else {
		actions.send(
			s.networkConfig.Nodes,
			&pb.Msg{
				Type: &pb.Msg_Prepare{
					Prepare: &pb.Prepare{
						SeqNo:  s.seqNo,
						Epoch:  s.epoch,
						Digest: s.digest,
					},
				},
			},
		)
	}

	return actions.concat(s.persisted.addQEntry(s.qEntry))
}

func (s *sequence) applyPrepareMsg(source nodeID, digest []byte) *Actions {
	// TODO, if the digest is known, mark a mismatch as oddity
	agreements := s.prepares[string(digest)]
	if agreements == nil {
		agreements = map[nodeID]struct{}{}
		s.prepares[string(digest)] = agreements
	}
	agreements[source] = struct{}{}
	s.prepares[string(digest)] = agreements

	return s.advanceState()
}

func (s *sequence) checkPrepareQuorum() *Actions {
	agreements := s.prepares[string(s.digest)]
	// Do not prepare unless we have sent our prepare as well
	// as this ensures we've persisted our qSet
	if _, ok := agreements[nodeID(s.myConfig.Id)]; !ok {
		return &Actions{}
	}

	// We do require 2f+1 prepares (instead of 2f), as the preprepare
	// for the leader will be applied as a prepare here
	requiredPrepares := intersectionQuorum(s.networkConfig)

	if len(agreements) < requiredPrepares {
		return &Actions{}
	}

	s.state = sequencePrepared

	pEntry := &pb.PEntry{
		SeqNo:  s.seqNo,
		Digest: s.digest,
	}

	actions := (&Actions{}).send(
		s.networkConfig.Nodes,
		&pb.Msg{
			Type: &pb.Msg_Commit{
				Commit: &pb.Commit{
					SeqNo:  s.seqNo,
					Epoch:  s.epoch,
					Digest: s.digest,
				},
			},
		},
	)
	return actions.concat(s.persisted.addPEntry(pEntry))
}

func (s *sequence) applyCommitMsg(source nodeID, digest []byte) *Actions {
	// TODO, if the digest is known, mark a mismatch as oddity
	agreements := s.commits[string(digest)]
	if agreements == nil {
		agreements = map[nodeID]struct{}{}
		s.commits[string(digest)] = agreements
	}
	agreements[source] = struct{}{}

	return s.advanceState()
}

func (s *sequence) checkCommitQuorum() {
	agreements := s.commits[string(s.digest)]
	// Do not commit unless we have sent a commit
	// and therefore already have persisted our pSet and qSet
	if _, ok := agreements[nodeID(s.myConfig.Id)]; !ok {
		return
	}

	requiredCommits := intersectionQuorum(s.networkConfig)

	if len(agreements) < requiredCommits {
		return
	}

	s.state = sequenceCommitted
}
