package beefyrelayer

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"

	gethTypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"

	"github.com/snowfork/polkadot-ethereum/relayer/chain"
	"github.com/snowfork/polkadot-ethereum/relayer/chain/ethereum"
	"github.com/snowfork/polkadot-ethereum/relayer/contracts/lightclientbridge"
	"github.com/snowfork/polkadot-ethereum/relayer/workers/beefyrelayer/store"
)

const MaxMessagesPerSend = 10

// Listener streams the Ethereum blockchain for application events
type BeefyEthereumListener struct {
	ethereumConfig    *ethereum.Config
	ethereumConn      *ethereum.Connection
	beefyDB           *store.Database
	lightClientBridge *lightclientbridge.Contract
	beefyMessages     chan<- store.BeefyRelayInfo
	dbMessages        chan<- store.DatabaseCmd
	headers           chan<- chain.Header
	blockWaitPeriod   uint64
	log               *logrus.Entry
}

func NewBeefyEthereumListener(ethereumConfig *ethereum.Config, ethereumConn *ethereum.Connection, beefyDB *store.Database,
	beefyMessages chan<- store.BeefyRelayInfo, dbMessages chan<- store.DatabaseCmd, headers chan<- chain.Header,
	log *logrus.Entry) *BeefyEthereumListener {
	return &BeefyEthereumListener{
		ethereumConfig:  ethereumConfig,
		ethereumConn:    ethereumConn,
		beefyDB:         beefyDB,
		dbMessages:      dbMessages,
		beefyMessages:   beefyMessages,
		headers:         headers,
		blockWaitPeriod: 0,
		log:             log,
	}
}

func (li *BeefyEthereumListener) Start(cxt context.Context, eg *errgroup.Group, descendantsUntilFinal uint64) error {

	// Set up light client bridge contract
	lightClientBridgeContract, err := lightclientbridge.NewContract(common.HexToAddress(li.ethereumConfig.LightClientBridge), li.ethereumConn.GetClient())
	if err != nil {
		return err
	}
	li.lightClientBridge = lightClientBridgeContract

	// Fetch BLOCK_WAIT_PERIOD from light client bridge contract
	blockWaitPeriod, err := li.lightClientBridge.ContractCaller.BLOCKWAITPERIOD(nil)
	if err != nil {
		return err
	}
	li.blockWaitPeriod = blockWaitPeriod.Uint64()

	// If starting block < latest block, sync the Relayer to the latest block
	blockNumber, err := li.ethereumConn.GetClient().BlockNumber(cxt)
	if err != nil {
		return err
	}
	if uint64(li.ethereumConfig.StartBlock) < blockNumber {
		li.log.Info(fmt.Sprintf("Syncing Relayer from block %d...", li.ethereumConfig.StartBlock))
		err := li.pollHistoricEventsAndHeaders(cxt)
		if err != nil {
			return err
		}
		li.log.Info(fmt.Sprintf("Relayer fully synced. Starting live processing on block number %d...", blockNumber))
	}

	// In live mode the relayer processes blocks as they're mined and broadcast
	eg.Go(func() error {
		err := li.pollEventsAndHeaders(cxt, descendantsUntilFinal)
		close(li.headers)
		return err
	})

	return nil
}

func (li *BeefyEthereumListener) pollHistoricEventsAndHeaders(ctx context.Context) error {
	// Load starting block number and latest block number
	blockNumber := li.ethereumConfig.StartBlock
	latestBlockNumber, err := li.ethereumConn.GetClient().BlockNumber(ctx)
	if err != nil {
		return err
	}

	li.processHistoricalInitialVerificationSuccessfulEvents(ctx, blockNumber, latestBlockNumber)
	li.processHistoricalFinalVerificationSuccessfulEvents(ctx, blockNumber, latestBlockNumber)

	return nil
}

func (li *BeefyEthereumListener) pollEventsAndHeaders(ctx context.Context, descendantsUntilFinal uint64) error {
	headers := make(chan *gethTypes.Header, 5)

	li.ethereumConn.GetClient().SubscribeNewHead(ctx, headers)

	for {
		select {
		case <-ctx.Done():
			li.log.Info("Shutting down listener...")
			return ctx.Err()
		case gethheader := <-headers:
			blockNumber := gethheader.Number.Uint64()

			li.processInitialVerificationSuccessfulEvents(ctx, blockNumber)
			li.forwardWitnessedBeefyCommitment(ctx, blockNumber, descendantsUntilFinal)
			li.processInitialVerificationSuccessfulEvents(ctx, blockNumber)
		}
	}
}

// queryInitialVerificationSuccessfulEvents queries ContractInitialVerificationSuccessful events from the LightClientBridge contract
func (li *BeefyEthereumListener) queryInitialVerificationSuccessfulEvents(ctx context.Context, start uint64,
	end *uint64) ([]*lightclientbridge.ContractInitialVerificationSuccessful, error) {
	var events []*lightclientbridge.ContractInitialVerificationSuccessful
	filterOps := bind.FilterOpts{Start: start, End: end, Context: ctx}

	iter, err := li.lightClientBridge.FilterInitialVerificationSuccessful(&filterOps)
	if err != nil {
		return nil, err
	}

	for {
		more := iter.Next()
		if !more {
			err = iter.Error()
			if err != nil {
				return nil, err
			}
			break
		}

		events = append(events, iter.Event)
	}

	return events, nil
}

// processHistoricalInitialVerificationSuccessfulEvents processes historical InitialVerificationSuccessful
// events, updating the status of matched BEEFY justifications in the database
func (li *BeefyEthereumListener) processHistoricalInitialVerificationSuccessfulEvents(ctx context.Context,
	blockNumber, latestBlockNumber uint64) {

	// Query previous InitialVerificationSuccessful events and update the status of BEEFY justifications in database
	events, err := li.queryInitialVerificationSuccessfulEvents(ctx, blockNumber, &latestBlockNumber)
	if err != nil {
		li.log.WithError(err).Error("Failure fetching event logs")
	}

	li.log.Info(fmt.Sprintf(
		"Found %d InitialVerificationSuccessful events between blocks %d-%d",
		len(events), blockNumber, latestBlockNumber),
	)

	for _, event := range events {
		// Fetch validation data from contract using event.ID
		validationData, err := li.lightClientBridge.ContractCaller.ValidationData(nil, event.Id)
		if err != nil {
			li.log.WithError(err).Error(fmt.Sprintf("Error querying validation data for ID %d", event.Id))
		}

		// Attempt to match items in database based on their payload
		itemFoundInDatabase := false
		items := li.beefyDB.GetItemsByStatus(store.CommitmentWitnessed)
		for _, item := range items {
			generatedPayload := li.simulatePayloadGeneration(*item)
			if generatedPayload == validationData.Payload {
				// Update existing database item
				li.log.Info("Updating item status from 'CommitmentWitnessed' to 'InitialVerificationTxConfirmed'")
				instructions := map[string]interface{}{
					"status":                  store.InitialVerificationTxConfirmed,
					"initial_verification_tx": event.Raw.TxHash.Hex(),
					"complete_on_block":       event.Raw.BlockNumber + li.blockWaitPeriod,
				}
				updateCmd := store.NewDatabaseCmd(item, store.Update, instructions)
				li.dbMessages <- updateCmd

				itemFoundInDatabase = true
				break
			}
		}
		if !itemFoundInDatabase {
			// Don't have an existing item to update, therefore we won't be able to build the completion tx
			li.log.Error("BEEFY justification data not found in database for InitialVerificationSuccessful event. Ignoring event.")
		}
	}
}

// processInitialVerificationSuccessfulEvents transitions matched database items from status
// InitialVerificationTxSent to InitialVerificationTxConfirmed
func (li *BeefyEthereumListener) processInitialVerificationSuccessfulEvents(ctx context.Context,
	blockNumber uint64) {

	events, err := li.queryInitialVerificationSuccessfulEvents(ctx, blockNumber, &blockNumber)
	if err != nil {
		li.log.WithError(err).Error("Failure fetching event logs")
	}

	if len(events) > 0 {
		li.log.Info(fmt.Sprintf("Found %d InitialVerificationSuccessful events on block %d", len(events), blockNumber))
	}

	for _, event := range events {
		li.log.WithFields(logrus.Fields{
			"blockHash":   event.Raw.BlockHash.Hex(),
			"blockNumber": event.Raw.BlockNumber,
			"txHash":      event.Raw.TxHash.Hex(),
		}).Info("event information")

		// Only process events emitted by transactions sent from our node
		if event.Prover != li.ethereumConn.GetKP().CommonAddress() {
			continue
		}

		item := li.beefyDB.GetItemByInitialVerificationTxHash(event.Raw.TxHash)
		if item.Status != store.InitialVerificationTxSent {
			continue
		}

		li.log.Info("3: Updating item status from 'InitialVerificationTxSent' to 'InitialVerificationTxConfirmed'")
		instructions := map[string]interface{}{
			"status":            store.InitialVerificationTxConfirmed,
			"complete_on_block": event.Raw.BlockNumber + li.blockWaitPeriod,
		}
		updateCmd := store.NewDatabaseCmd(item, store.Update, instructions)
		li.dbMessages <- updateCmd
	}
}

// queryFinalVerificationSuccessfulEvents queries ContractFinalVerificationSuccessful events from the LightClientBridge contract
func (li *BeefyEthereumListener) queryFinalVerificationSuccessfulEvents(ctx context.Context, start uint64,
	end *uint64) ([]*lightclientbridge.ContractFinalVerificationSuccessful, error) {
	var events []*lightclientbridge.ContractFinalVerificationSuccessful
	filterOps := bind.FilterOpts{Start: start, End: end, Context: ctx}

	iter, err := li.lightClientBridge.FilterFinalVerificationSuccessful(&filterOps)
	if err != nil {
		return nil, err
	}

	for {
		more := iter.Next()
		if !more {
			err = iter.Error()
			if err != nil {
				return nil, err
			}
			break
		}

		events = append(events, iter.Event)
	}

	return events, nil
}

// processHistoricalFinalVerificationSuccessfulEvents processes historical FinalVerificationSuccessful
// events, updating the status of matched BEEFY justifications in the database
func (li *BeefyEthereumListener) processHistoricalFinalVerificationSuccessfulEvents(ctx context.Context,
	blockNumber, latestBlockNumber uint64) {
	// Query previous FinalVerificationSuccessful events and update the status of BEEFY justifications in database
	events, err := li.queryFinalVerificationSuccessfulEvents(ctx, blockNumber, &latestBlockNumber)
	if err != nil {
		li.log.WithError(err).Error("Failure fetching event logs")
	}
	li.log.Info(fmt.Sprintf(
		"Found %d FinalVerificationSuccessful events between blocks %d-%d",
		len(events), blockNumber, latestBlockNumber),
	)

	for _, event := range events {
		// Fetch validation data from contract using event.ID
		validationData, err := li.lightClientBridge.ContractCaller.ValidationData(nil, event.Id)
		if err != nil {
			li.log.WithError(err).Error(fmt.Sprintf("Error querying validation data for ID %d", event.Id))
		}

		// Attempt to match items in database based on their payload
		itemFoundInDatabase := false
		items := li.beefyDB.GetItemsByStatus(store.InitialVerificationTxConfirmed) // TODO: list of statuses
		for _, item := range items {
			generatedPayload := li.simulatePayloadGeneration(*item)
			if generatedPayload == validationData.Payload {
				li.log.Info("Deleting finalized item from the database'")
				deleteCmd := store.NewDatabaseCmd(item, store.Delete, nil)
				li.dbMessages <- deleteCmd

				itemFoundInDatabase = true
				break
			}
		}
		if !itemFoundInDatabase {
			li.log.Error("BEEFY justification data not found in database for FinalVerificationSuccessful event. Ignoring event.")
		}
	}
}

// processFinalVerificationSuccessfulEvents removes finalized commitments from the relayer's BEEFY justification database
func (li *BeefyEthereumListener) processFinalVerificationSuccessfulEvents(ctx context.Context,
	blockNumber uint64) {
	events, err := li.queryFinalVerificationSuccessfulEvents(ctx, blockNumber, &blockNumber)
	if err != nil {
		li.log.WithError(err).Error("Failure fetching event logs")
	}

	if len(events) > 0 {
		li.log.Info(fmt.Sprintf("Found %d FinalVerificationSuccessful events on block %d", len(events), blockNumber))
	}

	for _, event := range events {
		li.log.WithFields(logrus.Fields{
			"blockHash":   event.Raw.BlockHash.Hex(),
			"blockNumber": event.Raw.BlockNumber,
			"txHash":      event.Raw.TxHash.Hex(),
		}).Info("event information")

		if event.Prover != li.ethereumConn.GetKP().CommonAddress() {
			continue
		}

		item := li.beefyDB.GetItemByFinalVerificationTxHash(event.Raw.TxHash)
		if item.Status != store.CompleteVerificationTxSent {
			continue
		}

		li.log.Info("6: Deleting finalized item from the database'")
		deleteCmd := store.NewDatabaseCmd(item, store.Delete, nil)
		li.dbMessages <- deleteCmd
	}
}

// matchGeneratedPayload simulates msg building and payload generation
func (li *BeefyEthereumListener) simulatePayloadGeneration(item store.BeefyRelayInfo) [32]byte {
	beefyJustification, err := item.ToBeefyJustification()
	if err != nil {
		li.log.WithError(fmt.Errorf("Error converting BeefyRelayInfo to BeefyJustification: %s", err.Error()))
	}

	msg, err := beefyJustification.BuildNewSignatureCommitmentMessage(0)
	if err != nil {
		li.log.WithError(err).Error("Error building commitment message")
	}

	return msg.Payload
}

// forwardWitnessedBeefyCommitment forwards witnessed BEEFY commitments to the Ethereum writer
func (li *BeefyEthereumListener) forwardWitnessedBeefyCommitment(ctx context.Context, blockNumber, descendantsUntilFinal uint64) {
	witnessedItems := li.beefyDB.GetItemsByStatus(store.CommitmentWitnessed)
	for _, item := range witnessedItems {
		li.beefyMessages <- *item
	}

	// Mark items ReadyToComplete if the current block number has passed their CompleteOnBlock number
	initialVerificationItems := li.beefyDB.GetItemsByStatus(store.InitialVerificationTxConfirmed)
	if len(initialVerificationItems) > 0 {
		li.log.Info(fmt.Sprintf("Found %d item(s) in database awaiting completion block", len(initialVerificationItems)))
	}
	for _, item := range initialVerificationItems {
		if item.CompleteOnBlock+descendantsUntilFinal <= blockNumber {
			// Fetch intended completion block's hash
			block, err := li.ethereumConn.GetClient().BlockByNumber(ctx, big.NewInt(int64(item.CompleteOnBlock)))
			if err != nil {
				li.log.WithError(err).Error("Failure fetching inclusion block")
			}

			li.log.Info("4: Updating item status from 'InitialVerificationTxConfirmed' to 'ReadyToComplete'")
			item.Status = store.ReadyToComplete
			item.RandomSeed = block.Hash()
			li.beefyMessages <- *item
		}
	}
}
