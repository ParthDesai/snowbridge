package parachaincommitment

import (
	"context"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/sirupsen/logrus"
	"github.com/snowfork/go-substrate-rpc-client/v2/types"
	"github.com/snowfork/polkadot-ethereum/relayer/contracts/inbound"
	"github.com/snowfork/polkadot-ethereum/relayer/substrate"
	chainTypes "github.com/snowfork/polkadot-ethereum/relayer/substrate"
)

// Catches up by searching for and relaying all missed commitments before the given block
func (li *Listener) catchupMissedCommitments(ctx context.Context, latestBlock uint64) error {
	basicContract, err := inbound.NewContract(common.HexToAddress(
		li.ethereumConfig.Channels.Basic.Inbound),
		li.ethereumConnection.GetClient(),
	)
	if err != nil {
		return err
	}

	incentivizedContract, err := inbound.NewContract(common.HexToAddress(
		li.ethereumConfig.Channels.Incentivized.Inbound),
		li.ethereumConnection.GetClient(),
	)
	if err != nil {
		return err
	}

	options := bind.CallOpts{
		Pending: true,
		Context: ctx,
	}

	ethBasicNonce, err := basicContract.Nonce(&options)
	if err != nil {
		return err
	}
	li.log.WithFields(logrus.Fields{
		"nonce": ethBasicNonce,
	}).Info("Checked latest nonce delivered to ethereum basic channel")

	ethIncentivizedNonce, err := incentivizedContract.Nonce(&options)
	if err != nil {
		return err
	}
	li.log.WithFields(logrus.Fields{
		"nonce": ethIncentivizedNonce,
	}).Info("Checked latest nonce delivered to ethereum incentivized channel")

	paraBasicNonceKey, err := types.CreateStorageKey(li.parachainConnection.GetMetadata(), "BasicOutboundModule", "Nonce", nil, nil)
	if err != nil {
		li.log.Error(err)
		return err
	}
	var paraBasicNonce types.U64
	ok, err := li.parachainConnection.GetAPI().RPC.State.GetStorageLatest(paraBasicNonceKey, &paraBasicNonce)
	if err != nil {
		li.log.Error(err)
		return err
	}
	if !ok {
		paraBasicNonce = 0
	}
	li.log.WithFields(logrus.Fields{
		"nonce": uint64(paraBasicNonce),
	}).Info("Checked latest nonce generated by parachain basic channel")

	paraIncentivizedNonceKey, err := types.CreateStorageKey(li.parachainConnection.GetMetadata(), "IncentivizedOutboundModule", "Nonce", nil, nil)
	if err != nil {
		li.log.Error(err)
		return err
	}
	var paraIncentivizedNonce types.U64
	ok, err = li.parachainConnection.GetAPI().RPC.State.GetStorageLatest(paraIncentivizedNonceKey, &paraIncentivizedNonce)
	if err != nil {
		li.log.Error(err)
		return err
	}
	if !ok {
		paraBasicNonce = 0
	}
	li.log.WithFields(logrus.Fields{
		"nonce": uint64(paraIncentivizedNonce),
	}).Info("Checked latest nonce generated by parachain incentivized channel")

	if ethBasicNonce == uint64(paraBasicNonce) && ethIncentivizedNonce == uint64(paraIncentivizedNonce) {
		return nil
	}

	err = li.searchForLostCommitments(ctx, latestBlock, ethBasicNonce, ethIncentivizedNonce)
	if err != nil {
		return err
	}

	li.log.Info("Stopped searching for lost commitments")

	return nil
}

func (li *Listener) searchForLostCommitments(ctx context.Context, lastBlockNumber uint64, basicNonceToFind uint64, incentivizedNonceToFind uint64) error {
	li.log.WithFields(logrus.Fields{
		"basicNonce":        basicNonceToFind,
		"incentivizedNonce": incentivizedNonceToFind,
		"latestblockNumber": lastBlockNumber,
	}).Debug("Searching backwards from latest block on parachain to find block with nonce")
	basicId := substrate.ChannelID{IsBasic: true}
	incentivizedId := substrate.ChannelID{IsIncentivized: true}

	currentBlockNumber := lastBlockNumber + 1
	basicNonceFound := false
	incentivizedNonceFound := false
	var digestItems []*chainTypes.AuxiliaryDigestItem
	for (basicNonceFound == false || incentivizedNonceFound == false) && currentBlockNumber != 0 {
		currentBlockNumber--
		li.log.WithFields(logrus.Fields{
			"blockNumber": currentBlockNumber,
		}).Debug("Checking header...")

		blockHash, err := li.parachainConnection.GetAPI().RPC.Chain.GetBlockHash(currentBlockNumber)
		if err != nil {
			li.log.WithFields(logrus.Fields{
				"blockNumber": currentBlockNumber,
			}).WithError(err).Error("Failed to fetch blockhash")
			return err
		}

		header, err := li.parachainConnection.GetAPI().RPC.Chain.GetHeader(blockHash)
		if err != nil {
			li.log.WithError(err).Error("Failed to fetch header")
			return err
		}

		digestItem, err := getAuxiliaryDigestItem(header.Digest)
		if err != nil {
			return err
		}

		if digestItem != nil && digestItem.IsCommitment {
			channelID := digestItem.AsCommitment.ChannelID
			if channelID == basicId && !basicNonceFound {
				isRelayed, err := li.checkIfDigestItemContainsNonce(digestItem, basicNonceToFind)
				if err != nil {
					return err
				}
				if isRelayed {
					basicNonceFound = true
				} else {
					digestItems = append(digestItems, digestItem)
				}
			}
			if channelID == incentivizedId && !incentivizedNonceFound {
				isRelayed, err := li.checkIfDigestItemContainsNonce(digestItem, incentivizedNonceToFind)
				if err != nil {
					return err
				}
				if isRelayed {
					incentivizedNonceFound = true
				} else {
					digestItems = append(digestItems, digestItem)
				}
			}
		}

	}

	// Reverse items
	for i, j := 0, len(digestItems)-1; i < j; i, j = i+1, j-1 {
		digestItems[i], digestItems[j] = digestItems[j], digestItems[i]
	}

	for _, digestItem := range digestItems {
		err := li.processDigestItem(ctx, digestItem)
		if err != nil {
			return err
		}
	}

	return nil
}

func (li *Listener) checkIfDigestItemContainsNonce(
	digestItem *chainTypes.AuxiliaryDigestItem, nonceToFind uint64) (bool, error) {
	messages, err := li.getMessagesForDigestItem(digestItem)
	if err != nil {
		return false, err
	}

	for _, message := range messages {
		if message.Nonce <= nonceToFind {
			return true, nil
		}
	}
	return false, nil
}
