package parachain

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	log "github.com/sirupsen/logrus"
	gsrpc "github.com/snowfork/go-substrate-rpc-client/v4"
	"github.com/snowfork/go-substrate-rpc-client/v4/types"
	"github.com/snowfork/snowbridge/relayer/chain/ethereum"
	"github.com/snowfork/snowbridge/relayer/chain/parachain"
	"github.com/snowfork/snowbridge/relayer/chain/relaychain"
	"github.com/snowfork/snowbridge/relayer/contracts/basic"
)

type Scanner struct {
	config           *SourceConfig
	ethConn          *ethereum.Connection
	relayConn        *relaychain.Connection
	paraConn         *parachain.Connection
	paraID           uint32
	tasks            chan<- *Task
	eventQueryClient QueryClient
	accounts         [][32]byte
}

// Scans for all parachain message commitments that need to be relayed and can be proven
// using the MMR root at the specified beefyBlockNumber of the relay chain.
//
// The algorithm works roughly like this:
//  1. Fetch channel nonces on both sides of the bridge and compare them
//  2. If the nonces on the parachain side are larger that means messages
//     need to be relayed. If not then exit early.
//  3. Scan parachain blocks to figure out exactly which commitments need to be relayed.
//  4. For all the parachain blocks with unsettled commitments, determine the relay chain
//     block number in which the parachain block was included.
func (s *Scanner) Scan(ctx context.Context, beefyBlockNumber uint64) ([]*Task, error) {
	// fetch last parachain header that was finalized *before* the BEEFY block
	beefyBlockMinusOneHash, err := s.relayConn.API().RPC.Chain.GetBlockHash(uint64(beefyBlockNumber - 1))
	if err != nil {
		return nil, fmt.Errorf("fetch block hash for block %v: %w", beefyBlockNumber, err)
	}
	var paraHead types.Header
	ok, err := s.relayConn.FetchParachainHead(beefyBlockMinusOneHash, s.paraID, &paraHead)
	if err != nil {
		return nil, fmt.Errorf("fetch head for parachain %v at block %v: %w", s.paraID, beefyBlockMinusOneHash.Hex(), err)
	}
	if !ok {
		return nil, fmt.Errorf("parachain %v is not registered", s.paraID)
	}

	paraBlockNumber := uint64(paraHead.Number)
	paraBlockHash, err := s.paraConn.API().RPC.Chain.GetBlockHash(paraBlockNumber)
	if err != nil {
		return nil, fmt.Errorf("fetch parachain block hash for block %v: %w", paraBlockNumber, err)
	}

	tasks, err := s.findTasks(ctx, paraBlockNumber, paraBlockHash)
	if err != nil {
		return nil, err
	}

	return tasks, nil
}

type AccountNonces struct {
	account                       [32]byte
	paraBasicNonce, ethBasicNonce uint64
}

// findTasks finds all the message commitments which need to be relayed
func (s *Scanner) findTasks(
	ctx context.Context,
	paraBlock uint64,
	paraHash types.Hash,
) ([]*Task, error) {
	basicContract, err := basic.NewBasicInboundChannel(common.HexToAddress(
		s.config.Contracts.BasicInboundChannel),
		s.ethConn.Client(),
	)
	if err != nil {
		return nil, err
	}

	options := bind.CallOpts{
		Pending: true,
		Context: ctx,
	}

	basicChannelAccountNoncesToFind := make(map[types.AccountID]uint64, len(s.accounts))
	for _, account := range s.accounts {
		ethBasicNonce, err := basicContract.Nonce(&options, account)
		if err != nil {
			return nil, err
		}
		log.WithFields(log.Fields{
			"nonce":   ethBasicNonce,
			"account": types.HexEncodeToString(account[:]),
		}).Info("Checked latest nonce delivered to ethereum basic channel")

		paraBasicNonceKey, err := types.CreateStorageKey(s.paraConn.Metadata(), "BasicOutboundChannel", "Nonce", account[:], nil)
		if err != nil {
			return nil, fmt.Errorf("create storage key for account '%v': %w", types.HexEncodeToString(account[:]), err)
		}
		var paraBasicNonce types.U64
		ok, err := s.paraConn.API().RPC.State.GetStorage(paraBasicNonceKey, &paraBasicNonce, paraHash)
		if err != nil {
			log.Error(err)
			return nil, err
		}
		if !ok {
			paraBasicNonce = 0
		}
		log.WithFields(log.Fields{
			"nonce":   uint64(paraBasicNonce),
			"account": types.HexEncodeToString(account[:]),
		}).Info("Checked latest nonce generated by parachain basic channel")

		if uint64(paraBasicNonce) > ethBasicNonce {
			basicChannelAccountNoncesToFind[account] = ethBasicNonce + 1
		}
	}

	if len(basicChannelAccountNoncesToFind) == 0 {
		return nil, nil
	}

	log.Info("Nonces are mismatched, scanning for commitments that need to be relayed")

	tasks, err := s.findTasksImpl(
		ctx,
		paraBlock,
		basicChannelAccountNoncesToFind,
	)
	if err != nil {
		return nil, err
	}

	s.gatherProofInputs(tasks)

	return tasks, nil
}

// Searches for all lost commitments on the basic channel from the given parachain block number backwards
// until it finds the given nonces
func (s *Scanner) findTasksImpl(
	ctx context.Context,
	lastParaBlockNumber uint64,
	basicChannelAccountNonces map[types.AccountID]uint64,
) ([]*Task, error) {
	basicChannelAccountNonceString := "map["
	for account, nonce := range basicChannelAccountNonces {
		basicChannelAccountNonceString += fmt.Sprintf("%v: %v ", hex.EncodeToString(account[:]), nonce)
	}
	basicChannelAccountNonceString = strings.Trim(basicChannelAccountNonceString, " ")
	basicChannelAccountNonceString += "]"

	log.WithFields(log.Fields{
		"basicAccountNonces": basicChannelAccountNonceString,
		"latestblockNumber":  lastParaBlockNumber,
	}).Debug("Searching backwards from latest block on parachain to find block with nonces")

	currentBlockNumber := lastParaBlockNumber

	basicChannelScanAccounts := make(map[types.AccountID]bool, len(basicChannelAccountNonces))
	for account := range basicChannelAccountNonces {
		basicChannelScanAccounts[account] = true
	}
	scanBasicChannelDone := len(basicChannelScanAccounts) == 0

	var tasks []*Task

	for !scanBasicChannelDone && currentBlockNumber > 0 {
		log.WithFields(log.Fields{
			"blockNumber": currentBlockNumber,
		}).Debug("Checking header")

		blockHash, err := s.paraConn.API().RPC.Chain.GetBlockHash(currentBlockNumber)
		if err != nil {
			return nil, fmt.Errorf("fetch blockhash for block %v: %w", currentBlockNumber, err)
		}

		header, err := s.paraConn.API().RPC.Chain.GetHeader(blockHash)
		if err != nil {
			return nil, fmt.Errorf("fetch header for %v: %w", blockHash.Hex(), err)
		}

		digestItems, err := ExtractAuxiliaryDigestItems(header.Digest)
		if err != nil {
			return nil, err
		}

		if len(digestItems) == 0 {
			currentBlockNumber--
			continue
		}

		basicChannelProofs := make([]BundleProof, 0, len(basicChannelAccountNonces))

		events, err := s.eventQueryClient.QueryEvent(ctx, s.config.Parachain.Endpoint, blockHash)
		if err != nil {
			return nil, fmt.Errorf("query events: %w", err)
		}

		for _, digestItem := range digestItems {
			if !digestItem.IsCommitment {
				continue
			}

			if !scanBasicChannelDone {
				if events == nil {
					return nil, fmt.Errorf("event basicOutboundChannel.Committed not found in block")
				}

				digestItemHash := digestItem.AsCommitment.Hash
				if events.Hash != digestItemHash {
					return nil, fmt.Errorf("basic channel commitment hash in digest item does not match the one in the Committed event")
				}

				// For basic channel commit hash is the merkle root calculated from bundles
				// https://github.com/Snowfork/snowbridge/blob/75a475cbf8fc8e13577ad6b773ac452b2bf82fbb/parachain/pallets/basic-channel/src/outbound/mod.rs#L275-L277
				// to verify it we fetch bundle proof from parachain
				result, err := scanForBasicChannelProofs(
					s.paraConn.API(),
					digestItemHash,
					basicChannelAccountNonces,
					basicChannelScanAccounts,
					events.Bundles,
				)
				if err != nil {
					return nil, err
				}
				basicChannelProofs = result.proofs
				scanBasicChannelDone = result.scanDone
			}
		}

		if len(basicChannelProofs) > 0 {
			task := Task{
				Header:             header,
				BasicChannelProofs: &basicChannelProofs,
				ProofInput:         nil,
				ProofOutput:        nil,
			}
			tasks = append(tasks, &task)
		}

		currentBlockNumber--
	}

	// sort tasks by ascending block number
	sort.SliceStable(tasks, func(i, j int) bool {
		return tasks[i].Header.Number < tasks[j].Header.Number
	})

	return tasks, nil
}

type PersistedValidationData struct {
	ParentHead             []byte
	RelayParentNumber      uint32
	RelayParentStorageRoot types.Hash
	MaxPOVSize             uint32
}

// For each task, gatherProofInputs will search to find the relay chain block
// in which that header was included as well as the parachain heads for that block.
func (s *Scanner) gatherProofInputs(
	tasks []*Task,
) error {
	for _, task := range tasks {

		log.WithFields(log.Fields{
			"ParaBlockNumber": task.Header.Number,
		}).Debug("Gathering proof inputs for parachain header")

		relayBlockNumber, err := s.findInclusionBlockNumber(uint64(task.Header.Number))
		if err != nil {
			return fmt.Errorf("find inclusion block number for parachain block %v: %w", task.Header.Number, err)
		}

		relayBlockHash, err := s.relayConn.API().RPC.Chain.GetBlockHash(relayBlockNumber)
		if err != nil {
			return fmt.Errorf("fetch relaychain block hash: %w", err)
		}

		parachainHeads, err := s.relayConn.FetchParachainHeads(relayBlockHash)
		if err != nil {
			return fmt.Errorf("fetch parachain heads: %w", err)
		}

		task.ProofInput = &ProofInput{
			ParaID:           s.paraID,
			RelayBlockNumber: relayBlockNumber,
			ParaHeads:        parachainHeads,
		}
	}

	return nil
}

// The process for finalizing a backed parachain header times out after these many blocks:
const FinalizationTimeout = 4

// Find the relaychain block in which a parachain header was included (finalized). This usually happens
// 2-3 blocks after the relaychain block in which the parachain header was backed.
func (s *Scanner) findInclusionBlockNumber(
	paraBlockNumber uint64,
) (uint64, error) {
	validationDataKey, err := types.CreateStorageKey(s.paraConn.Metadata(), "ParachainSystem", "ValidationData", nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create storage key: %w", err)
	}

	paraBlockHash, err := s.paraConn.API().RPC.Chain.GetBlockHash(paraBlockNumber)
	if err != nil {
		return 0, fmt.Errorf("fetch parachain block hash: %w", err)
	}

	var validationData PersistedValidationData
	ok, err := s.paraConn.API().RPC.State.GetStorage(validationDataKey, &validationData, paraBlockHash)
	if err != nil {
		return 0, fmt.Errorf("fetch PersistedValidationData for block %v: %w", paraBlockHash.Hex(), err)
	}
	if !ok {
		return 0, fmt.Errorf("PersistedValidationData not found for block %v", paraBlockHash.Hex())
	}

	startBlock := validationData.RelayParentNumber + 1
	for i := validationData.RelayParentNumber + 1; i < startBlock+FinalizationTimeout; i++ {
		relayBlockHash, err := s.relayConn.API().RPC.Chain.GetBlockHash(uint64(i))
		if err != nil {
			return 0, fmt.Errorf("fetch relaychain block hash: %w", err)
		}

		var paraHead types.Header
		ok, err := s.relayConn.FetchParachainHead(relayBlockHash, s.paraID, &paraHead)
		if err != nil {
			return 0, fmt.Errorf("fetch head for parachain %v at block %v: %w", s.paraID, relayBlockHash.Hex(), err)
		}
		if !ok {
			return 0, fmt.Errorf("parachain %v is not registered", s.paraID)
		}

		if paraBlockNumber == uint64(paraHead.Number) {
			return uint64(i), nil
		}
	}

	return 0, fmt.Errorf("scan terminated")
}

func scanForBasicChannelProofs(
	api *gsrpc.SubstrateAPI,
	digestItemHash types.H256,
	basicChannelAccountNonces map[types.AccountID]uint64,
	basicChannelScanAccounts map[types.AccountID]bool,
	bundles []BasicOutboundChannelMessageBundle,
) (*struct {
	proofs   []BundleProof
	scanDone bool
}, error) {
	var scanBasicChannelDone bool
	basicChannelProofs := make([]BundleProof, 0, len(basicChannelAccountNonces))

	for bundleIndex, bundle := range bundles {
		_, shouldCheckAccount := basicChannelScanAccounts[bundle.Account]
		if !shouldCheckAccount {
			continue
		}

		nonceToFind := basicChannelAccountNonces[bundle.Account]
		bundleNonceBigInt := big.Int(bundle.Nonce)
		bundleNonce := bundleNonceBigInt.Uint64()

		// This case will be hit if basicNonceToFind has not been committed yet.
		// Channels emit commitments every N blocks.
		if bundleNonce < nonceToFind {
			log.Debugf(
				"Halting scan for account '%v'. Messages not committed yet on basic channel",
				types.HexEncodeToString(bundle.Account[:]),
			)
			scanBasicChannelDone = markAccountScanDone(basicChannelScanAccounts, bundle.Account)
			continue
		}

		basicChannelBundleProof, err := fetchBundleProof(api, digestItemHash, bundleIndex, bundle)
		if err != nil {
			return nil, err
		}
		// check merkle root calculated from bundle proof is same as the digest hash from header
		if basicChannelBundleProof.Proof.Root != digestItemHash {
			log.Warnf(
				"Halting scan for account '%v'. Basic channel proof root doesn't match digest item's commitment hash",
				types.HexEncodeToString(bundle.Account[:]),
			)
			scanBasicChannelDone = markAccountScanDone(basicChannelScanAccounts, bundle.Account)
			continue
		}

		if bundleNonce > nonceToFind {
			// Collect these commitments
			basicChannelProofs = append(basicChannelProofs, basicChannelBundleProof)
		} else if bundleNonce == nonceToFind {
			// Collect this commitment and terminate scan
			basicChannelProofs = append(basicChannelProofs, basicChannelBundleProof)
			scanBasicChannelDone = markAccountScanDone(basicChannelScanAccounts, bundle.Account)
		}
	}

	return &struct {
		proofs   []BundleProof
		scanDone bool
	}{
		proofs:   basicChannelProofs,
		scanDone: scanBasicChannelDone,
	}, nil
}

func markAccountScanDone(scanBasicChannelAccounts map[types.AccountID]bool, accountID types.AccountID) bool {
	delete(scanBasicChannelAccounts, accountID)
	return len(scanBasicChannelAccounts) == 0
}

func fetchBundleProof(
	api *gsrpc.SubstrateAPI,
	commitmentHash types.H256,
	bundleIndex int,
	bundle BasicOutboundChannelMessageBundle,
) (BundleProof, error) {
	var proofHex string
	var rawProof RawMerkleProof
	var bundleProof BundleProof

	commitmentHashHex, err := types.EncodeToHexString(commitmentHash)
	if err != nil {
		return bundleProof, fmt.Errorf("encode commitmentHash(%v): %w", commitmentHash, err)
	}

	err = api.Client.Call(&proofHex, "basicOutboundChannel_getMerkleProof", commitmentHashHex, bundleIndex)
	if err != nil {
		return bundleProof, fmt.Errorf("call rpc basicOutboundChannel_getMerkleProof(%v, %v): %w", commitmentHash, bundleIndex, err)
	}

	err = types.DecodeFromHexString(proofHex, &rawProof)
	if err != nil {
		return bundleProof, fmt.Errorf("decode merkle proof: %w", err)
	}

	proof, err := NewMerkleProof(rawProof)
	if err != nil {
		return bundleProof, fmt.Errorf("decode merkle proof: %w", err)
	}

	return BundleProof{Bundle: bundle, Proof: proof}, nil
}
