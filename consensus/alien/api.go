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
	"fmt"
	"github.com/CarLiveChainCo/goiov/common"
	"github.com/CarLiveChainCo/goiov/consensus"
	"github.com/CarLiveChainCo/goiov/core/types"
	"github.com/CarLiveChainCo/goiov/rpc"
	"math/big"
)

// API is a user facing RPC API to allow controlling the signer and voting
// mechanisms of the delegated-proof-of-stake scheme.
type API struct {
	chain consensus.ChainReader
	alien *Alien
}

func (api *API) GetFreezeBalance(address common.Address) (uint64, error) {
	header := api.chain.CurrentHeader()
	if header == nil {
		return 0, errUnknownBlock
	}
	snapshot, err := api.alien.snapshot(api.chain, header.Number.Uint64(), header.Hash(), nil, nil, DefaultLoopCntRecalculateSigners)
	if err != nil {
		return 0, err
	}
	vote :=snapshot.Votes[address]
	if vote != nil {
		freezeBalance := vote.Stake
		return freezeBalance.Uint64(), nil
	}
	return 0, fmt.Errorf("No freeze balance for %x", address)
}

func (api *API) GetSideFreezeBalance(address common.Address, appId string) (uint64, error) {
	if sideChain, ok := api.alien.eth.SideBlockChain(appId); ok {
		header := sideChain.CurrentHeader()
		if header == nil {
			return 0, errUnknownBlock
		}
		sideAlien, _ := sideChain.Engine().(*Alien)
		snapshot, err := sideAlien.snapshot(sideChain, header.Number.Uint64(), header.Hash(), nil, nil, DefaultLoopCntRecalculateSigners)
		if err != nil {
			return 0, err
		}
		vote :=snapshot.Votes[address]
		if vote != nil {
			freezeBalance := vote.Stake
			return freezeBalance.Uint64(), nil
		}
		return 0, fmt.Errorf("No freeze balance for %x", address)

	} else {
		return 0, fmt.Errorf("appId %s does not exist", appId)
	}
}

func (api *API) GetRemainingFreezeTime(address common.Address) (uint64, error) {
	header := api.chain.CurrentHeader()
	if header == nil {
		return 0, errUnknownBlock
	}
	snapshot, err := api.alien.snapshot(api.chain, header.Number.Uint64(), header.Hash(), nil, nil, DefaultLoopCntRecalculateSigners)
	if err != nil {
		return 0, err
	}


	cancel := snapshot.Cancelers[address]
	if cancel != nil {
		cancelTime := cancel.Uint64()
		currentTime := header.Number.Uint64()
		freezeTime := api.alien.config.Freeze / api.alien.config.Period
		unfreezeTime := cancelTime + freezeTime
		remaining := unfreezeTime - currentTime
		if remaining <= 0 {
			remaining = 0
		}
		return remaining * api.alien.config.Period, nil
	} else {
		return 0, fmt.Errorf("No cancel for %x", address)
	}
}

func (api *API) GetSideRemainingFreezeTime(address common.Address, appId string) (uint64, error) {
	if sideChain, ok := api.alien.eth.SideBlockChain(appId); ok {
		header := sideChain.CurrentHeader()
		if header == nil {
			return 0, errUnknownBlock
		}
		sideAlien, _ := sideChain.Engine().(*Alien)
		snapshot, err := sideAlien.snapshot(sideChain, header.Number.Uint64(), header.Hash(), nil, nil, DefaultLoopCntRecalculateSigners)
		if err != nil {
			return 0, err
		}
		cancel := snapshot.Cancelers[address]
		if cancel != nil {
			cancelTime := cancel.Uint64()
			currentTime := header.Number.Uint64()
			freezeTime := api.alien.config.Freeze / api.alien.config.Period
			unfreezeTime := cancelTime + freezeTime
			remaining := unfreezeTime - currentTime
			if remaining <= 0 {
				remaining = 0
			}
			return remaining * api.alien.config.Period, nil
		} else {
			return 0, fmt.Errorf("No cancel for %x", address)
		}

	} else {
		return 0, fmt.Errorf("appId %s does not exist", appId)
	}
}


func (api *API) GetVote(address common.Address) (*Vote, error) {
	header := api.chain.CurrentHeader()
	if header == nil {
		return nil, errUnknownBlock
	}
	snapshot, err := api.alien.snapshot(api.chain, header.Number.Uint64(), header.Hash(), nil, nil, DefaultLoopCntRecalculateSigners)
	if err != nil {
		return nil, err
	}
	vote :=snapshot.Votes[address]
	return vote, nil
}

func (api *API) GetSideVote(address common.Address, appId string) (*Vote, error) {
	if sideChain, ok := api.alien.eth.SideBlockChain(appId); ok {
		header := sideChain.CurrentHeader()
		if header == nil {
			return nil, errUnknownBlock
		}
		sideAlien, _ := sideChain.Engine().(*Alien)
		snapshot, err := sideAlien.snapshot(sideChain, header.Number.Uint64(), header.Hash(), nil, nil, DefaultLoopCntRecalculateSigners)
		if err != nil {
			return nil, err
		}
		vote :=snapshot.Votes[address]
		return vote, nil

	} else {
		return nil, fmt.Errorf("appId %s does not exist", appId)
	}
}

func (api *API) GetTally(address common.Address) (uint64, error) {
	header := api.chain.CurrentHeader()
	if header == nil {
		return 0, errUnknownBlock
	}
	snapshot, err := api.alien.snapshot(api.chain, header.Number.Uint64(), header.Hash(), nil, nil, DefaultLoopCntRecalculateSigners)
	if err != nil {
		return 0, err
	}
	if tally, ok := snapshot.Tally[address]; ok {
		return tally.Uint64(), nil
	} else {
		return 0, fmt.Errorf("address doesn't have tally")
	}
}

func (api *API) GetSideTally(address common.Address, appId string) (uint64, error) {
	if sideChain, ok := api.alien.eth.SideBlockChain(appId); ok {
		header := sideChain.CurrentHeader()
		if header == nil {
			return 0, errUnknownBlock
		}
		sideAlien, _ := sideChain.Engine().(*Alien)
		snapshot, err :=  sideAlien.snapshot(sideChain, header.Number.Uint64(), header.Hash(), nil, nil, DefaultLoopCntRecalculateSigners)
		if err != nil {
			return 0, err
		}
		if tally, ok :=snapshot.Tally[address]; ok {
			return tally.Uint64(), nil
		} else {
			return 0, fmt.Errorf("address doesn't have tally")
		}

	} else {
		return 0, fmt.Errorf("appId %s does not exist", appId)
	}
}

func (api *API) GetCandidatesAndTally() (map[common.Address]*big.Int, error) {
	header := api.chain.CurrentHeader()
	if header == nil {
		return nil, errUnknownBlock
	}
	snapshot, err := api.alien.snapshot(api.chain, header.Number.Uint64(), header.Hash(), nil, nil, DefaultLoopCntRecalculateSigners)
	if err != nil {
		return nil, err
	}
	tally :=snapshot.Tally
	return tally, nil
}

func (api *API) GetSideCandidatesAndTally(appId string) (map[common.Address]*big.Int, error) {
	if sideChain, ok := api.alien.eth.SideBlockChain(appId); ok {
		header := sideChain.CurrentHeader()
		if header == nil {
			return nil, errUnknownBlock
		}
		sideAlien, _ := sideChain.Engine().(*Alien)
		snapshot, err := sideAlien.snapshot(sideChain, header.Number.Uint64(), header.Hash(), nil, nil, DefaultLoopCntRecalculateSigners)
		if err != nil {
			return nil, err
		}
		return snapshot.Tally, nil
	} else {
		return nil, fmt.Errorf("appId %s does not exist", appId)
	}
}
func (api *API) GetSideSnapshot(appId string) (*Snapshot, error) {
	// Retrieve the requested block number (or current if none requested)
	if sideChain, ok := api.alien.eth.SideBlockChain(appId); ok {
		header := sideChain.CurrentHeader()
		sideAlien, _ := sideChain.Engine().(*Alien)
		return sideAlien.snapshot(sideChain, header.Number.Uint64(), header.Hash(), nil, nil, DefaultLoopCntRecalculateSigners)
	} else {
		return nil, fmt.Errorf("appId %s does not exist", appId)
	}
}

// GetSnapshot retrieves the state snapshot at a given block.
func (api *API) GetSnapshot(number *rpc.BlockNumber) (*Snapshot, error) {
	// Retrieve the requested block number (or current if none requested)
	var header *types.Header
	if number == nil || *number == rpc.LatestBlockNumber {
		header = api.chain.CurrentHeader()
	} else {
		header = api.chain.GetHeaderByNumber(uint64(number.Int64()))
	}
	// Ensure we have an actually valid block and return its snapshot
	if header == nil {
		return nil, errUnknownBlock
	}
	return api.alien.snapshot(api.chain, header.Number.Uint64(), header.Hash(), nil, nil, DefaultLoopCntRecalculateSigners)
}

// GetSnapshotAtHash retrieves the state snapshot at a given block.
func (api *API) GetSnapshotAtHash(hash common.Hash) (*Snapshot, error) {
	header := api.chain.GetHeaderByHash(hash)
	if header == nil {
		return nil, errUnknownBlock
	}
	return api.alien.snapshot(api.chain, header.Number.Uint64(), header.Hash(), nil, nil, DefaultLoopCntRecalculateSigners)
}

// GetSideSnapshotAtNumber retrieves the state snapshot at a given block.
func (api *API) GetSideSnapshotAtNumber(number uint64, appId string) (*Snapshot, error) {

	if sideChain, ok := api.alien.eth.SideBlockChain(appId); ok {
		header := sideChain.GetHeaderByNumber(number)
		sideAlien, _ := sideChain.Engine().(*Alien)
		return sideAlien.snapshot(sideChain, header.Number.Uint64(), header.Hash(), nil, nil, DefaultLoopCntRecalculateSigners)
	} else {
		return nil, fmt.Errorf("appId %s does not exist", appId)
	}
}

// GetSnapshotAtNumber retrieves the state snapshot at a given block.
func (api *API) GetSnapshotAtNumber(number uint64) (*Snapshot, error) {
	header := api.chain.GetHeaderByNumber(number)
	if header == nil {
		return nil, errUnknownBlock
	}
	return api.alien.snapshot(api.chain, header.Number.Uint64(), header.Hash(), nil, nil, DefaultLoopCntRecalculateSigners)
}

// GetSnapshotByHeaderTime retrieves the state snapshot by timestamp of header.
// snapshot.header.time <= targetTime < snapshot.header.time + period
func (api *API) GetSnapshotByHeaderTime(targetTime uint64) (*Snapshot, error) {
	period := api.chain.Config().Alien.Period
	header := api.chain.CurrentHeader()
	if header == nil || targetTime > header.Time.Uint64()+period {
		return nil, errUnknownBlock
	}
	minN := uint64(0)
	maxN := header.Number.Uint64()
	for {
		if targetTime >= header.Time.Uint64() && targetTime < header.Time.Uint64()+period {
			return api.alien.snapshot(api.chain, header.Number.Uint64(), header.Hash(), nil, nil, DefaultLoopCntRecalculateSigners)
		} else {
			if maxN == minN || maxN == minN+1 {
				break
			}
			// calculate next number
			nextN := uint64(int64(header.Number.Uint64()) + (int64(targetTime)-int64(header.Time.Uint64()))/int64(period))
			if nextN >= maxN || nextN <= minN {
				nextN = (maxN + minN) / 2
			}
			// get new header
			header = api.chain.GetHeaderByNumber(nextN)
			if header == nil {
				break
			}
			// update maxN & minN
			if header.Time.Uint64() >= targetTime {
				if header.Number.Uint64() < maxN {
					maxN = header.Number.Uint64()
				}
			} else if header.Time.Uint64() <= targetTime {
				if header.Number.Uint64() > minN {
					minN = header.Number.Uint64()
				}
			}
		}
	}
	return nil, errUnknownBlock
}
