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
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/carlivechain/goiov/accounts"
	"github.com/carlivechain/goiov/common"
	"github.com/carlivechain/goiov/consensus"
	"github.com/carlivechain/goiov/core"
	"github.com/carlivechain/goiov/core/rawdb"
	"github.com/carlivechain/goiov/core/state"
	"github.com/carlivechain/goiov/core/types"
	"github.com/carlivechain/goiov/crypto"
	"github.com/carlivechain/goiov/crypto/sha3"
	"github.com/carlivechain/goiov/ethdb"
	"github.com/carlivechain/goiov/log"
	"github.com/carlivechain/goiov/node"
	"github.com/carlivechain/goiov/params"
	"github.com/carlivechain/goiov/rlp"
	"github.com/carlivechain/goiov/rpc"
	"github.com/hashicorp/golang-lru"
)

const (
	inMemorySnapshots  = 128                 // Number of recent vote snapshots to keep in memory
	inMemorySignatures = 4096                // Number of recent block signatures to keep in memory
	SecondsPerYear     = 2 * 365 * 24 * 3600 // Number of seconds for one year
	checkpointInterval = 360                   // About N hours if config.period is N
)

// Alien delegated-proof-of-stake protocol constants.
var (
	SignerBlockReward                = new(big.Int).Mul(big.NewInt(5), big.NewInt(5e+18)) // Block reward in wei for successfully mining a block first year
	MinerRewardPerThousand           = uint64(618)                                        // Default reward for miner in each block from block reward (618/1000)
	DefaultLoopCntRecalculateSigners = uint64(1)                                          // Default loop count to recreate signers from top tally
	defaultBlockPeriod               = uint64(5)                                          // Default minimum difference between two consecutive block's timestamps
	defaultMaxSignerCount            = uint64(21)                                         //
	defaultMinVoteValue              = new(big.Int).Mul(big.NewInt(100), big.NewInt(1e+18))
	defaultSelfVoteValue             = new(big.Int).Mul(big.NewInt(5000000), big.NewInt(1e+18))
	extraVanity                      = 32                       // Fixed number of extra-data prefix bytes reserved for signer vanity
	extraSeal                        = 65                       // Fixed number of extra-data suffix bytes reserved for signer seal
	uncleHash                        = types.CalcUncleHash(nil) // Always Keccak256(RLP([])) as uncles are meaningless outside of PoW.
	defaultDifficulty                = big.NewInt(1)            // Default difficulty
	defaultSignerFirst               = "0x3bec3387afdb06daf8b892b17ddbec323e7954ad"
	defaultSignerSecond              = "0xd74835491a6562faa6e9580c1986cf216c0f1c44"
	defaultSignerThired              = "0x4be612b5a43aa2f3c52b4e88a18f5fc46356123f"

	testSignerFirst  = "0x5e603ede4c2b510bdecef2f837b837ed2a3172d3"
	testSignerSecond = "0xe51a6769298b9649dd4d4cd6fd67891ceb8d97f7"
)

// Various error messages to mark blocks invalid. These should be private to
// prevent engine specific errors from being referenced in the remainder of the
// codebase, inherently breaking if the engine is swapped out. Please put common
// error types into the consensus package.
var (
	// errUnknownBlock is returned when the list of signers is requested for a block
	// that is not part of the local blockchain.
	errUnknownBlock = errors.New("unknown block")

	// errMissingVanity is returned if a block's extra-data section is shorter than
	// 32 bytes, which is required to store the signer vanity.
	errMissingVanity = errors.New("extra-data 32 byte vanity prefix missing")

	// errMissingSignature is returned if a block's extra-data section doesn't seem
	// to contain a 65 byte secp256k1 signature.
	errMissingSignature = errors.New("extra-data 65 byte suffix signature missing")

	// errInvalidMixDigest is returned if a block's mix digest is non-zero.
	errInvalidMixDigest = errors.New("non-zero mix digest")

	// errInvalidUncleHash is returned if a block contains an non-empty uncle list.
	errInvalidUncleHash = errors.New("non empty uncle hash")

	// ErrInvalidTimestamp is returned if the timestamp of a block is lower than
	// the previous block's timestamp + the minimum block period.
	ErrInvalidTimestamp = errors.New("invalid timestamp")

	// errInvalidVotingChain is returned if an authorization list is attempted to
	// be modified via out-of-range or non-contiguous headers.
	errInvalidVotingChain = errors.New("invalid voting chain")

	// errUnauthorized is returned if a header is signed by a non-authorized entity.
	errUnauthorized = errors.New("unauthorized")

	// errPunishedMissing is returned if a header calculate punished signer is wrong.
	errPunishedMissing = errors.New("punished signer missing")

	// errWaitTransactions is returned if an empty block is attempted to be sealed
	// on an instant chain (0 second period). It's important to refuse these as the
	// block reward is zero, so an empty block just bloats the chain... fast.
	errWaitTransactions = errors.New("waiting for transactions")

	// errUnclesNotAllowed is returned if uncles exists
	errUnclesNotAllowed = errors.New("uncles not allowed")

	// errCreateSignerQueueNotAllowed is returned if called in (block number + 1) % maxSignerCount != 0
	errCreateSignerQueueNotAllowed = errors.New("create signer queue not allowed")

	// errInvalidSignerQueue is returned if verify SignerQueue fail
	errInvalidSignerQueue = errors.New("invalid signer queue")

	// errSignerQueueEmpty is returned if no signer when calculate
	errSignerQueueEmpty = errors.New("signer queue is empty")
)

// Alien is the delegated-proof-of-stake consensus engine.
type Alien struct {
	config     *params.AlienConfig // Consensus engine configuration parameters
	db         ethdb.Database      // Database to store and retrieve snapshot checkpoints
	recents    *lru.ARCCache       // Snapshots for recent block to speed up reorgs
	signatures *lru.ARCCache       // Signatures of recent blocks to speed up mining
	signer     common.Address      // Ethereum address of the signing key
	signFn     SignerFn            // Signer function to authorize hashes with
	signTxFn   SignTxFn            // Sign transaction function to sign tx
	lock       sync.RWMutex        // Protects the signer fields
	lcsc       uint64              // Last confirmed side chain
	eth        core.Backend        // 用于侧链通向主链
}

// SignerFn is a signer callback function to request a hash to be signed by a
// backing account.
type SignerFn func(accounts.Account, []byte) ([]byte, error)

// SignTxFn is a signTx
type SignTxFn func(accounts.Account, *types.Transaction, *big.Int) (*types.Transaction, error)

// sigHash returns the hash which is used as input for the delegated-proof-of-stake
// signing. It is the hash of the entire header apart from the 65 byte signature
// contained at the end of the extra data.
//
// Note, the method requires the extra data to be at least 65 bytes, otherwise it
// panics. This is done to avoid accidentally using both forms (signature present
// or not), which could be abused to produce different hashes for the same header.
func sigHash(header *types.Header) (hash common.Hash) {
	hasher := sha3.NewKeccak256()
	rlp.Encode(hasher, []interface{}{
		header.ParentHash,
		header.UncleHash,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		header.Extra[:len(header.Extra)-65], // Yes, this will panic if extra is too short
		header.MixDigest,
		header.Nonce,
	})
	hasher.Sum(hash[:0])
	return hash
}

// ecrecover extracts the Ethereum account address from a signed header.
func ecrecover(header *types.Header, sigcache *lru.ARCCache) (common.Address, error) {
	// If the signature's already cached, return that
	hash := header.Hash()
	if address, known := sigcache.Get(hash); known {
		return address.(common.Address), nil
	}
	// Retrieve the signature from the header extra-data
	if len(header.Extra) < extraSeal {
		return common.Address{}, errMissingSignature
	}
	signature := header.Extra[len(header.Extra)-extraSeal:]

	// Recover the public key and the Ethereum address
	// 用原文解密加密后的密文得到公钥
	pubkey, err := crypto.Ecrecover(sigHash(header).Bytes(), signature)
	if err != nil {
		return common.Address{}, err
	}
	var signer common.Address
	copy(signer[:], crypto.Keccak256(pubkey[1:])[12:])

	sigcache.Add(hash, signer)
	return signer, nil
}

// New creates a Alien delegated-proof-of-stake consensus engine with the initial
// signers set to the ones provided by the user.
func New(config *params.AlienConfig, db ethdb.Database, testFlag bool, eth ...core.Backend) *Alien {
	// Set any missing consensus parameters to their defaults
	conf := config
	if conf.Period == 0 {
		conf.Period = defaultBlockPeriod
	}
	if conf.MaxSignerCount == 0 {
		conf.MaxSignerCount = defaultMaxSignerCount
	}
	if conf.MinVoteValue == nil || conf.MinVoteValue.Uint64() == 0 {
		conf.MinVoteValue = defaultMinVoteValue
	}
	if conf.SelfVoteValue == nil || conf.SelfVoteValue.Uint64() == 0 {
		conf.SelfVoteValue = defaultSelfVoteValue
	}
	if conf.Freeze == 0 {
		conf.Freeze = 20
	}

	if (len(conf.SelfVoteSigners) == 0) && conf.AppId == "" {
		if testFlag {
			conf.SelfVoteSigners = append(conf.SelfVoteSigners, common.HexToAddress(testSignerFirst))
			conf.SelfVoteSigners = append(conf.SelfVoteSigners, common.HexToAddress(testSignerSecond))
		} else {
			conf.SelfVoteSigners = append(conf.SelfVoteSigners, common.HexToAddress(defaultSignerFirst))
			conf.SelfVoteSigners = append(conf.SelfVoteSigners, common.HexToAddress(defaultSignerSecond))
			conf.SelfVoteSigners = append(conf.SelfVoteSigners, common.HexToAddress(defaultSignerThired))
		}
	}

	client, err := rpc.Dial("http://" + "localhost" + ":" + strconv.Itoa(node.DefaultHTTPPort))
	if err != nil {
		log.Error("side net rpc connect fail: %v", err)
	}
	conf.MCRPCClient = client

	// Allocate the snapshot caches and create the engine
	recents, _ := lru.NewARC(inMemorySnapshots)
	signatures, _ := lru.NewARC(inMemorySignatures)

	var backend core.Backend
	if len(eth) > 0 {
		backend = eth[0]
	}
	return &Alien{
		config:     conf,
		db:         db,
		recents:    recents,
		signatures: signatures,
		eth:        backend,
	}
}

// Author implements consensus.Engine, returning the Ethereum address recovered
// from the signature in the header's extra-data section.
func (a *Alien) Author(header *types.Header) (common.Address, error) {
	return ecrecover(header, a.signatures)
}

func (a *Alien) SetEth(eth core.Backend) {
	a.eth = eth
}

// VerifyHeader checks whether a header conforms to the consensus rules.
func (a *Alien) VerifyHeader(chain consensus.ChainReader, header *types.Header, seal bool) error {
	return a.verifyHeader(chain, header, nil)
}

// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers. The
// method returns a quit channel to abort the operations and a results channel to
// retrieve the async verifications (the order is that of the input slice).
func (a *Alien) VerifyHeaders(chain consensus.ChainReader, headers []*types.Header, seals []bool) (chan<- struct{}, <-chan error) {
	abort := make(chan struct{})
	results := make(chan error, len(headers))

	go func() {
		for i, header := range headers {
			err := a.verifyHeader(chain, header, headers[:i])

			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	}()
	return abort, results
}

// verifyHeader checks whether a header conforms to the consensus rules.The
// caller may optionally pass in a batch of parents (ascending order) to avoid
// looking those up from the database. This is useful for concurrently verifying
// a batch of new headers.
func (a *Alien) verifyHeader(chain consensus.ChainReader, header *types.Header, parents []*types.Header) error {
	if header.Number == nil {
		return errUnknownBlock
	}

	// Don't waste time checking blocks from the future
	if header.Time.Cmp(big.NewInt(time.Now().Unix())) > 0 {
		return consensus.ErrFutureBlock
	}

	// Check that the extra-data contains both the vanity and signature
	if len(header.Extra) < extraVanity {
		return errMissingVanity
	}
	if len(header.Extra) < extraVanity+extraSeal {
		return errMissingSignature
	}

	// Ensure that the mix digest is zero as we don't have fork protection currently
	if header.MixDigest != (common.Hash{}) {
		return errInvalidMixDigest
	}
	// Ensure that the block doesn't contain any uncles which are meaningless in PoA
	if header.UncleHash != uncleHash {
		return errInvalidUncleHash
	}

	// All basic checks passed, verify cascading fields
	return a.verifyCascadingFields(chain, header, parents)
}

// verifyCascadingFields verifies all the header fields that are not standalone,
// rather depend on a batch of previous headers. The caller may optionally pass
// in a batch of parents (ascending order) to avoid looking those up from the
// database. This is useful for concurrently verifying a batch of new headers.
func (a *Alien) verifyCascadingFields(chain consensus.ChainReader, header *types.Header, parents []*types.Header) error {
	// The genesis block is the always valid dead-end
	number := header.Number.Uint64()
	if number == 0 {
		return nil
	}
	// Ensure that the block's timestamp isn't too close to it's parent
	var parent *types.Header
	if len(parents) > 0 {
		parent = parents[len(parents)-1]
	} else {
		parent = chain.GetHeader(header.ParentHash, number-1)
	}
	if parent == nil || parent.Number.Uint64() != number-1 || parent.Hash() != header.ParentHash {
		return consensus.ErrUnknownAncestor
	}
	if parent.Time.Uint64() > header.Time.Uint64() {
		return ErrInvalidTimestamp
	}

	// Retrieve the snapshot needed to verify this header and cache it
	_, err := a.snapshot(chain, number-1, header.ParentHash, parents, nil, DefaultLoopCntRecalculateSigners)
	if err != nil {
		return err
	}

	// All basic checks passed, verify the seal and return
	return a.verifySeal(chain, header, parents)
}

// snapshot retrieves the authorization snapshot at a given point in time.
func (a *Alien) snapshot(chain consensus.ChainReader, number uint64, hash common.Hash, parents []*types.Header, genesisVotes []*Vote, lcrs uint64) (*Snapshot, error) {
	// Don't keep snapshot for side chain
	//if chain.Config().Alien.SideChain {
	//	return nil, nil
	//}
	// Search for a snapshot in memory or on disk for checkpoints
	var (
		headers []*types.Header
		snap    *Snapshot
	)
	for snap == nil {
		// If an in-memory snapshot was found, use that
		if s, ok := a.recents.Get(hash); ok {
			snap = s.(*Snapshot)
			break
		}
		// If an on-disk checkpoint snapshot can be found, use that
		if number%checkpointInterval == 0 {
			if s, err := loadSnapshot(a.config, a.signatures, a.db, hash); err == nil {
				log.Trace("Loaded voting snapshot from disk", "number", number, "hash", hash)
				snap = s
				break
			}
		}
		// If we're at block zero, make a snapshot
		if number == 0 {
			genesis := chain.GetHeaderByNumber(0)
			if err := a.VerifyHeader(chain, genesis, false); err != nil {
				return nil, err
			}
			a.config.Period = chain.Config().Alien.Period
			snap = newSnapshot(a.config, a.signatures, genesis.Hash(), genesisVotes, lcrs)
			if err := snap.store(a.db); err != nil {
				return nil, err
			}
			log.Trace("Stored genesis voting snapshot to disk")
			break
		}
		// No snapshot for this header, gather the header and move backward
		var header *types.Header
		if len(parents) > 0 {
			// If we have explicit parents, pick from there (enforced)
			header = parents[len(parents)-1]
			if header.Hash() != hash || header.Number.Uint64() != number {
				return nil, consensus.ErrUnknownAncestor
			}
			parents = parents[:len(parents)-1]
		} else {
			// No explicit parents (or no more left), reach out to the database
			header = chain.GetHeader(hash, number)

			if header == nil {
				return nil, consensus.ErrUnknownAncestor
			}
		}
		headers = append(headers, header)
		number, hash = number-1, header.ParentHash
	}
	// Previous snapshot found, apply any pending headers on top of it
	for i := 0; i < len(headers)/2; i++ {
		headers[i], headers[len(headers)-1-i] = headers[len(headers)-1-i], headers[i]
	}

	snap, err := snap.apply(headers)
	if err != nil {
		return nil, err
	}

	a.recents.Add(snap.Hash, snap)

	// If we've generated a new checkpoint snapshot, save to disk
	if snap.Number%checkpointInterval == 0 && len(headers) > 0 {
		if err = snap.store(a.db); err != nil {
			return nil, err
		}
		log.Trace("Stored voting snapshot to disk", "number", snap.Number, "hash", snap.Hash)
	}
	return snap, err
}
func (a *Alien) Snapshot(chain consensus.ChainReader, number uint64, hash common.Hash, parents []*types.Header, genesisVotes []*Vote, lcrs uint64) (*Snapshot, error) {
	return a.snapshot(chain, number, hash, parents, genesisVotes, lcrs)
}

// VerifyUncles implements consensus.Engine, always returning an error for any
// uncles as this consensus mechanism doesn't permit uncles.
func (a *Alien) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	if len(block.Uncles()) > 0 {
		return errUnclesNotAllowed
	}
	return nil
}

// VerifySeal implements consensus.Engine, checking whether the signature contained
// in the header satisfies the consensus protocol requirements.
func (a *Alien) VerifySeal(chain consensus.ChainReader, header *types.Header) error {
	return a.verifySeal(chain, header, nil)
}

// verifySeal checks whether the signature contained in the header satisfies the
// consensus protocol requirements. The method accepts an optional list of parent
// headers that aren't yet part of the local blockchain to generate the snapshots
// from.
func (a *Alien) verifySeal(chain consensus.ChainReader, header *types.Header, parents []*types.Header) error {
	// Verifying the genesis block is not supported
	number := header.Number.Uint64()
	if number == 0 {
		return errUnknownBlock
	}
	// Retrieve the snapshot needed to verify this header and cache it
	snap, err := a.snapshot(chain, number-1, header.ParentHash, parents, nil, DefaultLoopCntRecalculateSigners)
	if err != nil {
		return err
	}

	// Resolve the authorization key and check against signers
	// 解析授权密钥并检查签名者
	signer, err := ecrecover(header, a.signatures)
	if err != nil {
		return err
	}

	if !chain.Config().Alien.SideChain {

		if number > a.config.MaxSignerCount {
			var parent *types.Header
			if len(parents) > 0 {
				parent = parents[len(parents)-1]
			} else {
				parent = chain.GetHeader(header.ParentHash, number-1)
			}
			parentHeaderExtra := HeaderExtra{}
			err = rlp.DecodeBytes(parent.Extra[extraVanity:len(parent.Extra)-extraSeal], &parentHeaderExtra)
			if err != nil {
				log.Info("Fail to decode parent header", "err", err)
				return err
			}
			currentHeaderExtra := HeaderExtra{}
			err = rlp.DecodeBytes(header.Extra[extraVanity:len(header.Extra)-extraSeal], &currentHeaderExtra)
			if err != nil {
				log.Info("Fail to decode header", "err", err)
				return err
			}
			// verify signerqueue
			if number%a.config.MaxSignerCount == 0 {
				err := snap.verifySignerQueue(currentHeaderExtra.SignerQueue, a.eth)
				if err != nil {
					return err
				}

			} else {
				for i := 0; i < int(a.config.MaxSignerCount); i++ {
					if parentHeaderExtra.SignerQueue[i] != currentHeaderExtra.SignerQueue[i] {
						return errInvalidSignerQueue
					}
				}
			}

			// verify missing signer for punish
			parentSignerMissing := getSignerMissing(parent.Coinbase, header.Coinbase, parentHeaderExtra)
			if len(parentSignerMissing) != len(currentHeaderExtra.SignerMissing) {
				return errPunishedMissing
			}
			for i, signerMissing := range currentHeaderExtra.SignerMissing {
				if parentSignerMissing[i] != signerMissing {
					return errPunishedMissing
				}
			}
		}

		if !snap.inturn(signer, header) {
			return errUnauthorized
		}
	} else {
		if !a.mcInturn(chain, signer, header.Time.Uint64()) {
			return errUnauthorized
		} else {
			// send tx to main chain to confirm this block
			a.mcConfirmBlock(chain, header)
		}
	}

	return nil
}

// Prepare implements consensus.Engine, preparing all the consensus fields of the
// header for running the transactions on top.
func (a *Alien) Prepare(chain consensus.ChainReader, header *types.Header) error {
	// Set the correct difficulty
	header.Difficulty = new(big.Int).Set(defaultDifficulty)

	number := header.Number.Uint64()
	parent := chain.GetHeader(header.ParentHash, number-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	header.Time = new(big.Int).Add(parent.Time, new(big.Int).SetUint64(a.config.Period))
	if header.Time.Int64() < time.Now().Unix() {
		header.Time = big.NewInt(time.Now().Unix())
	}

	// If now is later than genesis timestamp, skip prepare
	if a.config.GenesisTimestamp < uint64(time.Now().Unix()) {
		return nil
	}

	// Count down for start
	if header.Number.Uint64() == 1 {
		for {
			delay := time.Unix(int64(a.config.GenesisTimestamp-2), 0).Sub(time.Now())
			if delay <= time.Duration(0) {
				log.Info("Ready for seal block", "time", time.Now())
				break
			} else if delay > time.Duration(a.config.Period)*time.Second {
				delay = time.Duration(a.config.Period) * time.Second
			}
			log.Info("Waiting for seal block", "delay", common.PrettyDuration(time.Unix(int64(a.config.GenesisTimestamp-2), 0).Sub(time.Now())))
			select {
			case <-time.After(delay):
				continue
			}
		}
	}

	return nil
}

func (a *Alien) mcInturn(chain consensus.ChainReader, signer common.Address, headerTime uint64) bool {
	if chain.Config().Alien.SideChain {
		ms, err := a.getMainChainSnapshotByTime(chain, headerTime)
		if err != nil {
			log.Info("Main chain snapshot query fail ", "err", err)
			return false
		}
		// calculate the coinbase by loopStartTime & signers slice
		loopIndex := int((headerTime-ms.LoopStartTime)/ms.Period) % len(ms.Signers)
		if loopIndex >= len(ms.Signers) {
			return false
		} else if *ms.Signers[loopIndex] != signer {
			return false
		}
		return true
	}
	return false
}

func (a *Alien) mcConfirmBlock(chain consensus.ChainReader, header *types.Header) {

	a.lock.RLock()
	signer, signTxFn := a.signer, a.signTxFn
	a.lock.RUnlock()

	if signer != (common.Address{}) {
		nonce, err := a.getTransactionCountFromMainChain(chain, signer)
		if err != nil {
			log.Info("confirm tx sign fail", "err", err)
		}
		// todo update gaslimit , gasprice ,and get ChainID need to get from mainchain
		if header.Number.Uint64() > a.lcsc {

			tx := types.NewTransaction(nonce,
				header.Coinbase, big.NewInt(0),
				uint64(100000), big.NewInt(100000),
				[]byte(fmt.Sprintf("ufo:1:sc:confirm:%s:%d", chain.GetHeaderByNumber(1).Hash().Hex(), header.Number.Uint64())))

			signedTx, err := signTxFn(accounts.Account{Address: signer}, tx, big.NewInt(1014))
			if err != nil {
				log.Info("confirm tx sign fail", "err", err)
			}
			res, err := a.sendTransactionToMainChain(chain, signedTx)
			if err != nil {

				log.Info("confirm tx send fail", "err", err)
			} else {
				log.Info("confirm tx result", "hash", res)
				a.lcsc = header.Number.Uint64()
			}
		}
	}

}

// Finalize implements consensus.Engine, ensuring no uncles are set, nor block
// rewards given, and returns the final block.
func (a *Alien) Finalize(chain consensus.ChainReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header, receipts []*types.Receipt) (*types.Block, error) {

	number := header.Number.Uint64()

	// Mix digest is reserved for now, set to empty
	header.MixDigest = common.Hash{}

	// Ensure the timestamp has the correct delay
	// 确保时间戳具有正确的延迟
	parent := chain.GetHeader(header.ParentHash, number-1)
	if parent == nil {
		return nil, consensus.ErrUnknownAncestor
	}

	// Ensure the extra data has all it's components
	if len(header.Extra) < extraVanity {
		header.Extra = append(header.Extra, bytes.Repeat([]byte{0x00}, extraVanity-len(header.Extra))...)
	}
	header.Extra = header.Extra[:extraVanity]
	// genesisVotes write direct into snapshot, which number is 1
	//创世投票写进快照，number 为 1
	var genesisVotes []*Vote
	parentHeaderExtra := HeaderExtra{}
	currentHeaderExtra := HeaderExtra{}
	//mainState := state
	//if a.eth != nil {
	//	mainState = a.eth.TxPool().GetCurrentState()
	//}
	if number == 1 {
		alreadyVote := make(map[common.Address]struct{})
		for _, voter := range a.config.SelfVoteSigners {
			if _, ok := alreadyVote[voter]; !ok {
				if state.GetBalance(voter).Cmp(a.config.SelfVoteValue) >= 0 {
					state.SubBalance(voter, a.config.SelfVoteValue)
					genesisVotes = append(genesisVotes, &Vote{
						Voter:     voter,
						Candidate: voter,
						Stake:     a.config.SelfVoteValue,
						Hash:      common.Hash{},
					})
					alreadyVote[voter] = struct{}{}
				}
			}
		}

	} else {
		// decode extra from last header.extra
		err := rlp.DecodeBytes(parent.Extra[extraVanity:len(parent.Extra)-extraSeal], &parentHeaderExtra)
		if err != nil {
			log.Info("Fail to decode parent header", "err", err)
			return nil, err
		}
		currentHeaderExtra.ConfirmedBlockNumber = parentHeaderExtra.ConfirmedBlockNumber //确认块号
		currentHeaderExtra.SignerQueue = parentHeaderExtra.SignerQueue                   //出块人
		currentHeaderExtra.LoopStartTime = parentHeaderExtra.LoopStartTime
		currentHeaderExtra.SignerMissing = getSignerMissing(parent.Coinbase, header.Coinbase, parentHeaderExtra)
	}
	// calculate votes write into header.extra
	//区分各种交易
	currentHeaderExtra, err := a.processCustomTx(currentHeaderExtra, chain, header, state, txs)
	if err != nil {
		return nil, err
	}
	// Assemble the voting snapshot to check which votes make sense
	snap, err := a.snapshot(chain, number-1, header.ParentHash, nil, genesisVotes, DefaultLoopCntRecalculateSigners)
	if err != nil {
		return nil, err
	}

	if !chain.Config().Alien.SideChain {
		currentHeaderExtra.ConfirmedBlockNumber = snap.getLastConfirmedBlockNumber(currentHeaderExtra.CurrentBlockConfirmations).Uint64()

		// write signerQueue in first header, from self vote signers in genesis block
		if number == 1 {
			currentHeaderExtra.LoopStartTime = a.config.GenesisTimestamp
			for i := 0; i < int(a.config.MaxSignerCount); i++ {
				currentHeaderExtra.SignerQueue = append(currentHeaderExtra.SignerQueue, a.config.SelfVoteSigners[i%len(a.config.SelfVoteSigners)])
			}
		}

		// add balance for cancels
		for canceler, cancel := range snap.Cancels {
			number := header.Number.Uint64()
			if (cancel.Passive && (number == 1+snap.Cancelers[canceler].Uint64())) ||
				!cancel.Passive && (number+2 == snap.Cancelers[canceler].Uint64()+snap.config.Freeze/snap.config.Period) {
				if vote, ok := snap.Votes[canceler]; ok {
					a.lock.Lock()
					state.AddBalance(cancel.Canceler, vote.Stake)
					a.lock.Unlock()
				}
			}
		}

		if number%a.config.MaxSignerCount == 0 {
			//currentHeaderExtra.LoopStartTime = header.Time.Uint64()
			currentHeaderExtra.LoopStartTime = currentHeaderExtra.LoopStartTime + a.config.Period*a.config.MaxSignerCount
			// create random signersQueue in currentHeaderExtra by snapshot.Tally
			currentHeaderExtra.SignerQueue = []common.Address{}
			newSignerQueue, err := snap.createSignerQueue(a.eth)
			if err != nil {
				return nil, err
			}
			currentHeaderExtra.SignerQueue = newSignerQueue
		}
		// 主链矿工帮 appid <= 100 且没有候选人的侧链挖矿
		a.automaticMining(number,snap)

	} else {
		// use currentHeaderExtra.SignerQueue as signer queue
		currentHeaderExtra.SignerQueue = append([]common.Address{header.Coinbase}, parentHeaderExtra.SignerQueue...)
		if len(currentHeaderExtra.SignerQueue) > int(a.config.MaxSignerCount) {
			currentHeaderExtra.SignerQueue = currentHeaderExtra.SignerQueue[:int(a.config.MaxSignerCount)]
		}
	}
	// encode header.extra
	currentHeaderExtraEnc, err := rlp.EncodeToBytes(currentHeaderExtra)
	if err != nil {
		return nil, err
	}

	header.Extra = append(header.Extra, currentHeaderExtraEnc...)
	header.Extra = append(header.Extra, make([]byte, extraSeal)...)

	// Set the correct difficulty
	header.Difficulty = new(big.Int).Set(defaultDifficulty)
	// Accumulate any block rewards and commit the final state root
	accumulateRewards(chain.Config(), state, header, snap)

	header.Root = state.IntermediateRoot(chain.Config().IsEIP158(header.Number))
	// No uncle block
	header.UncleHash = types.CalcUncleHash(nil)
	// Assemble and return the final block for sealing
	return types.NewBlock(header, txs, nil, receipts), nil
}

func (a *Alien) automaticMining(number uint64,snap *Snapshot){
	isMainMinerNil := reflect.ValueOf(a.eth.SideMiner("")).IsNil()
	isTimeToChangeSinger := (number+1)%(snap.config.MaxSignerCount*snap.LCRS) == 0
	if a.config.AppId == "" && isTimeToChangeSinger && !isMainMinerNil && a.eth.IsMining() {
		sideMap := rawdb.ReadAllChainConfig(a.db)
		for id := range sideMap {
			// 检查appid，处理小于等于100的appid
			appid, err := strconv.ParseUint(id, 10, 64)
			if err != nil || appid > 100 {continue}
			// 如果没有引入则引入侧链
			chain, ok := a.eth.SideBlockChain(id)
			if !ok {
				a.eth.NewSideChain(true, id)
				continue
			}
			sideSnap := getSnapshot(chain)
			if sideSnap == nil {continue}
			isSideMining := a.eth.SideMiner(id).Mining()
			// 候选人==nil：开始挖矿
			if len(sideSnap.buildTallySlice()) == 0 {
				if !isSideMining {
					a.eth.NewPassiveMiner(id)
				}
				continue
			}
			// 候选人!=nil：停止挖矿
			if isSideMining && a.eth.SideMiner(id).GetIsPassive() {
				a.eth.SideMiner(id).Stop()
			}
		}
	}
}

// Authorize injects a private key into the consensus engine to mint new blocks with.
func (a *Alien) Authorize(signer common.Address, signFn SignerFn) {
	a.lock.Lock()
	defer a.lock.Unlock()

	a.signer = signer
	a.signFn = signFn
}

func (a *Alien) SignTx(signTxFn SignTxFn) {
	a.lock.Lock()
	defer a.lock.Unlock()
	a.signTxFn = signTxFn
}

// Seal implements consensus.Engine, attempting to create a sealed block using
// the local signing credentials.
func (a *Alien) Seal(chain consensus.ChainReader, block *types.Block, stop <-chan struct{}) (*types.Block, error) {
	header := block.Header()
	// Sealing the genesis block is not supported
	number := header.Number.Uint64()
	if number == 0 {
		return nil, errUnknownBlock
	}

	// For 0-period chains, refuse to seal empty blocks (no reward but would spin sealing)
	if a.config.Period == 0 && len(block.Transactions()) == 0 {
		return nil, errWaitTransactions
	}
	// Don't hold the signer fields for the entire sealing procedure
	a.lock.RLock()
	signer, signFn := a.signer, a.signFn
	a.lock.RUnlock()

	// Bail out if we're unauthorized to sign a block
	snap, err := a.snapshot(chain, number-1, header.ParentHash, nil, nil, DefaultLoopCntRecalculateSigners)
	if err != nil {
		return nil, err
	}

	if !chain.Config().Alien.SideChain {
		if !snap.inturn(signer, header) {
			<-stop
			return nil, errUnauthorized
		}
	} else {
		if !a.mcInturn(chain, signer, header.Time.Uint64()) {
			<-stop
			return nil, errUnauthorized
		}
	}
	// correct the time
	delay := time.Unix(header.Time.Int64(), 0).Sub(time.Now())

	select {
	case <-stop:
		return nil, nil
	case <-time.After(delay):
	}
	// Sign all the things!
	sighash, err := signFn(accounts.Account{Address: signer}, sigHash(header).Bytes())
	if err != nil {
		return nil, err
	}
	copy(header.Extra[len(header.Extra)-extraSeal:], sighash)

	return block.WithSeal(header), nil
}

// CalcDifficulty is the difficulty adjustment algorithm. It returns the difficulty
// that a new block should have based on the previous blocks in the chain and the
// current signer.
func (a *Alien) CalcDifficulty(chain consensus.ChainReader, time uint64, parent *types.Header) *big.Int {

	return new(big.Int).Set(defaultDifficulty)
}

// APIs implements consensus.Engine, returning the user facing RPC API to allow
// controlling the signer voting.
func (a *Alien) APIs(chain consensus.ChainReader) []rpc.API {
	return []rpc.API{{
		Namespace: "alien",
		Version:   "0.1",
		Service:   &API{chain: chain, alien: a},
		Public:    false,
	}}
}

// AccumulateRewards credits the coinbase of the given block with the mining reward.
func accumulateRewards(config *params.ChainConfig, state *state.StateDB, header *types.Header, snap *Snapshot) {
	// Calculate the block reword by year
	blockNumPerYear := SecondsPerYear / config.Alien.Period
	yearCount := header.Number.Uint64() / blockNumPerYear
	blockReward := new(big.Int).Rsh(SignerBlockReward, uint(yearCount))

	if !config.Alien.SideChain {

		minerReward := new(big.Int).Set(blockReward)
		minerReward.Mul(minerReward, big.NewInt(int64(MinerRewardPerThousand)))
		minerReward.Div(minerReward, big.NewInt(1000)) // cause the reward is calculate by cnt per thousand

		votersReward := blockReward.Sub(blockReward, minerReward)

		// rewards for the voters
		for voter, reward := range snap.calculateReward(header.Coinbase, votersReward) {
			state.AddBalance(voter, reward)
		}
		// rewards for the miner
		state.AddBalance(header.Coinbase, minerReward)
	} else {
		state.AddBalance(header.Coinbase, blockReward)
	}
}

// Get the signer missing from last signer till header.Coinbase
func getSignerMissing(lastSigner common.Address, currentSigner common.Address, extra HeaderExtra) []common.Address {

	var signerMissing []common.Address
	recordMissing := false
	for _, signer := range extra.SignerQueue {
		if signer == lastSigner {
			recordMissing = true
			continue
		}
		if signer == currentSigner {
			break
		}
		if recordMissing {
			signerMissing = append(signerMissing, signer)
		}
	}
	return signerMissing
}
func getSnapshot(chain *core.BlockChain) *Snapshot {
	cHeader := chain.CurrentHeader()
	sideSnap, err := chain.Engine().(*Alien).snapshot(chain, cHeader.Number.Uint64(), cHeader.Hash(), nil, nil, DefaultLoopCntRecalculateSigners)
	if sideSnap == nil || err != nil {
		log.Warn("load snapshot failed", "appId", chain.Config().AppId)
		return nil
	}
	return sideSnap
}
