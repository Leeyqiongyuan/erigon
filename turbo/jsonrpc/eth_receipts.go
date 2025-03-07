package jsonrpc

import (
	"context"
	"fmt"

	"github.com/RoaringBitmap/roaring"

	"github.com/ledgerwatch/erigon-lib/log/v3"

	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/bitmapdb"
	"github.com/ledgerwatch/erigon-lib/kv/iter"
	"github.com/ledgerwatch/erigon-lib/kv/order"
	"github.com/ledgerwatch/erigon-lib/kv/rawdbv3"

	"github.com/ledgerwatch/erigon/cmd/state/exec3"
	"github.com/ledgerwatch/erigon/core"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/core/state"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/core/vm"
	"github.com/ledgerwatch/erigon/eth/ethutils"
	"github.com/ledgerwatch/erigon/eth/filters"
	bortypes "github.com/ledgerwatch/erigon/polygon/bor/types"
	"github.com/ledgerwatch/erigon/rpc"
	"github.com/ledgerwatch/erigon/turbo/rpchelper"
	"github.com/ledgerwatch/erigon/turbo/transactions"
)

// getReceipts - checking in-mem cache, or else fallback to db, or else fallback to re-exec of block to re-gen receipts
func (api *BaseAPI) getReceipts(ctx context.Context, tx kv.Tx, block *types.Block, senders []common.Address) (types.Receipts, error) {
	if receipts, ok := api.receiptsCache.Get(block.Hash()); ok {
		return receipts, nil
	}

	if receipts := rawdb.ReadReceipts(tx, block, senders); receipts != nil {
		api.receiptsCache.Add(block.Hash(), receipts)
		return receipts, nil
	}

	engine := api.engine()
	chainConfig, err := api.chainConfig(ctx, tx)
	if err != nil {
		return nil, err
	}

	_, _, _, ibs, _, err := transactions.ComputeTxEnv(ctx, engine, block, chainConfig, api._blockReader, tx, 0)
	if err != nil {
		return nil, err
	}

	usedGas := new(uint64)
	usedBlobGas := new(uint64)
	gp := new(core.GasPool).AddGas(block.GasLimit()).AddBlobGas(chainConfig.GetMaxBlobGasPerBlock())

	noopWriter := state.NewNoopWriter()

	receipts := make(types.Receipts, len(block.Transactions()))

	getHeader := func(hash common.Hash, number uint64) *types.Header {
		h, e := api._blockReader.Header(ctx, tx, hash, number)
		if e != nil {
			log.Error("getHeader error", "number", number, "hash", hash, "err", e)
		}
		return h
	}
	header := block.Header()
	for i, txn := range block.Transactions() {
		ibs.SetTxContext(txn.Hash(), block.Hash(), i)
		receipt, _, err := core.ApplyTransaction(chainConfig, core.GetHashFn(header, getHeader), engine, nil, gp, ibs, noopWriter, header, txn, usedGas, usedBlobGas, vm.Config{})
		if err != nil {
			return nil, err
		}
		receipt.BlockHash = block.Hash()
		receipts[i] = receipt
	}

	api.receiptsCache.Add(block.Hash(), receipts)
	return receipts, nil
}

// GetLogs implements eth_getLogs. Returns an array of logs matching a given filter object.
func (api *APIImpl) GetLogs(ctx context.Context, crit filters.FilterCriteria) (types.Logs, error) {
	var begin, end uint64
	logs := types.Logs{}

	tx, beginErr := api.db.BeginRo(ctx)
	if beginErr != nil {
		return logs, beginErr
	}
	defer tx.Rollback()

	if crit.BlockHash != nil {
		block, err := api.blockByHashWithSenders(ctx, tx, *crit.BlockHash)
		if err != nil {
			return nil, err
		}
		if block == nil {
			return nil, fmt.Errorf("block not found: %x", *crit.BlockHash)
		}

		num := block.NumberU64()
		begin = num
		end = num
	} else {
		// Convert the RPC block numbers into internal representations
		latest, _, _, err := rpchelper.GetBlockNumber(rpc.BlockNumberOrHashWithNumber(rpc.LatestExecutedBlockNumber), tx, nil)
		if err != nil {
			return nil, err
		}

		begin = latest
		if crit.FromBlock != nil {
			fromBlock := crit.FromBlock.Int64()
			if fromBlock > 0 {
				begin = uint64(fromBlock)
			} else {
				blockNum := rpc.BlockNumber(fromBlock)
				begin, _, _, err = rpchelper.GetBlockNumber(rpc.BlockNumberOrHashWithNumber(blockNum), tx, api.filters)
				if err != nil {
					return nil, err
				}
			}

		}
		end = latest
		if crit.ToBlock != nil {
			toBlock := crit.ToBlock.Int64()
			if toBlock > 0 {
				end = uint64(toBlock)
			} else {
				blockNum := rpc.BlockNumber(toBlock)
				end, _, _, err = rpchelper.GetBlockNumber(rpc.BlockNumberOrHashWithNumber(blockNum), tx, api.filters)
				if err != nil {
					return nil, err
				}
			}
		}
	}

	if end < begin {
		return nil, fmt.Errorf("end (%d) < begin (%d)", end, begin)
	}
	if end > roaring.MaxUint32 {
		latest, err := rpchelper.GetLatestBlockNumber(tx)
		if err != nil {
			return nil, err
		}
		if begin > latest {
			return nil, fmt.Errorf("begin (%d) > latest (%d)", begin, latest)
		}
		end = latest
	}

	return api.getLogsV3(ctx, tx.(kv.TemporalTx), begin, end, crit)
}

// The Topic list restricts matches to particular event topics. Each event has a list
// of topics. Topics matches a prefix of that list. An empty element slice matches any
// topic. Non-empty elements represent an alternative that matches any of the
// contained topics.
//
// Examples:
// {} or nil          matches any topic list
// {{A}}              matches topic A in first position
// {{}, {B}}          matches any topic in first position AND B in second position
// {{A}, {B}}         matches topic A in first position AND B in second position
// {{A, B}, {C, D}}   matches topic (A OR B) in first position AND (C OR D) in second position
func getTopicsBitmap(c kv.Tx, topics [][]common.Hash, from, to uint64) (*roaring.Bitmap, error) {
	var result *roaring.Bitmap
	for _, sub := range topics {
		var bitmapForORing *roaring.Bitmap
		for _, topic := range sub {
			m, err := bitmapdb.Get(c, kv.LogTopicIndex, topic[:], uint32(from), uint32(to))
			if err != nil {
				return nil, err
			}
			if bitmapForORing == nil {
				bitmapForORing = m
				continue
			}
			bitmapForORing.Or(m)
		}

		if bitmapForORing == nil {
			continue
		}
		if result == nil {
			result = bitmapForORing
			continue
		}

		result = roaring.And(bitmapForORing, result)
	}
	return result, nil
}
func getAddrsBitmap(tx kv.Tx, addrs []common.Address, from, to uint64) (*roaring.Bitmap, error) {
	if len(addrs) == 0 {
		return nil, nil
	}
	rx := make([]*roaring.Bitmap, len(addrs))
	defer func() {
		for _, bm := range rx {
			bitmapdb.ReturnToPool(bm)
		}
	}()
	for idx, addr := range addrs {
		m, err := bitmapdb.Get(tx, kv.LogAddressIndex, addr[:], uint32(from), uint32(to))
		if err != nil {
			return nil, err
		}
		rx[idx] = m
	}
	return roaring.FastOr(rx...), nil
}

func applyFilters(out *roaring.Bitmap, tx kv.Tx, begin, end uint64, crit filters.FilterCriteria) error {
	out.AddRange(begin, end+1) // [from,to)
	topicsBitmap, err := getTopicsBitmap(tx, crit.Topics, begin, end)
	if err != nil {
		return err
	}
	if topicsBitmap != nil {
		out.And(topicsBitmap)
	}
	addrBitmap, err := getAddrsBitmap(tx, crit.Addresses, begin, end)
	if err != nil {
		return err
	}
	if addrBitmap != nil {
		out.And(addrBitmap)
	}
	return nil
}

func applyFiltersV3(tx kv.TemporalTx, begin, end uint64, crit filters.FilterCriteria) (out iter.U64, err error) {
	//[from,to)
	var fromTxNum, toTxNum uint64
	if begin > 0 {
		fromTxNum, err = rawdbv3.TxNums.Min(tx, begin)
		if err != nil {
			return out, err
		}
	}
	toTxNum, err = rawdbv3.TxNums.Max(tx, end)
	if err != nil {
		return out, err
	}
	toTxNum++

	topicsBitmap, err := getTopicsBitmapV3(tx, crit.Topics, fromTxNum, toTxNum)
	if err != nil {
		return out, err
	}
	if topicsBitmap != nil {
		out = topicsBitmap
	}
	addrBitmap, err := getAddrsBitmapV3(tx, crit.Addresses, fromTxNum, toTxNum)
	if err != nil {
		return out, err
	}
	if addrBitmap != nil {
		if out == nil {
			out = addrBitmap
		} else {
			out = iter.Intersect[uint64](out, addrBitmap, -1)
		}
	}
	if out == nil {
		out = iter.Range[uint64](fromTxNum, toTxNum)
	}
	return out, nil
}

func (api *APIImpl) getLogsV3(ctx context.Context, tx kv.TemporalTx, begin, end uint64, crit filters.FilterCriteria) ([]*types.Log, error) {
	logs := []*types.Log{}

	addrMap := make(map[common.Address]struct{}, len(crit.Addresses))
	for _, v := range crit.Addresses {
		addrMap[v] = struct{}{}
	}

	chainConfig, err := api.chainConfig(ctx, tx)
	if err != nil {
		return nil, err
	}
	exec := exec3.NewTraceWorker(tx, chainConfig, api.engine(), api._blockReader, nil)

	var blockHash common.Hash
	var header *types.Header

	txNumbers, err := applyFiltersV3(tx, begin, end, crit)
	if err != nil {
		return logs, err
	}
	iter := rawdbv3.TxNums2BlockNums(tx, txNumbers, order.Asc)
	defer iter.Close()
	for iter.HasNext() {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		txNum, blockNum, txIndex, isFinalTxn, blockNumChanged, err := iter.Next()
		if err != nil {
			return nil, err
		}
		if isFinalTxn {
			continue
		}

		// if block number changed, calculate all related field
		if blockNumChanged {
			if header, err = api._blockReader.HeaderByNumber(ctx, tx, blockNum); err != nil {
				return nil, err
			}
			if header == nil {
				log.Warn("[rpc] header is nil", "blockNum", blockNum)
				continue
			}
			blockHash = header.Hash()
			exec.ChangeBlock(header)
		}

		//fmt.Printf("txNum=%d, blockNum=%d, txIndex=%d, maxTxNumInBlock=%d,mixTxNumInBlock=%d\n", txNum, blockNum, txIndex, maxTxNumInBlock, minTxNumInBlock)
		txn, err := api._txnReader.TxnByIdxInBlock(ctx, tx, blockNum, txIndex)
		if err != nil {
			return nil, err
		}
		if txn == nil {
			continue
		}

		_, err = exec.ExecTxn(txNum, txIndex, txn)
		if err != nil {
			return nil, err
		}
		rawLogs := exec.GetLogs(txIndex, txn)
		//TODO: logIndex within the block! no way to calc it now
		//logIndex := uint(0)
		//for _, log := range rawLogs {
		//	log.Index = logIndex
		//	logIndex++
		//}
		filtered := rawLogs.Filter(addrMap, crit.Topics)
		for _, log := range filtered {
			log.BlockNumber = blockNum
			log.BlockHash = blockHash
			log.TxHash = txn.Hash()
		}
		logs = append(logs, filtered...)
	}

	//stats := api._agg.GetAndResetStats()
	//log.Info("Finished", "duration", time.Since(start), "history queries", stats.FilesQueries, "ef search duration", stats.EfSearchTime)
	return logs, nil
}

// The Topic list restricts matches to particular event topics. Each event has a list
// of topics. Topics matches a prefix of that list. An empty element slice matches any
// topic. Non-empty elements represent an alternative that matches any of the
// contained topics.
//
// Examples:
// {} or nil          matches any topic list
// {{A}}              matches topic A in first position
// {{}, {B}}          matches any topic in first position AND B in second position
// {{A}, {B}}         matches topic A in first position AND B in second position
// {{A, B}, {C, D}}   matches topic (A OR B) in first position AND (C OR D) in second position
func getTopicsBitmapV3(tx kv.TemporalTx, topics [][]common.Hash, from, to uint64) (res iter.U64, err error) {
	for _, sub := range topics {

		var topicsUnion iter.U64
		for _, topic := range sub {
			it, err := tx.IndexRange(kv.LogTopicIdx, topic.Bytes(), int(from), int(to), order.Asc, kv.Unlim)
			if err != nil {
				return nil, err
			}
			topicsUnion = iter.Union[uint64](topicsUnion, it, order.Asc, -1)
		}

		if res == nil {
			res = topicsUnion
			continue
		}
		res = iter.Intersect[uint64](res, topicsUnion, -1)
	}
	return res, nil
}

func getAddrsBitmapV3(tx kv.TemporalTx, addrs []common.Address, from, to uint64) (res iter.U64, err error) {
	for _, addr := range addrs {
		it, err := tx.IndexRange(kv.LogAddrIdx, addr[:], int(from), int(to), true, kv.Unlim)
		if err != nil {
			return nil, err
		}
		res = iter.Union[uint64](res, it, order.Asc, -1)
	}
	return res, nil
}

// GetTransactionReceipt implements eth_getTransactionReceipt. Returns the receipt of a transaction given the transaction's hash.
func (api *APIImpl) GetTransactionReceipt(ctx context.Context, txnHash common.Hash) (map[string]interface{}, error) {
	tx, err := api.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var blockNum uint64
	var ok bool

	blockNum, ok, err = api.txnLookup(ctx, tx, txnHash)
	if err != nil {
		return nil, err
	}

	cc, err := api.chainConfig(ctx, tx)
	if err != nil {
		return nil, err
	}
	// Private API returns 0 if transaction is not found.
	if blockNum == 0 && cc.Bor != nil {
		blockNum, ok, err = api._blockReader.EventLookup(ctx, tx, txnHash)
		if err != nil {
			return nil, err
		}
	}

	if !ok {
		return nil, nil
	}

	block, err := api.blockByNumberWithSenders(ctx, tx, blockNum)
	if err != nil {
		return nil, err
	}
	if block == nil {
		return nil, nil // not error, see https://github.com/ledgerwatch/erigon/issues/1645
	}

	var txnIndex uint64
	var txn types.Transaction
	for idx, transaction := range block.Transactions() {
		if transaction.Hash() == txnHash {
			txn = transaction
			txnIndex = uint64(idx)
			break
		}
	}

	var borTx types.Transaction
	if txn == nil && cc.Bor != nil {
		borTx = rawdb.ReadBorTransactionForBlock(tx, blockNum)
		if borTx == nil {
			borTx = bortypes.NewBorTransaction()
		}
	}
	receipts, err := api.getReceipts(ctx, tx, block, block.Body().SendersFromTxs())
	if err != nil {
		return nil, fmt.Errorf("getReceipts error: %w", err)
	}

	if txn == nil && cc.Bor != nil {
		borReceipt, err := rawdb.ReadBorReceipt(tx, block.Hash(), blockNum, receipts)
		if err != nil {
			return nil, err
		}
		if borReceipt == nil {
			return nil, nil
		}
		return ethutils.MarshalReceipt(borReceipt, borTx, cc, block.HeaderNoCopy(), txnHash, false), nil
	}

	if len(receipts) <= int(txnIndex) {
		return nil, fmt.Errorf("block has less receipts than expected: %d <= %d, block: %d", len(receipts), int(txnIndex), blockNum)
	}

	return ethutils.MarshalReceipt(receipts[txnIndex], block.Transactions()[txnIndex], cc, block.HeaderNoCopy(), txnHash, true), nil
}

// GetBlockReceipts - receipts for individual block
func (api *APIImpl) GetBlockReceipts(ctx context.Context, numberOrHash rpc.BlockNumberOrHash) ([]map[string]interface{}, error) {
	tx, err := api.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	blockNum, blockHash, _, err := rpchelper.GetBlockNumber(numberOrHash, tx, api.filters)
	if err != nil {
		return nil, err
	}
	block, err := api.blockWithSenders(ctx, tx, blockHash, blockNum)
	if err != nil {
		return nil, err
	}
	if block == nil {
		return nil, nil
	}
	chainConfig, err := api.chainConfig(ctx, tx)
	if err != nil {
		return nil, err
	}
	receipts, err := api.getReceipts(ctx, tx, block, block.Body().SendersFromTxs())
	if err != nil {
		return nil, fmt.Errorf("getReceipts error: %w", err)
	}
	result := make([]map[string]interface{}, 0, len(receipts))
	for _, receipt := range receipts {
		txn := block.Transactions()[receipt.TransactionIndex]
		result = append(result, ethutils.MarshalReceipt(receipt, txn, chainConfig, block.HeaderNoCopy(), txn.Hash(), true))
	}

	if chainConfig.Bor != nil {
		borTx := rawdb.ReadBorTransactionForBlock(tx, blockNum)
		if borTx != nil {
			borReceipt, err := rawdb.ReadBorReceipt(tx, block.Hash(), blockNum, receipts)
			if err != nil {
				return nil, err
			}
			if borReceipt != nil {
				result = append(result, ethutils.MarshalReceipt(borReceipt, borTx, chainConfig, block.HeaderNoCopy(), borReceipt.TxHash, false))
			}
		}
	}

	return result, nil
}

// MapTxNum2BlockNumIter - enrich iterator by TxNumbers, adding more info:
//   - blockNum
//   - txIndex in block: -1 means first system tx
//   - isFinalTxn: last system-txn. BlockRewards and similar things - are attribute to this virtual txn.
//   - blockNumChanged: means this and previous txNum belongs to different blockNumbers
//
// Expect: `it` to return sorted txNums, then blockNum will not change until `it.Next() < maxTxNumInBlock`
//
//	it allow certain optimizations.
type MapTxNum2BlockNumIter struct {
	it          iter.U64
	tx          kv.Tx
	orderAscend bool

	blockNum                         uint64
	minTxNumInBlock, maxTxNumInBlock uint64
}

func MapTxNum2BlockNum(tx kv.Tx, it iter.U64) *MapTxNum2BlockNumIter {
	return &MapTxNum2BlockNumIter{tx: tx, it: it, orderAscend: true}
}
func MapDescendTxNum2BlockNum(tx kv.Tx, it iter.U64) *MapTxNum2BlockNumIter {
	return &MapTxNum2BlockNumIter{tx: tx, it: it, orderAscend: false}
}
func (i *MapTxNum2BlockNumIter) HasNext() bool { return i.it.HasNext() }
func (i *MapTxNum2BlockNumIter) Next() (txNum, blockNum uint64, txIndex int, isFinalTxn, blockNumChanged bool, err error) {
	txNum, err = i.it.Next()
	if err != nil {
		return txNum, blockNum, txIndex, isFinalTxn, blockNumChanged, err
	}

	// txNums are sorted, it means blockNum will not change until `txNum < maxTxNumInBlock`
	if i.maxTxNumInBlock == 0 || (i.orderAscend && txNum > i.maxTxNumInBlock) || (!i.orderAscend && txNum < i.minTxNumInBlock) {
		blockNumChanged = true

		var ok bool
		ok, i.blockNum, err = rawdbv3.TxNums.FindBlockNum(i.tx, txNum)
		if err != nil {
			return
		}
		if !ok {
			return txNum, i.blockNum, txIndex, isFinalTxn, blockNumChanged, fmt.Errorf("can't find blockNumber by txnID=%d", txNum)
		}
	}
	blockNum = i.blockNum

	// if block number changed, calculate all related field
	if blockNumChanged {
		i.minTxNumInBlock, err = rawdbv3.TxNums.Min(i.tx, blockNum)
		if err != nil {
			return
		}
		i.maxTxNumInBlock, err = rawdbv3.TxNums.Max(i.tx, blockNum)
		if err != nil {
			return
		}
	}

	txIndex = int(txNum) - int(i.minTxNumInBlock) - 1
	isFinalTxn = txNum == i.maxTxNumInBlock
	return
}
