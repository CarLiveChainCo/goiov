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
	"github.com/carlivechain/goiov/core"
	"math/big"
	"strconv"
	"strings"

	"github.com/carlivechain/goiov/common"
	"github.com/carlivechain/goiov/consensus"
	"github.com/carlivechain/goiov/core/state"
	"github.com/carlivechain/goiov/core/types"
	"github.com/carlivechain/goiov/log"
	"github.com/carlivechain/goiov/rlp"
)

const (
	/*
	 *  ufo:version:category:action/data
	 */
	ufoPrefix             = "ufo"
	ufoVersion            = "1"
	ufoCategoryEvent      = "event"
	ufoCategoryLog        = "oplog"
	ufoCategorySC         = "sc"
	ufoEventVote          = "vote"
	ufoEventConfirm       = "confirm"
	ufoEventCancel        = "cancel"
	ufoMinSplitLen        = 3
	posPrefix             = 0
	posVersion            = 1
	posCategory           = 2
	posEventVote          = 3
	posEventConfirm       = 3
	posEventCancel        = 3
	posEventVoteValue     = 4
	posEventConfirmNumber = 4
)

// Vote :
// vote come from custom tx which data like "ufo:1:event:vote:stake"
// Sender of tx is Voter, the tx.to is Candidate or self
type Vote struct {
	Voter     common.Address
	Candidate common.Address
	Stake     *big.Int
	Hash      common.Hash
}

// Cancel :
// cancel come from custom tx which data like "ufo:1:event:cancel"
// Sender of tx is Canceler
// Passive is true if
type Cancel struct {
	Canceler common.Address
	Passive  bool
}

// Confirmation :
// confirmation come  from custom tx which data like "ufo:1:event:confirm:123"
// 123 is the block number be confirmed
// Sender of tx is Signer only if the signer in the SignerQueue for block number 123
type Confirmation struct {
	Signer      common.Address
	BlockNumber *big.Int
}



// HeaderExtra is the struct of info in header.Extra[extraVanity:len(header.extra)-extraSeal]
type HeaderExtra struct {
	CurrentBlockConfirmations []Confirmation
	CurrentBlockVotes         []Vote
	CurrentBlockCancels       []Cancel
	LoopStartTime             uint64
	SignerQueue               []common.Address
	SignerMissing             []common.Address
	ConfirmedBlockNumber      uint64
	backup1					  []byte
	backup2                   []byte
}

// Calculate Votes from transaction in this block, write into header.Extra
func (a *Alien) processCustomTx(headerExtra HeaderExtra, chain consensus.ChainReader, header *types.Header, state *state.StateDB, txs []*types.Transaction) (HeaderExtra, error) {

	// if predecessor voter make transaction and vote in this block,
	// just process as vote, do it in snapshot.apply
	var (
		number uint64
	)
	number = header.Number.Uint64()
	for _, tx := range txs {
		txSender, err := types.Sender(types.NewEIP155Signer(tx.ChainId()), tx)
		if err != nil {
			continue
		}
		if len(string(tx.Data())) >= len(ufoPrefix) {
			txData := string(tx.Data())
			txDataInfo := strings.Split(txData, ":")
			if len(txDataInfo) >= ufoMinSplitLen {
				if txDataInfo[posPrefix] == ufoPrefix {
					if txDataInfo[posVersion] == ufoVersion {
						// process vote event
						if txDataInfo[posCategory] == ufoCategoryEvent {
							if len(txDataInfo) > ufoMinSplitLen {
								// check is vote or not
								if txDataInfo[posEventVote] == ufoEventVote {
									headerExtra.CurrentBlockVotes = a.processEventVote(chain , headerExtra.CurrentBlockVotes, state, tx, txSender, txDataInfo)
								} else if txDataInfo[posEventCancel] == ufoEventCancel {
									headerExtra.CurrentBlockCancels = a.processEventCancel(headerExtra.CurrentBlockCancels, state, tx, txSender, txDataInfo)
								} else if txDataInfo[posEventConfirm] == ufoEventConfirm {
									headerExtra.CurrentBlockConfirmations = a.processEventConfirm(headerExtra.CurrentBlockConfirmations, chain, txDataInfo, number, tx, txSender)
								}

								// if value is not zero, this vote may influence the balance of tx.To()
								if tx.Value().Cmp(big.NewInt(0)) == 0 {
									continue
								}

							} else {
								// todo : something wrong, leave this transaction to process as normal transaction
							}
						} else if txDataInfo[posCategory] == ufoCategoryLog {
							// todo :
						} else if txDataInfo[posCategory] == ufoCategorySC {
							if len(txDataInfo) > ufoMinSplitLen+2 {
								if txDataInfo[posEventConfirm] == ufoEventConfirm {
									// log.Info("Side chain confirm info", "hash", txDataInfo[ufoMinSplitLen+1])
									// log.Info("Side chain confirm info", "number", txDataInfo[ufoMinSplitLen+2])
								}
							}
						}
					}
				}
			}
		}

	}

	return headerExtra, nil
}

func (a *Alien) processEventVote(chain consensus.ChainReader,currentBlockVotes []Vote, state *state.StateDB, tx *types.Transaction, voter common.Address, txDataInfo []string) []Vote {
	if len(txDataInfo) >= posEventVoteValue {
		value, ok := big.NewInt(0).SetString(txDataInfo[posEventVoteValue], 10)
		if !ok {
			log.Warn("invalid vote value")
			return currentBlockVotes
		}

		bc, ok := chain.(*core.BlockChain)
		if !ok {
			log.Error("blockchain == nil when convert")
			return currentBlockVotes
		}
		header := bc.CurrentBlock().Header()

		snap , err:= a.snapshot(chain, header.Number.Uint64(), header.Hash(), nil, nil, DefaultLoopCntRecalculateSigners)
		if err != nil {
			log.Error(err.Error())
			return currentBlockVotes
		}

		if _, ok := snap.Votes[voter]; ok {
			log.Warn("Repeat vote in snap.Votes")
			log.Info("processEventVote...vote failed")
			return currentBlockVotes
		}
		if voter != *tx.To() {
			if value.Cmp(a.config.MinVoteValue) < 0 {
				log.Warn("Vote value less than MinVoteValue")
				return currentBlockVotes
			}
			if !snap.isCandidate(*tx.To()) {
				log.Warn("Vote target is not a candidate")
				return currentBlockVotes
			}
		} else {
			if value.Cmp(a.config.SelfVoteValue) < 0 {
				log.Warn("Vote value less than SelfVoteValue")
				return currentBlockVotes
			}
		}

		if state.GetBalance(voter).Cmp(value) > 0 {
			a.lock.RLock()
			stake := value
			a.lock.RUnlock()
			for _, vote := range currentBlockVotes {
				if vote.Voter == voter {
					log.Warn("Repeat vote in currentBlockVotes")
					return currentBlockVotes
				}
			}
			a.lock.Lock()
			state.SubBalance(voter, value)
			a.lock.Unlock()
			currentBlockVotes = append(currentBlockVotes, Vote{
				Voter:     voter,
				Candidate: *tx.To(),
				Stake:     stake,
				Hash:      tx.Hash(),
			})
		} else {
			log.Warn("Not enough balance for vote")
			return currentBlockVotes
		}
	}
	return currentBlockVotes
}

func (a *Alien) processEventCancel(currentBlockCancels []Cancel, state *state.StateDB, tx *types.Transaction, canceler common.Address, txDataInfo []string) []Cancel {
	if len(txDataInfo) >= posEventCancel {

		for _, cancel := range currentBlockCancels {
			if cancel.Canceler == canceler {
				log.Error("Repeat cancel")
				return currentBlockCancels
			}
		}

		currentBlockCancels = append(currentBlockCancels, Cancel{
			Canceler: canceler,
			Passive:  false,
		})
	}
	return currentBlockCancels
}

func (a *Alien) processEventConfirm(currentBlockConfirmations []Confirmation, chain consensus.ChainReader, txDataInfo []string, number uint64, tx *types.Transaction, confirmer common.Address) []Confirmation {
	if len(txDataInfo) >= posEventConfirmNumber {
		confirmedBlockNumber, err := strconv.Atoi(txDataInfo[posEventConfirmNumber])
		if err != nil || number-uint64(confirmedBlockNumber) > a.config.MaxSignerCount || number-uint64(confirmedBlockNumber) < 0 {
			return currentBlockConfirmations
		}
		// check if the voter is in block
		confirmedHeader := chain.GetHeaderByNumber(uint64(confirmedBlockNumber))
		if confirmedHeader == nil {
			log.Info("Fail to get confirmedHeader")
			return currentBlockConfirmations
		}
		confirmedHeaderExtra := HeaderExtra{}
		if extraVanity+extraSeal > len(confirmedHeader.Extra) {
			return currentBlockConfirmations
		}
		err = rlp.DecodeBytes(confirmedHeader.Extra[extraVanity:len(confirmedHeader.Extra)-extraSeal], &confirmedHeaderExtra)
		if err != nil {
			log.Info("Fail to decode parent header", "err", err)
			return currentBlockConfirmations
		}
		for _, s := range confirmedHeaderExtra.SignerQueue {
			if s == confirmer {
				currentBlockConfirmations = append(currentBlockConfirmations, Confirmation{
					Signer:      confirmer,
					BlockNumber: big.NewInt(int64(confirmedBlockNumber)),
				})
				break
			}
		}
	}

	return currentBlockConfirmations
}

func (a *Alien) processPredecessorVoter(modifyPredecessorVotes []Vote, state *state.StateDB, tx *types.Transaction, voter common.Address, snap *Snapshot) []Vote {
	// process normal transaction which relate to voter
	if tx.Value().Cmp(big.NewInt(0)) > 0 {
		if snap.isVoter(voter) {
			a.lock.RLock()
			stake := state.GetBalance(voter)
			a.lock.RUnlock()
			modifyPredecessorVotes = append(modifyPredecessorVotes, Vote{
				Voter:     voter,
				Candidate: common.Address{},
				Stake:     stake,
				Hash:      tx.Hash(),
			})
		}
		if snap.isVoter(*tx.To()) {
			a.lock.RLock()
			stake := state.GetBalance(*tx.To())
			a.lock.RUnlock()
			modifyPredecessorVotes = append(modifyPredecessorVotes, Vote{
				Voter:     *tx.To(),
				Candidate: common.Address{},
				Stake:     stake,
				Hash:      tx.Hash(),
			})
		}

	}
	return modifyPredecessorVotes
}
