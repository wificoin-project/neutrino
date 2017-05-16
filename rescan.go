// NOTE: THIS API IS UNSTABLE RIGHT NOW.

package neutrino

import (
	"bytes"
	"fmt"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcrpcclient"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/gcs"
	"github.com/btcsuite/btcutil/gcs/builder"
	"github.com/btcsuite/btcwallet/waddrmgr"
)

// Relevant package-level variables live here
var ()

// Functional parameters for Rescan
type rescanOptions struct {
	chain          *ChainService
	queryOptions   []QueryOption
	ntfn           btcrpcclient.NotificationHandlers
	startBlock     *waddrmgr.BlockStamp
	endBlock       *waddrmgr.BlockStamp
	watchAddrs     []btcutil.Address
	watchOutPoints []wire.OutPoint
	watchTxIDs     []chainhash.Hash
	watchList      [][]byte
	txIdx          uint32
	update         <-chan *updateOptions
	quit           <-chan struct{}
}

// RescanOption is a functional option argument to any of the rescan and
// notification subscription methods. These are always processed in order, with
// later options overriding earlier ones.
type RescanOption func(ro *rescanOptions)

func defaultRescanOptions() *rescanOptions {
	return &rescanOptions{}
}

// QueryOptions pass onto the underlying queries.
func QueryOptions(options ...QueryOption) RescanOption {
	return func(ro *rescanOptions) {
		ro.queryOptions = options
	}
}

// NotificationHandlers specifies notification handlers for the rescan. These
// will always run in the same goroutine as the caller.
func NotificationHandlers(ntfn btcrpcclient.NotificationHandlers) RescanOption {
	return func(ro *rescanOptions) {
		ro.ntfn = ntfn
	}
}

// StartBlock specifies the start block. The hash is checked first; if there's
// no such hash (zero hash avoids lookup), the height is checked next. If
// the height is 0 or the start block isn't specified, starts from the genesis
// block. This block is assumed to already be known, and no notifications will
// be sent for this block.
func StartBlock(startBlock *waddrmgr.BlockStamp) RescanOption {
	return func(ro *rescanOptions) {
		ro.startBlock = startBlock
	}
}

// EndBlock specifies the end block. The hash is checked first; if there's no
// such hash (zero hash avoids lookup), the height is checked next. If the
// height is 0 or in the future or the end block isn't specified, the quit
// channel MUST be specified as Rescan will sync to the tip of the blockchain
// and continue to stay in sync and pass notifications. This is enforced at
// runtime.
func EndBlock(endBlock *waddrmgr.BlockStamp) RescanOption {
	return func(ro *rescanOptions) {
		ro.endBlock = endBlock
	}
}

// WatchAddrs specifies the addresses to watch/filter for. Each call to this
// function adds to the list of addresses being watched rather than replacing
// the list. Each time a transaction spends to the specified address, the
// outpoint is added to the WatchOutPoints list.
func WatchAddrs(watchAddrs ...btcutil.Address) RescanOption {
	return func(ro *rescanOptions) {
		ro.watchAddrs = append(ro.watchAddrs, watchAddrs...)
	}
}

// WatchOutPoints specifies the outpoints to watch for on-chain spends. Each
// call to this function adds to the list of outpoints being watched rather
// than replacing the list.
func WatchOutPoints(watchOutPoints ...wire.OutPoint) RescanOption {
	return func(ro *rescanOptions) {
		ro.watchOutPoints = append(ro.watchOutPoints, watchOutPoints...)
	}
}

// WatchTxIDs specifies the outpoints to watch for on-chain spends. Each
// call to this function adds to the list of outpoints being watched rather
// than replacing the list.
func WatchTxIDs(watchTxIDs ...chainhash.Hash) RescanOption {
	return func(ro *rescanOptions) {
		ro.watchTxIDs = append(ro.watchTxIDs, watchTxIDs...)
	}
}

// TxIdx specifies a hint transaction index into the block in which the UTXO
// is created (eg, coinbase is 0, next transaction is 1, etc.)
func TxIdx(txIdx uint32) RescanOption {
	return func(ro *rescanOptions) {
		ro.txIdx = txIdx
	}
}

// QuitChan specifies the quit channel. This can be used by the caller to let
// an indefinite rescan (one with no EndBlock set) know it should gracefully
// shut down. If this isn't specified, an end block MUST be specified as Rescan
// must know when to stop. This is enforced at runtime.
func QuitChan(quit <-chan struct{}) RescanOption {
	return func(ro *rescanOptions) {
		ro.quit = quit
	}
}

// updateChan specifies an update channel. This is for internal use by the
// Rescan.Update functionality.
func updateChan(update <-chan *updateOptions) RescanOption {
	return func(ro *rescanOptions) {
		ro.update = update
	}
}

// Rescan is a single-threaded function that uses headers from the database and
// functional options as arguments.
func (s *ChainService) Rescan(options ...RescanOption) error {
	ro := defaultRescanOptions()
	ro.endBlock = &waddrmgr.BlockStamp{
		Hash:   *s.chainParams.GenesisHash,
		Height: 0,
	}
	for _, option := range options {
		option(ro)
	}
	ro.chain = s

	// If we have something to watch, create a watch list.
	for _, addr := range ro.watchAddrs {
		ro.watchList = append(ro.watchList, addr.ScriptAddress())
	}
	for _, op := range ro.watchOutPoints {
		ro.watchList = append(ro.watchList,
			builder.OutPointToFilterEntry(op))
	}
	for _, txid := range ro.watchTxIDs {
		ro.watchList = append(ro.watchList, txid[:])
	}
	if len(ro.watchList) == 0 {
		return fmt.Errorf("Rescan must specify addresses and/or " +
			"outpoints and/or TXIDs to watch")
	}

	// Check that we have either an end block or a quit channel.
	if ro.endBlock != nil {
		if (ro.endBlock.Hash != chainhash.Hash{}) {
			_, height, err := s.GetBlockByHash(ro.endBlock.Hash)
			if err != nil {
				ro.endBlock.Hash = chainhash.Hash{}
			} else {
				ro.endBlock.Height = int32(height)
			}
		}
		if (ro.endBlock.Hash == chainhash.Hash{}) {
			if ro.endBlock.Height != 0 {
				header, err := s.GetBlockByHeight(
					uint32(ro.endBlock.Height))
				if err == nil {
					ro.endBlock.Hash = header.BlockHash()
				} else {
					ro.endBlock = &waddrmgr.BlockStamp{}
				}
			}
		}
	} else {
		ro.endBlock = &waddrmgr.BlockStamp{}
	}
	if ro.quit == nil && ro.endBlock.Height == 0 {
		return fmt.Errorf("Rescan request must specify a quit channel" +
			" or valid end block")
	}

	// Track our position in the chain.
	var curHeader wire.BlockHeader
	curStamp := *ro.startBlock

	// Find our starting block.
	if (curStamp.Hash != chainhash.Hash{}) {
		header, height, err := s.GetBlockByHash(curStamp.Hash)
		if err == nil {
			curHeader = header
			curStamp.Height = int32(height)
		} else {
			curStamp.Hash = chainhash.Hash{}
		}
	}
	if (curStamp.Hash == chainhash.Hash{}) {
		if curStamp.Height == 0 {
			curStamp.Hash = *s.chainParams.GenesisHash
		} else {
			header, err := s.GetBlockByHeight(
				uint32(curStamp.Height))
			if err == nil {
				curHeader = header
				curStamp.Hash = curHeader.BlockHash()
			} else {
				curHeader =
					s.chainParams.GenesisBlock.Header
				curStamp.Hash =
					*s.chainParams.GenesisHash
				curStamp.Height = 0
			}
		}
	}
	log.Tracef("Starting rescan from known block %d (%s)", curStamp.Height,
		curStamp.Hash)

	// Listen for notifications.
	blockConnected := make(chan wire.BlockHeader)
	blockDisconnected := make(chan wire.BlockHeader)
	subscription := blockSubscription{
		onConnectBasic: blockConnected,
		onDisconnect:   blockDisconnected,
		quit:           ro.quit,
	}

	// Loop through blocks, one at a time. This relies on the underlying
	// ChainService API to send blockConnected and blockDisconnected
	// notifications in the correct order.
	current := false
rescanLoop:
	for {
		// If we're current, we wait for notifications.
		if current {
			// Wait for a signal that we have a newly connected
			// header and cfheader, or a newly disconnected header;
			// alternatively, forward ourselves to the next block
			// if possible.
			select {
			case <-ro.quit:
				s.unsubscribeBlockMsgs(subscription)
				return nil
			case update := <-ro.update:
				rewound, err := ro.updateFilter(update,
					&curStamp, &curHeader)
				if err != nil {
					return err
				}
				if rewound {
					current = false
				}
			case header := <-blockConnected:
				// Only deal with the next block from what we
				// know about. Otherwise, it's in the future.
				if header.PrevBlock != curStamp.Hash {
					continue rescanLoop
				}
				curHeader = header
				curStamp.Hash = header.BlockHash()
				curStamp.Height++
			case header := <-blockDisconnected:
				// Only deal with it if it's the current block
				// we know about. Otherwise, it's in the future.
				if header.BlockHash() == curStamp.Hash {
					// Run through notifications. This is
					// all single-threaded. We include
					// deprecated calls as they're still
					// used, for now.
					if ro.ntfn.
						OnFilteredBlockDisconnected !=
						nil {
						ro.ntfn.OnFilteredBlockDisconnected(
							curStamp.Height,
							&curHeader)
					}
					if ro.ntfn.OnBlockDisconnected != nil {
						ro.ntfn.OnBlockDisconnected(
							&curStamp.Hash,
							curStamp.Height,
							curHeader.Timestamp)
					}
					header, _, err := s.GetBlockByHash(
						header.PrevBlock)
					if err != nil {
						return err
					}
					curHeader = header
					curStamp.Hash = header.BlockHash()
					curStamp.Height--
				}
				continue rescanLoop
			}
		} else {
			// Since we're not current, we try to manually advance
			// the block. If we fail, we mark outselves as current
			// and follow notifications.
			header, err := s.GetBlockByHeight(uint32(
				curStamp.Height + 1))
			if err != nil {
				log.Tracef("Rescan became current at %d (%s), "+
					"subscribing to block notifications",
					curStamp.Height, curStamp.Hash)
				current = true
				// Subscribe to block notifications.
				s.subscribeBlockMsg(subscription)
				continue rescanLoop
			}
			curHeader = header
			curStamp.Height++
			curStamp.Hash = header.BlockHash()
		}

		// At this point, we've found the block header that's next in
		// our rescan. First, if we're sending out BlockConnected
		// notifications, do that.
		if ro.ntfn.OnBlockConnected != nil {
			ro.ntfn.OnBlockConnected(&curStamp.Hash,
				curStamp.Height, curHeader.Timestamp)
		}
		// Now we need to see if it matches the rescan's filters, so we
		// get the basic filter from the DB or network.
		var block *btcutil.Block
		var relevantTxs []*btcutil.Tx
		var bFilter, eFilter *gcs.Filter
		var err error
		key := builder.DeriveKey(&curStamp.Hash)
		matched := false
		bFilter, err = s.GetCFilter(curStamp.Hash, false)
		if err != nil {
			return err
		}
		if bFilter != nil && bFilter.N() != 0 {
			// We see if any relevant transactions match.
			matched, err = bFilter.MatchAny(key, ro.watchList)
			if err != nil {
				return err
			}
		}
		if len(ro.watchTxIDs) > 0 {
			eFilter, err = s.GetCFilter(curStamp.Hash, true)
			if err != nil {
				return err
			}
		}
		if eFilter != nil && eFilter.N() != 0 {
			// We see if any relevant transactions match.
			matched, err = eFilter.MatchAny(key, ro.watchList)
			if err != nil {
				return err
			}
		}
		// If we have no transactions, we just send an
		// OnFilteredBlockConnected notification with  no relevant
		// transactions.
		if matched {
			// We've matched. Now we actually get the block
			// and cycle through the transactions to see
			// which ones are relevant.
			block, err = s.GetBlockFromNetwork(
				curStamp.Hash, ro.queryOptions...)
			if err != nil {
				return err
			}
			if block == nil {
				return fmt.Errorf("Couldn't get block %d "+
					"(%s) from network", curStamp.Height,
					curStamp.Hash)
			}
			relevantTxs, err = ro.notifyBlock(block)
			if err != nil {
				return err
			}
		}
		if ro.ntfn.OnFilteredBlockConnected != nil {
			ro.ntfn.OnFilteredBlockConnected(curStamp.Height,
				&curHeader, relevantTxs)
		}
		if curStamp.Hash == ro.endBlock.Hash || curStamp.Height ==
			ro.endBlock.Height {
			return nil
		}
		select {
		case update := <-ro.update:
			rewound, err := ro.updateFilter(update, &curStamp,
				&curHeader)
			if err != nil {
				return err
			}
			if rewound {
				current = false
			}
		default:
		}
	}
}

// updateFilter atomically updates the filter and rewinds to the specified
// height if not 0.
func (ro *rescanOptions) updateFilter(update *updateOptions,
	curStamp *waddrmgr.BlockStamp, curHeader *wire.BlockHeader) (bool,
	error) {
	ro.watchAddrs = append(ro.watchAddrs,
		update.addrs...)
	ro.watchOutPoints = append(ro.watchOutPoints,
		update.outPoints...)
	ro.watchTxIDs = append(ro.watchTxIDs,
		update.txIDs...)
	for _, addr := range update.addrs {
		ro.watchList = append(ro.watchList, addr.ScriptAddress())
	}
	for _, op := range update.outPoints {
		ro.watchList = append(ro.watchList,
			builder.OutPointToFilterEntry(op))
	}
	for _, txid := range update.txIDs {
		ro.watchList = append(ro.watchList, txid[:])
	}
	// Rewind if requested
	if update.rewind == 0 {
		return false, nil
	}
	var header wire.BlockHeader
	var height uint32
	var rewound bool
	var err error
	for curStamp.Height > int32(update.rewind) {
		if ro.ntfn.OnBlockDisconnected != nil {
			ro.ntfn.OnBlockDisconnected(&curStamp.Hash,
				curStamp.Height, curHeader.Timestamp)
		}
		if ro.ntfn.OnFilteredBlockDisconnected != nil {
			ro.ntfn.OnFilteredBlockDisconnected(curStamp.Height,
				curHeader)
		}
		// Don't rewind past the last block we need to disconnect,
		// because otherwise we connect the last known good block
		// without ever disconnecting it.
		if curStamp.Height == int32(update.rewind+1) {
			break
		}
		// Rewind and continue.
		header, height, err =
			ro.chain.GetBlockByHash(curHeader.PrevBlock)
		if err != nil {
			return rewound, err
		}
		*curHeader = header
		curStamp.Height = int32(height)
		curStamp.Hash = curHeader.BlockHash()
		rewound = true
	}
	return rewound, nil
}

// notifyBlock notifies listeners based on the block filter. It writes back to
// the outPoints argument the updated list of outpoints to monitor based on
// matched addresses.
func (ro *rescanOptions) notifyBlock(block *btcutil.Block) ([]*btcutil.Tx,
	error) {
	var relevantTxs []*btcutil.Tx
	blockHeader := block.MsgBlock().Header
	details := btcjson.BlockDetails{
		Height: block.Height(),
		Hash:   block.Hash().String(),
		Time:   blockHeader.Timestamp.Unix(),
	}
	for txIdx, tx := range block.Transactions() {
		relevant := false
		txDetails := details
		txDetails.Index = txIdx
		for _, hash := range ro.watchTxIDs {
			if hash == *(tx.Hash()) {
				relevant = true
				break
			}
		}
		for _, in := range tx.MsgTx().TxIn {
			if relevant {
				break
			}
			for _, op := range ro.watchOutPoints {
				if in.PreviousOutPoint == op {
					relevant = true
					if ro.ntfn.OnRedeemingTx != nil {
						ro.ntfn.OnRedeemingTx(tx,
							&txDetails)
					}
					break
				}
			}
		}
		for outIdx, out := range tx.MsgTx().TxOut {
			pushedData, err := txscript.PushedData(out.PkScript)
			if err != nil {
				continue
			}
			for _, addr := range ro.watchAddrs {
				if relevant {
					break
				}
				for _, data := range pushedData {
					if bytes.Equal(data,
						addr.ScriptAddress()) {
						relevant = true
						hash := tx.Hash()
						outPoint := wire.OutPoint{
							Hash:  *hash,
							Index: uint32(outIdx),
						}
						ro.watchOutPoints = append(
							ro.watchOutPoints,
							outPoint)
						ro.watchList = append(
							ro.watchList,
							builder.OutPointToFilterEntry(
								outPoint))
						if ro.ntfn.OnRecvTx != nil {
							ro.ntfn.OnRecvTx(tx,
								&txDetails)
						}
					}
				}
			}
		}
		if relevant {
			relevantTxs = append(relevantTxs, tx)
		}
	}
	return relevantTxs, nil
}

// Rescan is an object that represents a long-running rescan/notification
// client with updateable filters. It's meant to be close to a drop-in
// replacement for the btcd rescan and notification functionality used in
// wallets. It only contains information about whether a goroutine is running.
type Rescan struct {
	running    uint32
	updateChan chan<- *updateOptions
}

// NewRescan returns a rescan object that runs in another goroutine and has an
// updateable filter. It returns the long-running rescan object, and a channel
// which returns any error on termination of the rescan process.
func (s *ChainService) NewRescan(options ...RescanOption) (Rescan,
	<-chan error) {
	updChan := make(chan *updateOptions)
	errChan := make(chan error)
	rescan := Rescan{
		running:    1,
		updateChan: updChan,
	}
	go func() {
		err := s.Rescan(append(options, updateChan(updChan))...)
		atomic.StoreUint32(&rescan.running, 0)
		errChan <- err
	}()
	return rescan, errChan
}

// Functional parameters for Update.
type updateOptions struct {
	addrs     []btcutil.Address
	outPoints []wire.OutPoint
	txIDs     []chainhash.Hash
	rewind    uint32
}

// UpdateOption is a functional option argument for the Rescan.Update method.
type UpdateOption func(uo *updateOptions)

func defaultUpdateOptions() *updateOptions {
	return &updateOptions{}
}

// AddAddrs adds addresses to the filter.
func AddAddrs(addrs ...btcutil.Address) UpdateOption {
	return func(uo *updateOptions) {
		uo.addrs = append(uo.addrs, addrs...)
	}
}

// AddOutPoints adds outpoints to the filter.
func AddOutPoints(outPoints ...wire.OutPoint) UpdateOption {
	return func(uo *updateOptions) {
		uo.outPoints = append(uo.outPoints, outPoints...)
	}
}

// AddTxIDs adds TxIDs to the filter.
func AddTxIDs(txIDs ...chainhash.Hash) UpdateOption {
	return func(uo *updateOptions) {
		uo.txIDs = append(uo.txIDs, txIDs...)
	}
}

// Rewind rewinds the rescan to the specified height (meaning, disconnects down
// to the block immediately after the specified height) and restarts it from
// that point with the (possibly) newly expanded filter. Especially useful when
// called in the same Update() as one of the previous three options.
func Rewind(height uint32) UpdateOption {
	return func(uo *updateOptions) {
		uo.rewind = height
	}
}

// Update sends an update to a long-running rescan/notification goroutine.
func (r *Rescan) Update(options ...UpdateOption) error {
	running := atomic.LoadUint32(&r.running)
	if running != 1 {
		return fmt.Errorf("Rescan is already done and cannot be " +
			"updated.")
	}
	uo := defaultUpdateOptions()
	for _, option := range options {
		option(uo)
	}
	r.updateChan <- uo
	return nil
}

// GetUtxo gets the appropriate TxOut or errors if it's spent. The option
// WatchOutPoints (with a single outpoint) is required. StartBlock can be used
// to give a hint about which block the transaction is in, and TxIdx can be used
// to give a hint of which transaction in the block matches it (coinbase is 0,
// first normal transaction is 1, etc.).
func (s *ChainService) GetUtxo(options ...RescanOption) (*wire.TxOut,
	*wire.MsgTx, error) {
	ro := defaultRescanOptions()
	ro.startBlock = &waddrmgr.BlockStamp{
		Hash:   *s.chainParams.GenesisHash,
		Height: 0,
	}
	for _, option := range options {
		option(ro)
	}
	if len(ro.watchOutPoints) != 1 {
		return nil, nil, fmt.Errorf("Must pass exactly one OutPoint.")
	}
	watchList := [][]byte{
		builder.OutPointToFilterEntry(ro.watchOutPoints[0]),
		ro.watchOutPoints[0].Hash[:],
	}
	// Track our position in the chain.
	curHeader, curHeight, err := s.LatestBlock()
	curStamp := &waddrmgr.BlockStamp{
		Hash:   curHeader.BlockHash(),
		Height: int32(curHeight),
	}
	if err != nil {
		return nil, nil, err
	}

	// Find our earliest possible block.
	if (ro.startBlock.Hash != chainhash.Hash{}) {
		_, height, err := s.GetBlockByHash(ro.startBlock.Hash)
		if err == nil {
			ro.startBlock.Height = int32(height)
		} else {
			ro.startBlock.Hash = chainhash.Hash{}
		}
	}
	if (ro.startBlock.Hash == chainhash.Hash{}) {
		if ro.startBlock.Height == 0 {
			ro.startBlock.Hash = *s.chainParams.GenesisHash
		} else {
			header, err := s.GetBlockByHeight(
				uint32(ro.startBlock.Height))
			if err == nil {
				ro.startBlock.Hash = header.BlockHash()
			} else {
				ro.startBlock.Hash = *s.chainParams.GenesisHash
				ro.startBlock.Height = 0
			}
		}
	}
	log.Tracef("Starting scan for output spend from known block %d (%s) "+
		"back to block %d (%s)", curStamp.Height, curStamp.Hash,
		ro.startBlock.Height, ro.startBlock.Hash)

	for {
		// Check the basic filter for the spend and the extended filter
		// for the transaction in which the outpout is funded.
		filter, err := s.GetCFilter(curStamp.Hash, false,
			ro.queryOptions...)
		if err != nil {
			return nil, nil, fmt.Errorf("Couldn't get basic "+
				"filter for block %d (%s)", curStamp.Height,
				curStamp.Hash)
		}
		matched := false
		if filter != nil {
			matched, err = filter.MatchAny(builder.DeriveKey(
				&curStamp.Hash), watchList)
		}
		if err != nil {
			return nil, nil, err
		}
		if !matched {
			filter, err = s.GetCFilter(curStamp.Hash, true,
				ro.queryOptions...)
			if err != nil {
				return nil, nil, fmt.Errorf("Couldn't get "+
					"extended filter for block %d (%s)",
					curStamp.Height, curStamp.Hash)
			}
			if filter != nil {
				matched, err = filter.MatchAny(
					builder.DeriveKey(&curStamp.Hash),
					watchList)
			}
		}
		// If either is matched, download the block and check to see
		// what we have.
		if matched {
			block, err := s.GetBlockFromNetwork(curStamp.Hash,
				ro.queryOptions...)
			if err != nil {
				return nil, nil, err
			}
			if block == nil {
				return nil, nil, fmt.Errorf("Couldn't get "+
					"block %d (%s)", curStamp.Height,
					curStamp.Hash)
			}
			// If we've spent the output in this block, return an
			// error stating that the output is spent.
			for _, tx := range block.Transactions() {
				for _, ti := range tx.MsgTx().TxIn {
					if ti.PreviousOutPoint ==
						ro.watchOutPoints[0] {
						return nil, tx.MsgTx(), nil
					}
				}
			}
			// If we found the transaction that created the output,
			// then it's not spent and we can return the TxOut.
			for _, tx := range block.Transactions() {
				if *(tx.Hash()) ==
					ro.watchOutPoints[0].Hash {
					return tx.MsgTx().
						TxOut[ro.watchOutPoints[0].
						Index], nil, nil
				}
			}
		}
		// Otherwise, iterate backwards until we've gone too
		// far.
		curStamp.Height--
		if curStamp.Height < ro.startBlock.Height {
			return nil, nil, fmt.Errorf("Couldn't find "+
				"transaction %s",
				ro.watchOutPoints[0].Hash)
		}
		header, err := s.GetBlockByHeight(
			uint32(curStamp.Height))
		if err != nil {
			return nil, nil, err
		}
		curStamp.Hash = header.BlockHash()
	}
}