/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package mirbft

// This stateless file contains stateless functions leveraged in assorted pieces of the code.

import (
	"bytes"
	"fmt"

	pb "github.com/IBM/mirbft/mirbftpb"
)

// intersectionQuorum is the number of nodes required to agree
// such that any two sets intersected will each contain some same
// correct node.  This is ceil((n+f+1)/2), which is equivalent to
// (n+f+2)/2 under truncating integer math.
func intersectionQuorum(nc *pb.NetworkState_Config) int {
	return (len(nc.Nodes) + int(nc.F) + 2) / 2
}

// someCorrectQuorum is the number of nodes such that at least one of them is correct
func someCorrectQuorum(nc *pb.NetworkState_Config) int {
	return int(nc.F) + 1
}

// logWidth is the number of sequence numbers in the sliding window
func logWidth(nc *pb.NetworkState_Config) int {
	return 3 * int(nc.CheckpointInterval)
}

func initialSequence(epochConfig *pb.EpochConfig, networkConfig *pb.NetworkState_Config) uint64 {
	if epochConfig.PlannedExpiration > networkConfig.MaxEpochLength {
		return epochConfig.PlannedExpiration - networkConfig.MaxEpochLength + 1
	}
	return 1
}

func seqToBucket(seqNo uint64, ec *pb.EpochConfig, nc *pb.NetworkState_Config) BucketID {
	return BucketID((seqNo - initialSequence(ec, nc)) % uint64(nc.NumberOfBuckets))
}

func (e *epoch) seqToBucket(seqNo uint64) BucketID {
	return seqToBucket(seqNo, e.epochConfig, e.networkConfig)
}

func seqToColumn(seqNo uint64, ec *pb.EpochConfig, nc *pb.NetworkState_Config) uint64 {
	return (seqNo-initialSequence(ec, nc))/uint64(nc.NumberOfBuckets) + 1
}

func constructNewEpochConfig(config *pb.NetworkState_Config, newLeaders []uint64, epochChanges map[NodeID]*parsedEpochChange) *pb.NewEpochConfig {
	type checkpointKey struct {
		SeqNo uint64
		Value string
	}

	checkpoints := map[checkpointKey][]NodeID{}

	var newEpochNumber uint64 // TODO this is super-hacky

	for nodeID, epochChange := range epochChanges {
		newEpochNumber = epochChange.underlying.NewEpoch
		for _, checkpoint := range epochChange.underlying.Checkpoints {

			key := checkpointKey{
				SeqNo: checkpoint.SeqNo,
				Value: string(checkpoint.Value),
			}

			checkpoints[key] = append(checkpoints[key], nodeID)
		}
	}

	var maxCheckpoint *checkpointKey

	for key, supporters := range checkpoints {
		key := key // shadow for when we take the pointer
		if len(supporters) < someCorrectQuorum(config) {
			continue
		}

		nodesWithLowerWatermark := 0
		for _, epochChange := range epochChanges {
			if epochChange.lowWatermark <= key.SeqNo {
				nodesWithLowerWatermark++
			}
		}

		if nodesWithLowerWatermark < intersectionQuorum(config) {
			continue
		}

		if maxCheckpoint == nil {
			maxCheckpoint = &key
			continue
		}

		if maxCheckpoint.SeqNo > key.SeqNo {
			continue
		}

		if maxCheckpoint.SeqNo == key.SeqNo {
			panic(fmt.Sprintf("two correct quorums have different checkpoints for same seqno %d -- %x != %x", key.SeqNo, []byte(maxCheckpoint.Value), []byte(key.Value)))
		}

		maxCheckpoint = &key
	}

	if maxCheckpoint == nil {
		return nil
	}

	newEpochConfig := &pb.NewEpochConfig{
		Config: &pb.EpochConfig{
			Number:            newEpochNumber,
			Leaders:           newLeaders,
			PlannedExpiration: maxCheckpoint.SeqNo + config.MaxEpochLength,
		},
		StartingCheckpoint: &pb.Checkpoint{
			SeqNo: maxCheckpoint.SeqNo,
			Value: []byte(maxCheckpoint.Value),
		},
		FinalPreprepares: make([][]byte, 2*config.CheckpointInterval),
	}

	anySelected := false

	for seqNoOffset := range newEpochConfig.FinalPreprepares {
		seqNo := uint64(seqNoOffset) + maxCheckpoint.SeqNo + 1

		var selectedEntry *pb.EpochChange_SetEntry

		for _, nodeID := range config.Nodes {
			nodeID := NodeID(nodeID)
			// Note, it looks like we're re-implementing `range epochChanges` here,
			// and we are, but doing so in a deterministic order.

			epochChange, ok := epochChanges[nodeID]
			if !ok {
				continue
			}

			entry, ok := epochChange.pSet[seqNo]
			if !ok {
				continue
			}

			a1Count := 0
			for _, iEpochChange := range epochChanges {
				if iEpochChange.lowWatermark >= seqNo {
					continue
				}

				iEntry, ok := iEpochChange.pSet[seqNo]
				if !ok || iEntry.Epoch < entry.Epoch {
					a1Count++
					continue
				}

				if iEntry.Epoch > entry.Epoch {
					continue
				}

				// Thus, iEntry.Epoch == entry.Epoch

				if bytes.Equal(entry.Digest, iEntry.Digest) {
					a1Count++
				}
			}

			if a1Count < intersectionQuorum(config) {
				continue
			}

			a2Count := 0
			for _, iEpochChange := range epochChanges {
				epochEntries, ok := iEpochChange.qSet[seqNo]
				if !ok {
					continue
				}

				for epoch, digest := range epochEntries {
					if epoch < entry.Epoch {
						continue
					}

					if !bytes.Equal(entry.Digest, digest) {
						continue
					}

					a2Count++
					break
				}
			}

			if a2Count < someCorrectQuorum(config) {
				continue
			}

			selectedEntry = entry
			break
		}

		if selectedEntry != nil {
			newEpochConfig.FinalPreprepares[seqNoOffset] = selectedEntry.Digest
			anySelected = true
			continue
		}

		bCount := 0
		for _, epochChange := range epochChanges {
			if epochChange.lowWatermark >= seqNo {
				continue
			}

			if _, ok := epochChange.pSet[seqNo]; !ok {
				bCount++
			}
		}

		if bCount < intersectionQuorum(config) {
			// We could not satisfy condition A, or B, we need to wait
			return nil
		}
	}

	if !anySelected {
		newEpochConfig.FinalPreprepares = nil
	}

	return newEpochConfig
}

func epochChangeHashData(epochChange *pb.EpochChange) [][]byte {
	// [new_epoch, checkpoints, pSet, qSet]
	hashData := make([][]byte, 1+len(epochChange.Checkpoints)*2+len(epochChange.PSet)*3+len(epochChange.QSet)*3)
	hashData[0] = uint64ToBytes(epochChange.NewEpoch)

	cpOffset := 1
	for i, cp := range epochChange.Checkpoints {
		hashData[cpOffset+2*i] = uint64ToBytes(cp.SeqNo)
		hashData[cpOffset+2*i+1] = cp.Value
	}

	pEntryOffset := cpOffset + len(epochChange.Checkpoints)*2
	for i, pEntry := range epochChange.PSet {
		hashData[pEntryOffset+3*i] = uint64ToBytes(pEntry.Epoch)
		hashData[pEntryOffset+3*i+1] = uint64ToBytes(pEntry.SeqNo)
		hashData[pEntryOffset+3*i+2] = pEntry.Digest
	}

	qEntryOffset := pEntryOffset + len(epochChange.PSet)*3
	for i, qEntry := range epochChange.QSet {
		hashData[qEntryOffset+3*i] = uint64ToBytes(qEntry.Epoch)
		hashData[qEntryOffset+3*i+1] = uint64ToBytes(qEntry.SeqNo)
		hashData[qEntryOffset+3*i+2] = qEntry.Digest
	}

	if qEntryOffset+len(epochChange.QSet)*3 != len(hashData) {
		panic("TODO, remove me, but this is bad")
	}

	return hashData
}
