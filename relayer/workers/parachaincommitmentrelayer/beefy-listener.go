package parachaincommitmentrelayer

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/blake2b"
	"github.com/sirupsen/logrus"
	rpcOffchain "github.com/snowfork/go-substrate-rpc-client/v2/rpc/offchain"
	"github.com/snowfork/go-substrate-rpc-client/v2/types"
	"github.com/wealdtech/go-merkletree"
	"golang.org/x/sync/errgroup"

	"github.com/snowfork/polkadot-ethereum/relayer/chain/parachain"
	"github.com/snowfork/polkadot-ethereum/relayer/chain/relaychain"
	chainTypes "github.com/snowfork/polkadot-ethereum/relayer/substrate"
	"github.com/snowfork/polkadot-ethereum/relayer/workers/beefyrelayer/store"
)

//TODO - put in config
const OUR_PARACHAIN_ID = 200

// TODO: This file is currently listening to the relay chain for new beefy justifications. This is temporary, as in
// a follow up PR, it will be changed to listen to Ethereum for new justifications.
// This can't be done yet, as we still need to add block numbers to the Ethereum proofs being submitted
// to the relay chain light client, but will be done once that's complete.

type MessagePackage struct {
	channelID              chainTypes.ChannelID
	commitmentHash         types.H256
	commitmentMessagesData types.StorageDataRaw
	paraHeadProof          [][32]byte
	mmrProof               types.GenerateMMRProofResponse
}

type BeefyListener struct {
	relaychainConfig    *relaychain.Config
	relaychainConn      *relaychain.Connection
	parachainConnection *parachain.Connection
	messages            chan<- MessagePackage
	log                 *logrus.Entry
}

func NewBeefyListener(
	relaychainConfig *relaychain.Config,
	relaychainConn *relaychain.Connection,
	parachainConnection *parachain.Connection,
	messages chan<- MessagePackage,
	log *logrus.Entry) *BeefyListener {
	return &BeefyListener{
		relaychainConfig:    relaychainConfig,
		relaychainConn:      relaychainConn,
		parachainConnection: parachainConnection,
		messages:            messages,
		log:                 log,
	}
}

func (li *BeefyListener) Start(ctx context.Context, eg *errgroup.Group) error {

	eg.Go(func() error {
		return li.subBeefyJustifications(ctx)
	})

	return nil
}

func (li *BeefyListener) onDone(ctx context.Context) error {
	li.log.Info("Shutting down listener...")
	if li.messages != nil {
		close(li.messages)
	}
	return ctx.Err()
}

func (li *BeefyListener) subBeefyJustifications(ctx context.Context) error {
	ch := make(chan interface{})

	li.log.Info("Subscribing to relay chain light client for new mmr payloads")
	sub, err := li.relaychainConn.GetAPI().Client.Subscribe(context.Background(), "beefy", "subscribeJustifications", "unsubscribeJustifications", "justifications", ch)
	if err != nil {
		panic(err)
	}
	defer sub.Unsubscribe()

	for {
		select {
		case <-ctx.Done():
			return li.onDone(ctx)
		case msg := <-ch:

			signedCommitment := &store.SignedCommitment{}
			err := types.DecodeFromHexString(msg.(string), signedCommitment)
			if err != nil {
				li.log.WithError(err).Error("Failed to decode BEEFY commitment messages")
			}

			blockNumber := signedCommitment.Commitment.BlockNumber

			li.log.WithFields(logrus.Fields{
				"commitmentBlockNumber": blockNumber,
				"payload":               signedCommitment.Commitment.Payload.Hex(),
				"validatorSetID":        signedCommitment.Commitment.ValidatorSetID,
			}).Info("Witnessed a new BEEFY commitment:")
			if len(signedCommitment.Signatures) == 0 {
				li.log.Info("BEEFY commitment has no signatures, skipping...")
				continue
			} else {
				hash := blake2b.Sum256(signedCommitment.Commitment.Bytes())
				li.log.WithFields(logrus.Fields{
					"commitment":       hex.EncodeToString(signedCommitment.Commitment.Bytes()),
					"hashedCommitment": hex.EncodeToString(hash[:]),
				}).Info("Commitment with signatures:")
			}
			li.log.WithField("blockNumber", blockNumber+1).Info("Getting hash for next block")
			nextBlockHash, err := li.relaychainConn.GetAPI().RPC.Chain.GetBlockHash(uint64(blockNumber + 1))
			if err != nil {
				li.log.WithError(err).Error("Failed to get block hash")
			}
			li.log.WithField("blockHash", nextBlockHash.Hex()).Info("Got blockhash")

			// TODO this just queries the latest MMR leaf in the latest MMR and our latest parahead in that leaf.
			// we should ideally be querying the last few leaves in the latest MMR until we find
			// the first parachain block that has not yet been fully processed on ethereum,
			// and then package and relay all newer heads/commitments
			mmrProof := li.GetMMRLeafForBlock(uint64(blockNumber), nextBlockHash)
			allParaHeads, ourParaHead := li.GetAllParaheads(nextBlockHash, OUR_PARACHAIN_ID)

			ourParaHeadProof, err := createParachainHeaderProof(allParaHeads, ourParaHead)
			if err != nil {
				li.log.WithError(err).Error("Failed to create para head proof")
			}

			li.log.WithFields(logrus.Fields{
				"ParachainHeads": mmrProof.Leaf.ParachainHeads.Hex(),
			}).Info("ParachainHeadsParachainHeadsParachainHeads")

			messagePackets, err := li.extractCommitments(ourParaHead, mmrProof, ourParaHeadProof)
			if err != nil {
				li.log.WithError(err).Error("Failed to extract commitment and messages")
			}
			if len(messagePackets) == 0 {
				li.log.Info("Parachain header has no commitment with messages, skipping...")
				continue
			}
			for _, messagePacket := range messagePackets {
				li.log.WithFields(logrus.Fields{
					"channelID":              messagePacket.channelID,
					"commitmentHash":         messagePacket.commitmentHash,
					"commitmentMessagesData": messagePacket.commitmentMessagesData,
					"ourParaHeadProof":       messagePacket.paraHeadProof,
					"mmrProof":               messagePacket.mmrProof,
				}).Info("Beefy Listener emitted new message packet")

				li.messages <- messagePacket
			}
		}
	}
}

func (li *BeefyListener) GetMMRLeafForBlock(
	blockNumber uint64,
	blockHash types.Hash,
) types.GenerateMMRProofResponse {
	li.log.WithFields(logrus.Fields{
		"blockNumber": blockNumber,
		"blockHash":   blockHash.Hex(),
	}).Info("Getting MMR Leaf for block...")
	proofResponse, err := li.relaychainConn.GetAPI().RPC.MMR.GenerateProof(blockNumber, blockHash)
	if err != nil {
		li.log.WithError(err).Error("Failed to generate mmr proof")
	}

	var proofItemsHex = []string{}
	for _, item := range proofResponse.Proof.Items {
		proofItemsHex = append(proofItemsHex, item.Hex())
	}

	li.log.WithFields(logrus.Fields{
		"BlockHash":                       proofResponse.BlockHash.Hex(),
		"Leaf.ParentNumber":               proofResponse.Leaf.ParentNumberAndHash.ParentNumber,
		"Leaf.Hash":                       proofResponse.Leaf.ParentNumberAndHash.Hash.Hex(),
		"Leaf.ParachainHeads":             proofResponse.Leaf.ParachainHeads.Hex(),
		"Leaf.BeefyNextAuthoritySet.ID":   proofResponse.Leaf.BeefyNextAuthoritySet.ID,
		"Leaf.BeefyNextAuthoritySet.Len":  proofResponse.Leaf.BeefyNextAuthoritySet.Len,
		"Leaf.BeefyNextAuthoritySet.Root": proofResponse.Leaf.BeefyNextAuthoritySet.Root.Hex(),
		"Proof.LeafIndex":                 proofResponse.Proof.LeafIndex,
		"Proof.LeafCount":                 proofResponse.Proof.LeafCount,
		"Proof.Items":                     proofItemsHex,
	}).Info("Generated MMR Proof")
	return proofResponse
}

func (li *BeefyListener) GetAllParaheads(blockHash types.Hash, ourParachainId uint32) ([]types.Header, types.Header) {
	none := types.NewOptionU32Empty()
	encoded, err := types.EncodeToBytes(none)
	if err != nil {
		li.log.WithError(err).Error("Error")
	}

	baseParaHeadsStorageKey, err := types.CreateStorageKey(
		li.relaychainConn.GetMetadata(),
		"Paras",
		"Heads", encoded, nil)
	if err != nil {
		li.log.WithError(err).Error("Failed to create parachain header storage key")
	}

	//TODO fix this manual slice.
	// The above types.CreateStorageKey does not give the same base key as polkadotjs needs for getKeys.
	// It has some extra bytes.
	// maybe from the none u32 in golang being wrong, or maybe slightly off CreateStorageKey call? we slice it
	// here as a hack.
	actualBaseParaHeadsStorageKey := baseParaHeadsStorageKey[:32]
	li.log.WithField("actualBaseParaHeadsStorageKey", actualBaseParaHeadsStorageKey.Hex()).Info("actualBaseParaHeadsStorageKey")

	keysResponse, err := li.relaychainConn.GetAPI().RPC.State.GetKeys(actualBaseParaHeadsStorageKey, blockHash)
	if err != nil {
		li.log.WithError(err).Error("Failed to get all parachain keys")
	}

	headersResponse, err := li.relaychainConn.GetAPI().RPC.State.QueryStorage(keysResponse, blockHash, blockHash)
	if err != nil {
		li.log.WithError(err).Error("Failed to get all parachain headers")
	}

	li.log.Info("Got all parachain headers")
	var headers []types.Header
	var ourParachainHeader types.Header
	for _, headerResponse := range headersResponse {
		for _, change := range headerResponse.Changes {

			// TODO fix this manual slice with a proper type decode. only the last few bytes are for the ParaId,
			// not sure what the early ones are for.
			key := change.StorageKey[40:]
			var parachainID types.U32
			if err := types.DecodeFromBytes(key, &parachainID); err != nil {
				li.log.WithError(err).Error("Failed to decode parachain ID")
			}

			li.log.WithField("parachainId", parachainID).Info("Decoding header for parachain")
			var encodableOpaqueHeader types.Bytes
			if err := types.DecodeFromBytes(change.StorageData, &encodableOpaqueHeader); err != nil {
				li.log.WithError(err).Error("Failed to decode MMREncodableOpaqueLeaf")
			}

			var header types.Header
			if err := types.DecodeFromBytes(encodableOpaqueHeader, &header); err != nil {
				li.log.WithError(err).Error("Failed to decode Header")
			}
			li.log.WithFields(logrus.Fields{
				"headerBytes":           fmt.Sprintf("%#x", encodableOpaqueHeader),
				"header.ParentHash":     header.ParentHash.Hex(),
				"header.Number":         header.Number,
				"header.StateRoot":      header.StateRoot.Hex(),
				"header.ExtrinsicsRoot": header.ExtrinsicsRoot.Hex(),
				"header.Digest":         header.Digest,
				"parachainId":           parachainID,
			}).Info("Decoded header for parachain")
			headers = append(headers, header)

			if parachainID == types.U32(ourParachainId) {
				ourParachainHeader = header
			}
		}
	}
	return headers, ourParachainHeader
}

func createParachainHeaderProof(allParaHeads []types.Header, ourParaHead types.Header) ([][32]byte, error) {
	var allParaHeadsBytes [][]byte
	for _, paraHead := range allParaHeads {
		paraHeadBytes, err := types.EncodeToBytes(paraHead)
		if err != nil {
			return [][32]byte{}, err
		}
		allParaHeadsBytes = append(allParaHeadsBytes, paraHeadBytes)
	}
	ourParaHeadBytes, err := types.EncodeToBytes(ourParaHead)
	if err != nil {
		return [][32]byte{}, err
	}

	paraTreeData := make([][]byte, len(allParaHeadsBytes))
	for i, paraHead := range allParaHeadsBytes {
		paraTreeData[i] = paraHead
	}

	// Create the tree
	paraMerkleTree, err := merkletree.NewUsing(paraTreeData, &Keccak256{}, nil)
	if err != nil {
		return [][32]byte{}, err
	}

	// Generate Merkle Proof for our parachain's head
	proof, err := paraMerkleTree.GenerateProof(ourParaHeadBytes)
	if err != nil {
		return [][32]byte{}, err
	}

	// Verify the proof
	root := paraMerkleTree.Root()
	verified, err := merkletree.VerifyProofUsing(ourParaHeadBytes, proof, root, &Keccak256{}, nil)
	if err != nil {
		return [][32]byte{}, err
	}
	if !verified {
		return [][32]byte{}, fmt.Errorf("failed to verify proof")
	}

	proofContents := make([][32]byte, len(proof.Hashes))
	for i, hash := range proof.Hashes {
		var hash32Byte [32]byte
		copy(hash32Byte[:], hash)
		proofContents[i] = hash32Byte
	}

	fmt.Println("parachain-commitment-relayer allParaHeadsBytes", allParaHeadsBytes)
	allParaHeadsBytesHex, _ := types.EncodeToHexString(allParaHeadsBytes)
	fmt.Println("parachain-commitment-relayer allParaHeadsBytesHex", allParaHeadsBytesHex)

	paraHeadBytes0Hex, _ := types.EncodeToHexString(allParaHeadsBytes[0])
	fmt.Println("parachain-commitment-relayer paraHeadBytes0Hex", paraHeadBytes0Hex)
	paraHeadBytes1Hex, _ := types.EncodeToHexString(allParaHeadsBytes[1])
	fmt.Println("parachain-commitment-relayer paraHeadBytes1Hex", paraHeadBytes1Hex)
	fmt.Println("parachain-commitment-relayer paraHeadBytesHex", paraHeadBytes0Hex, paraHeadBytes1Hex)

	fmt.Println("parachain-commitment-relayer ourParaHeadBytes", ourParaHeadBytes)
	ourParaHeadBytesHex, _ := types.EncodeToHexString(ourParaHeadBytes)
	fmt.Println("parachain-commitment-relayer ourParaHeadBytesHex", ourParaHeadBytesHex)
	rootHex, _ := types.EncodeToHexString(root)
	fmt.Println("parachain-commitment-relayer root", rootHex)
	fmt.Println("parachain-commitment-relayer proof", proof)
	fmt.Println("parachain-commitment-relayer len(proof.Hashes)", len(proof.Hashes))
	fmt.Println("parachain-commitment-relayer proofContents", proofContents)
	proofContents0Hex, _ := types.EncodeToHexString(proofContents[0])
	fmt.Println("parachain-commitment-relayer proofContents0Hex", proofContents0Hex)

	return proofContents, nil
}

// Keccak256 is the Keccak256 hashing method
type Keccak256 struct{}

// New creates a new Keccak256 hashing method
func New() *Keccak256 {
	return &Keccak256{}
}

// Hash generates a Keccak256 hash from a byte array
func (h *Keccak256) Hash(data []byte) []byte {
	hash := crypto.Keccak256(data)
	return hash[:]
}

func (li *BeefyListener) extractCommitments(
	header types.Header,
	mmrProof types.GenerateMMRProofResponse,
	ourParaHeadProof [][32]byte) ([]MessagePackage, error) {

	li.log.WithFields(logrus.Fields{
		"blockNumber": header.Number,
	}).Debug("Extracting commitment from parachain header")

	auxDigestItems, err := getAuxiliaryDigestItems(header.Digest)
	if err != nil {
		return nil, err
	}

	var messagePackages []MessagePackage
	for _, auxDigestItem := range auxDigestItems {
		li.log.WithFields(logrus.Fields{
			"block":          header.Number,
			"channelID":      auxDigestItem.AsCommitment.ChannelID,
			"commitmentHash": auxDigestItem.AsCommitment.Hash.Hex(),
		}).Debug("Found commitment hash in header digest")
		commitmentHash := auxDigestItem.AsCommitment.Hash
		commitmentMessagesData, err := li.getMessagesDataForDigestItem(&auxDigestItem)
		if err != nil {
			return nil, err
		}
		messagePackage := MessagePackage{
			auxDigestItem.AsCommitment.ChannelID,
			commitmentHash,
			commitmentMessagesData,
			ourParaHeadProof,
			mmrProof,
		}
		messagePackages = append(messagePackages, messagePackage)
	}

	return messagePackages, nil
}

func getAuxiliaryDigestItems(digest types.Digest) ([]chainTypes.AuxiliaryDigestItem, error) {
	var auxDigestItems []chainTypes.AuxiliaryDigestItem
	for _, digestItem := range digest {
		if digestItem.IsOther {
			var auxDigestItem chainTypes.AuxiliaryDigestItem
			err := types.DecodeFromBytes(digestItem.AsOther, &auxDigestItem)
			if err != nil {
				return nil, err
			}
			auxDigestItems = append(auxDigestItems, auxDigestItem)
		}
	}
	return auxDigestItems, nil
}

func (li *BeefyListener) getMessagesDataForDigestItem(digestItem *chainTypes.AuxiliaryDigestItem) (types.StorageDataRaw, error) {
	storageKey, err := parachain.MakeStorageKey(digestItem.AsCommitment.ChannelID, digestItem.AsCommitment.Hash)
	if err != nil {
		return nil, err
	}

	data, err := li.parachainConnection.GetAPI().RPC.Offchain.LocalStorageGet(rpcOffchain.Persistent, storageKey)
	if err != nil {
		li.log.WithError(err).Error("Failed to read commitment from offchain storage")
		return nil, err
	}

	if data != nil {
		li.log.WithFields(logrus.Fields{
			"commitmentSizeBytes": len(*data),
		}).Debug("Retrieved commitment from offchain storage")
	} else {
		li.log.WithError(err).Error("Commitment not found in offchain storage")
		return nil, err
	}

	return *data, nil
}
