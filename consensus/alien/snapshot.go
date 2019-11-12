// Copyright 2018 The giov Authors
// This file is part of the giov library.
//
// The giov library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The giov library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the giov library. If not, see <http://www.gnu.org/licenses/>.

// Package alien implements the delegated-proof-of-stake consensus engine.

package alien

import (
	"encoding/json"
	"errors"
	"github.com/CarLiveChainCo/goiov/common"
	"github.com/CarLiveChainCo/goiov/core/types"
	"github.com/CarLiveChainCo/goiov/ethdb"
	"github.com/CarLiveChainCo/goiov/log"
	"github.com/CarLiveChainCo/goiov/params"
	"github.com/CarLiveChainCo/goiov/rlp"
	"github.com/hashicorp/golang-lru"
	"math/big"
	"sort"
	"time"
)

const (
	defaultFullCredit               = 1000 // no punished
	missingPublishCredit            = 100  // punished for missing one block seal
	signRewardCredit                = 100   // seal one block
	autoRewardCredit                = 1    // credit auto recover for each block
	minCalSignerQueueCredit         = 300  // when calculate the signerQueue
	defaultOfficialMaxSignerCount   = 21   // official max signer count
	defaultOfficialFirstLevelCount  = 10   // official first level , 100% in signer queue
	defaultOfficialSecondLevelCount = 20   // official second level, 60% in signer queue
	defaultOfficialThirdLevelCount  = 30   // official third level, 40% in signer queue
	// the credit of one signer is at least minCalSignerQueueCredit
	candidateStateNormal = 1
	candidateMaxLen      = 500 // if candidateNeedPD is false and candidate is more than candidateMaxLen, then minimum tickets candidates will be remove in each LCRS*loop

)

var (
	errIncorrectTallyCount = errors.New("incorrect tally count")
)

// Snapshot is the state of the authorization voting at a given point in time.
type Snapshot struct {
	config   *params.AlienConfig // Consensus engine parameters to fine tune behavior
	sigcache *lru.ARCCache       // Cache of recent block signatures to speed up ecrecover
	LCRS     uint64              // Loop count to recreate signers from top tally

	Period          uint64                       `json:"period"`          // Period of seal each block
	Number          uint64                       `json:"number"`          // Block number where the snapshot was created
	ConfirmedNumber uint64                       `json:"confirmedNumber"` // Block number confirmed when the snapshot was created
	Hash            common.Hash                  `json:"hash"`            // Block hash where the snapshot was created
	HistoryHash     []common.Hash                `json:"historyHash"`     // Block hash list for two recent loop
	Signers         []*common.Address            `json:"signers"`         // Signers queue in current header
	Votes           map[common.Address]*Vote     `json:"votes"`           // All validate votes from genesis block
	Tally           map[common.Address]*big.Int  `json:"tally"`           // Stake for each candidate address
	Voters          map[common.Address]*big.Int  `json:"voters"`          // Block number for each voter address
	Cancels         map[common.Address]*Cancel   `json:"cancels"`         // All cancels
	Cancelers       map[common.Address]*big.Int  `json:"cancelers"`       // Block number for each canceler address
	Candidates      map[common.Address][]*Vote   `json:"candidates"`      		  // all votes for candidates, used for private
	Punished        map[common.Address]uint64    `json:"punished"`        // The signer be punished count cause of missing seal
	Confirmations   map[uint64][]*common.Address `json:"confirms"`        // The signer confirm given block number
	HeaderTime      uint64                       `json:"headerTime"`      // Time of the current header
	LoopStartTime   uint64                       `json:"loopStartTime"`   // Start Time of the current loop
	Backup1         []byte
	Backup2         []byte
}

// newSnapshot creates a new snapshot with the specified startup parameters. only ever use if for
// the genesis block.
func newSnapshot(config *params.AlienConfig, sigcache *lru.ARCCache, hash common.Hash, votes []*Vote, lcrs uint64) *Snapshot {

	snap := &Snapshot{
		config:          config,
		sigcache:        sigcache,
		LCRS:            lcrs,
		Period:          config.Period,
		Number:          0,
		ConfirmedNumber: 0,
		Hash:            hash,
		HistoryHash:     []common.Hash{},
		Signers:         []*common.Address{},
		Votes:           make(map[common.Address]*Vote),
		Tally:           make(map[common.Address]*big.Int),
		Voters:          make(map[common.Address]*big.Int),
		Cancels:         make(map[common.Address]*Cancel),
		Cancelers:       make(map[common.Address]*big.Int),
		Punished:        make(map[common.Address]uint64),
		Candidates:      make(map[common.Address][]*Vote),
		Confirmations:   make(map[uint64][]*common.Address),
		HeaderTime:      uint64(time.Now().Unix()) - 1,
		LoopStartTime:   config.GenesisTimestamp,
		Backup1: 		 []byte{},
		Backup2: 		 []byte{},
	}
	snap.HistoryHash = append(snap.HistoryHash, hash)

	for _, vote := range votes {
		// init Votes from each vote
		snap.Votes[vote.Voter] = vote
		// init Tally
		_, ok := snap.Tally[vote.Candidate]
		if !ok {
			snap.Tally[vote.Candidate] = big.NewInt(0)
		}
		snap.Tally[vote.Candidate].Add(snap.Tally[vote.Candidate], vote.Stake)
		// init Voters
		snap.Voters[vote.Voter] = big.NewInt(0) // block number is 0 , vote in genesis block
		// init Candidates
		snap.Candidates[vote.Voter] = append(snap.Candidates[vote.Voter], vote)
	}

	for i := 0; i < int(config.MaxSignerCount); i++ {
		snap.Signers = append(snap.Signers, &config.SelfVoteSigners[i%len(config.SelfVoteSigners)])
	}

	return snap
}

// loadSnapshot loads an existing snapshot from the database.
func loadSnapshot(config *params.AlienConfig, sigcache *lru.ARCCache, db ethdb.Database, hash common.Hash) (*Snapshot, error) {
	blob, err := db.Get(append([]byte("alien-"), hash[:]...))
	if err != nil {
		return nil, err
	}
	snap := new(Snapshot)
	if err := json.Unmarshal(blob, snap); err != nil {
		return nil, err
	}
	snap.config = config
	snap.sigcache = sigcache
	return snap, nil
}

// store inserts the snapshot into the database.
func (s *Snapshot) store(db ethdb.Database) error {
	blob, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return db.Put(append([]byte("alien-"), s.Hash[:]...), blob)
}

// copy creates a deep copy of the snapshot, though not the individual votes.
func (s *Snapshot) copy() *Snapshot {
	cpy := &Snapshot{
		config:          s.config,
		sigcache:        s.sigcache,
		LCRS:            s.LCRS,
		Period:          s.Period,
		Number:          s.Number,
		ConfirmedNumber: s.ConfirmedNumber,
		Hash:            s.Hash,
		HistoryHash:     make([]common.Hash, len(s.HistoryHash)),

		Signers:       make([]*common.Address, len(s.Signers)),
		Votes:         make(map[common.Address]*Vote),
		Tally:         make(map[common.Address]*big.Int),
		Voters:        make(map[common.Address]*big.Int),
		Cancels:       make(map[common.Address]*Cancel),
		Cancelers:     make(map[common.Address]*big.Int),
		Candidates:    make(map[common.Address][]*Vote),
		Punished:      make(map[common.Address]uint64),
		Confirmations: make(map[uint64][]*common.Address),

		HeaderTime:    s.HeaderTime,
		LoopStartTime: s.LoopStartTime,
		Backup1: 		make([]byte, len(s.Backup1)),
		Backup2: 		make([]byte, len(s.Backup2)),
	}
	copy(cpy.HistoryHash, s.HistoryHash)
	copy(cpy.Signers, s.Signers)
	copy(cpy.Backup1, s.Backup1)
	copy(cpy.Backup2, s.Backup2)

	for voter, vote := range s.Votes {
		cpy.Votes[voter] = &Vote{
			Voter:     vote.Voter,
			Candidate: vote.Candidate,
			Stake:     new(big.Int).Set(vote.Stake),
			Hash:      vote.Hash,
		}
	}
	for canceler, cancel := range s.Cancels {
		cpy.Cancels[canceler] = &Cancel{
			Canceler: canceler,
			Passive:  cancel.Passive,
		}
	}

	for candidate, tally := range s.Tally {
		cpy.Tally[candidate] = new(big.Int).Set(tally)
	}
	for voter, number := range s.Voters {
		cpy.Voters[voter] = new(big.Int).Set(number)
	}
	for canceler, number := range s.Cancelers {
		cpy.Cancelers[canceler] = new(big.Int).Set(number)
	}
	for candidate, state := range s.Candidates {
		cpy.Candidates[candidate] = state
	}
	for signer, cnt := range s.Punished {
		cpy.Punished[signer] = cnt
	}
	for blockNumber, confirmers := range s.Confirmations {
		cpy.Confirmations[blockNumber] = make([]*common.Address, len(confirmers))
		copy(cpy.Confirmations[blockNumber], confirmers)
	}

	return cpy
}

// apply creates a new authorization snapshot by applying the given headers to
// the original one.
func (s *Snapshot) apply(headers []*types.Header) (*Snapshot, error) {
	// Allow passing in no headers for cleaner code
	if len(headers) == 0 {
		return s, nil
	}
	// Sanity check that the headers can be applied
	for i := 0; i < len(headers)-1; i++ {
		if headers[i+1].Number.Uint64() != headers[i].Number.Uint64()+1 {
			return nil, errInvalidVotingChain
		}
	}
	if headers[0].Number.Uint64() != s.Number+1 {
		return nil, errInvalidVotingChain
	}
	// Iterate through the headers and create a new snapshot
	snap := s.copy()

	for _, header := range headers {
		// Resolve the authorization key and check against signers
		coinbase, err := ecrecover(header, s.sigcache)
		if err != nil {
			return nil, err
		}

		if coinbase.Str() != header.Coinbase.Str() {
			return nil, errUnauthorized
		}

		headerExtra := HeaderExtra{}
		err = rlp.DecodeBytes(header.Extra[extraVanity:len(header.Extra)-extraSeal], &headerExtra)
		if err != nil {
			return nil, err
		}
		snap.HeaderTime = header.Time.Uint64()
		snap.LoopStartTime = headerExtra.LoopStartTime
		snap.Signers = nil
		for i := range headerExtra.SignerQueue {
			snap.Signers = append(snap.Signers, &headerExtra.SignerQueue[i])
		}

		snap.ConfirmedNumber = headerExtra.ConfirmedBlockNumber

		if len(snap.HistoryHash) >= int(s.config.MaxSignerCount)*2 {
			snap.HistoryHash = snap.HistoryHash[1 : int(s.config.MaxSignerCount)*2]
		}
		snap.HistoryHash = append(snap.HistoryHash, header.Hash())

		// deal the new confirmation in this block
		snap.updateSnapshotByConfirmations(headerExtra.CurrentBlockConfirmations)

		// deal the new vote from voter
		snap.updateSnapshotByVotes(headerExtra.CurrentBlockVotes, header.Number)

		// deal the new cancel from canceler
		snap.updateSnapshotByCancels(headerExtra.CurrentBlockCancels, header.Number)

		// deal the voter which balance modified
		//snap.updateSnapshotByMPVotes(headerExtra.ModifyPredecessorVotes)

		// deal the snap related with punished
		snap.updateSnapshotForPunish(headerExtra.SignerMissing, header.Number, header.Coinbase)

		// check the len of candidate if not candidateNeedPD
		//if (snap.Number+1)%(snap.config.MaxSignerCount*snap.LCRS) == 0 {
		//	snap.removeZeroTallyCandidate()
		//}

		snap.removeExtraVotesAndCancel()

	}
	snap.Number += uint64(len(headers))
	snap.Hash = headers[len(headers)-1].Hash()

	snap.updateSnapshotForExpired()
	err := snap.verifyTallyCnt()
	if err != nil {
		return nil, err
	}

	return snap, nil
}



func (s *Snapshot) removeExtraVotesAndCancel() {
	for canceler, cancel := range s.Cancels {
		if (cancel.Passive && (s.Number > s.Cancelers[canceler].Uint64() + 1)) ||
			!cancel.Passive && (s.Number + 1 - s.Cancelers[cancel.Canceler].Uint64() >= s.config.Freeze/s.config.Period) {
			// delete s.Candidates
			if s.isCandidate(canceler) {
				delete(s.Punished, canceler)
				delete(s.Candidates, canceler)
			} else {
				var candidate = s.Votes[canceler].Candidate
				for i := 0; i < len(s.Candidates[candidate]); i++ {
					if s.Candidates[candidate][i].Voter == canceler {
						s.Candidates[candidate] =
							append(s.Candidates[candidate][:i], s.Candidates[candidate][i + 1:]...)
					}
				}
			}
			// delete s.Votes, s.Cancels...
			delete(s.Votes, canceler)
			delete(s.Voters, canceler)
			delete(s.Cancels, canceler)
			delete(s.Cancelers, canceler)
		}
	}
}

//func (s *Snapshot) removeZeroTallyCandidate() {
//	tallySlice := s.buildTallySlice()
//	sort.Sort(TallySlice(tallySlice))
//		for _, tallySlice := range tallySlice {
//			if tallySlice.stake.Cmp(big.NewInt(0)) == 0 {
//				delete(s.Candidates, tallySlice.addr)
//			}
//		}
//}

func (s *Snapshot) removeExtraCandidate() {
	// remove minimum tickets tally beyond candidateMaxLen
	tallySlice := s.buildTallySlice()
	sort.Sort(TallySlice(tallySlice))
	if len(tallySlice) > candidateMaxLen {
		removeNeedTally := tallySlice[candidateMaxLen:]
		for _, tallySlice := range removeNeedTally {
			delete(s.Candidates, tallySlice.addr)
		}
	}
}

func (s *Snapshot) verifyTallyCnt() error {

	tallyTarget := make(map[common.Address]*big.Int)
	for _, v := range s.Votes {
		if _, ok := tallyTarget[v.Candidate]; ok {
			tallyTarget[v.Candidate].Add(tallyTarget[v.Candidate], v.Stake)
		} else {
			tallyTarget[v.Candidate] = new(big.Int).Set(v.Stake)
		}
	}
	for _, c := range s.Cancels {
		vote := s.Votes[c.Canceler]
		if _, ok := tallyTarget[vote.Candidate]; ok {
			tallyTarget[vote.Candidate].Sub(tallyTarget[vote.Candidate], vote.Stake)
		}
	}
	for address, tally := range s.Tally {
		if targetTally, ok := tallyTarget[address]; ok && targetTally.Cmp(tally) == 0 {
			continue
		} else {
			log.Info("in verifyTallyCnt", "targetTally", targetTally, "tally", tally)
			return errIncorrectTallyCount
		}
	}

	return nil
}


func (s *Snapshot) updateSnapshotForExpired() {

	//// deal the expired vote
	//var expiredVotes []*Vote
	//for voterAddress, voteNumber := range s.Voters {
	//	if s.Number-voteNumber.Uint64() > s.config.Epoch {
	//		// clear the vote
	//		if expiredVote, ok := s.Votes[voterAddress]; ok {
	//			expiredVotes = append(expiredVotes, expiredVote)
	//		}
	//	}
	//}
	//// remove expiredVotes only enough voters left
	//if uint64(len(s.Voters)-len(expiredVotes)) >= s.config.MaxSignerCount {
	//	for _, expiredVote := range expiredVotes {
	//		s.Tally[expiredVote.Candidate].Sub(s.Tally[expiredVote.Candidate], expiredVote.Stake)
	//		if s.Tally[expiredVote.Candidate].Cmp(big.NewInt(0)) == 0 {
	//			delete(s.Tally, expiredVote.Candidate)
	//		}
	//		delete(s.Votes, expiredVote.Voter)
	//		delete(s.Voters, expiredVote.Voter)
	//	}
	//}

	// deal the expired confirmation
	for blockNumber := range s.Confirmations {
		if s.Number-blockNumber > s.config.MaxSignerCount {
			delete(s.Confirmations, blockNumber)
		}
	}

	// remove 0 stake tally
	for address, tally := range s.Tally {
		if tally.Cmp(big.NewInt(0)) <= 0 {
			delete(s.Tally, address)
		}
	}
}

func (s *Snapshot) updateSnapshotByConfirmations(confirmations []Confirmation) {
	for _, confirmation := range confirmations {
		_, ok := s.Confirmations[confirmation.BlockNumber.Uint64()]
		if !ok {
			s.Confirmations[confirmation.BlockNumber.Uint64()] = []*common.Address{}
		}
		addConfirmation := true
		for _, address := range s.Confirmations[confirmation.BlockNumber.Uint64()] {
			if confirmation.Signer.Str() == address.Str() {
				addConfirmation = false
				break
			}
		}
		if addConfirmation == true {
			var confirmSigner common.Address
			confirmSigner.Set(confirmation.Signer)
			s.Confirmations[confirmation.BlockNumber.Uint64()] = append(s.Confirmations[confirmation.BlockNumber.Uint64()], &confirmSigner)
		}
	}
}

func (s *Snapshot) updateSnapshotByVotes(votes []Vote, headerNumber *big.Int) {
	for _, vote := range votes {
		// update Votes, Tally, Voters data
		if _, ok := s.Votes[vote.Voter]; ok {
			log.Warn("Repeat vote in updateSnapshotByVotes")
			continue
		}
		if !s.isCandidate(vote.Candidate) && vote.Candidate.Str() != vote.Voter.Str() {
			log.Warn("Invalid vote target")
			continue
		}
		if s.isCandidate(vote.Candidate) {
			s.Tally[vote.Candidate].Add(s.Tally[vote.Candidate], vote.Stake)
		} else {
			s.Tally[vote.Candidate] = new(big.Int).Set(vote.Stake)
		}
		s.Votes[vote.Voter] = &Vote{vote.Voter, vote.Candidate, vote.Stake, vote.Hash}
		s.Voters[vote.Voter] = new(big.Int).Set(headerNumber)
		s.Candidates[vote.Candidate] = append(s.Candidates[vote.Candidate], &Vote{vote.Voter, vote.Candidate, vote.Stake, vote.Hash})
	}
}

func (s *Snapshot) updateSnapshotByCancels(cancels []Cancel, headerNumber *big.Int) {
	for i := 0; i < len(cancels); i++ {
		if _, ok := s.Cancels[cancels[i].Canceler]; ok {
			log.Error("Repeat cancel")
			continue
		}

		if s.isCandidate(cancels[i].Canceler) {
			for _, vote := range s.Candidates[cancels[i].Canceler] {
				if vote.Voter.Str() != cancels[i].Canceler.Str() {
					cancels = append(cancels, Cancel{vote.Voter, true})
				}
			}
		}

		if vote, ok := s.Votes[cancels[i].Canceler]; ok {
			if _, ok := s.Tally[vote.Candidate]; ok {
				s.Tally[vote.Candidate].Sub(s.Tally[vote.Candidate], vote.Stake)
				s.Cancels[cancels[i].Canceler] = &Cancel{cancels[i].Canceler, cancels[i].Passive}
				s.Cancelers[cancels[i].Canceler] = headerNumber
			} else {
				log.Error("No vote for the candidate")
			}
		} else {
			log.Error("No votes for the cancler")
		}
	}
}

func (s *Snapshot) updateSnapshotByMPVotes(votes []Vote) {
	for _, txVote := range votes {

		if lastVote, ok := s.Votes[txVote.Voter]; ok {
			s.Tally[lastVote.Candidate].Sub(s.Tally[lastVote.Candidate], lastVote.Stake)
			s.Tally[lastVote.Candidate].Add(s.Tally[lastVote.Candidate], txVote.Stake)
			s.Votes[txVote.Voter] = &Vote{Voter: txVote.Voter, Candidate: lastVote.Candidate, Stake: txVote.Stake}
			// do not modify header number of snap.Voters
		}
	}
}

func (s *Snapshot) updateSnapshotForPunish(signerMissing []common.Address, headerNumber *big.Int, coinbase common.Address) {
	// set punished count to half of origin in Epoch
	/*
		if headerNumber.Uint64()%s.config.Epoch == 0 {
			for bePublished := range s.Punished {
				if count := s.Punished[bePublished] / 2; count > 0 {
					s.Punished[bePublished] = count
				} else {
					delete(s.Punished, bePublished)
				}
			}
		}
	*/
	// punish the missing signer
	for _, signerMissing := range signerMissing {
		if _, ok := s.Punished[signerMissing]; ok {
			if s.Punished[signerMissing] <= 10 * defaultFullCredit {
				s.Punished[signerMissing] += missingPublishCredit
			}
		} else {
			s.Punished[signerMissing] = missingPublishCredit
		}
	}
	// reduce the punish of sign signer
	if _, ok := s.Punished[coinbase]; ok {

		if s.Punished[coinbase] > signRewardCredit {
			s.Punished[coinbase] -= signRewardCredit
		} else {
			delete(s.Punished, coinbase)
		}
	}
	// reduce the punish for all punished
	for signerEach := range s.Punished {
		if s.Punished[signerEach] > autoRewardCredit {
			s.Punished[signerEach] -= autoRewardCredit
		} else {
			delete(s.Punished, signerEach)
		}
	}
}

// inturn returns if a signer at a given block height is in-turn or not.
func (s *Snapshot) inturn(signer common.Address, header *types.Header) bool {
	if header.Coinbase != signer {
		return false
	}
	if signersCount := len(s.Signers); signersCount > 0 {
		if loopIndex := ((header.Time.Uint64() - s.LoopStartTime) / s.config.Period) % uint64(signersCount); *s.Signers[loopIndex] == signer {
			return true
		}
	}
	return false
}

// check if address belong to voter
func (s *Snapshot) isVoter(address common.Address) bool {
	if _, ok := s.Voters[address]; ok {
		return true
	}
	return false
}

// check if address belong to candidate
func (s *Snapshot) isCandidate(address common.Address) bool {
	if _, ok := s.Candidates[address]; ok {
		return true
	}
	return false
}

// get last block number meet the confirm condition
func (s *Snapshot) getLastConfirmedBlockNumber(confirmations []Confirmation) *big.Int {

	cpyConfirmations := make(map[uint64][]*common.Address)
	for blockNumber, confirmers := range s.Confirmations {
		cpyConfirmations[blockNumber] = make([]*common.Address, len(confirmers))
		copy(cpyConfirmations[blockNumber], confirmers)
	}
	// update confirmation into snapshot
	for _, confirmation := range confirmations {
		_, ok := cpyConfirmations[confirmation.BlockNumber.Uint64()]
		if !ok {
			cpyConfirmations[confirmation.BlockNumber.Uint64()] = []*common.Address{}
		}
		addConfirmation := true
		for _, address := range cpyConfirmations[confirmation.BlockNumber.Uint64()] {
			if confirmation.Signer.Str() == address.Str() {
				addConfirmation = false
				break
			}
		}
		if addConfirmation == true {
			var confirmSigner common.Address
			confirmSigner.Set(confirmation.Signer)
			cpyConfirmations[confirmation.BlockNumber.Uint64()] = append(cpyConfirmations[confirmation.BlockNumber.Uint64()], &confirmSigner)
		}
	}

	i := s.Number
	for ; i > s.Number-s.config.MaxSignerCount*2/3+1; i-- {
		if confirmers, ok := cpyConfirmations[i]; ok {
			if len(confirmers) > int(s.config.MaxSignerCount*2/3) {
				return big.NewInt(int64(i))
			}
		}
	}
	return big.NewInt(int64(i))
}

func (s *Snapshot) calculateReward(coinbase common.Address, votersReward *big.Int) map[common.Address]*big.Int {

	rewards := make(map[common.Address]*big.Int)
	allStake := big.NewInt(0)
	for voter, vote := range s.Votes {
		if s.Number >= 1507109 {
			// if voter has voted that candidate and is now in freezing state...
			if vote.Candidate.Str() == coinbase.Str()  && s.Cancelers[voter] == nil{
				allStake.Add(allStake, vote.Stake)
				rewards[voter] = new(big.Int).Set(vote.Stake)
			}
		} else {
			if vote.Candidate.Str() == coinbase.Str() {
				allStake.Add(allStake, vote.Stake)
				rewards[voter] = new(big.Int).Set(vote.Stake)
			}
		}
	}
	for _, stake := range rewards {
		stake.Mul(stake, votersReward)
		stake.Div(stake, allStake)
	}
	return rewards
}
func (s * Snapshot)CalculateReward(coinbase common.Address, votersReward *big.Int) map[common.Address]*big.Int {
	return s.calculateReward(coinbase , votersReward)
}