package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"net/http"
	_ "net/http/pprof" //nolint:gosec
	"os"
	"path/filepath"
	"runtime/pprof"
	"slices"
	"sort"
	"strings"

	"github.com/ledgerwatch/erigon-lib/kv/dbutils"

	"github.com/RoaringBitmap/roaring/roaring64"
	"github.com/holiman/uint256"

	"github.com/ledgerwatch/erigon-lib/log/v3"

	libcommon "github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/hexutility"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/mdbx"
	"github.com/ledgerwatch/erigon-lib/recsplit"
	"github.com/ledgerwatch/erigon-lib/recsplit/eliasfano32"
	"github.com/ledgerwatch/erigon-lib/seg"

	hackdb "github.com/ledgerwatch/erigon/cmd/hack/db"
	"github.com/ledgerwatch/erigon/cmd/hack/flow"
	"github.com/ledgerwatch/erigon/cmd/hack/tool"
	"github.com/ledgerwatch/erigon/common"
	"github.com/ledgerwatch/erigon/common/paths"
	"github.com/ledgerwatch/erigon/core"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/core/rawdb/blockio"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/eth/ethconfig"
	"github.com/ledgerwatch/erigon/eth/stagedsync/stages"
	"github.com/ledgerwatch/erigon/ethdb/cbor"
	"github.com/ledgerwatch/erigon/params"
	"github.com/ledgerwatch/erigon/rlp"
	"github.com/ledgerwatch/erigon/turbo/debug"
	"github.com/ledgerwatch/erigon/turbo/logging"
	"github.com/ledgerwatch/erigon/turbo/services"
	"github.com/ledgerwatch/erigon/turbo/snapshotsync/freezeblocks"
)

var (
	action     = flag.String("action", "", "action to execute")
	cpuprofile = flag.String("cpuprofile", "", "write cpu profile `file`")
	block      = flag.Int("block", 1, "specifies a block number for operation")
	blockTotal = flag.Int("blocktotal", 1, "specifies a total amount of blocks to process (will offset from head block if <= 0)")
	account    = flag.String("account", "0x", "specifies account to investigate")
	name       = flag.String("name", "", "name to add to the file names")
	chaindata  = flag.String("chaindata", "chaindata", "path to the chaindata database file")
	bucket     = flag.String("bucket", "", "bucket in the database")
	hash       = flag.String("hash", "0x00", "image for preimage or state root for testBlockHashes action")
)

func dbSlice(chaindata string, bucket string, prefix []byte) {
	db := mdbx.MustOpen(chaindata)
	defer db.Close()
	if err := db.View(context.Background(), func(tx kv.Tx) error {
		c, err := tx.Cursor(bucket)
		if err != nil {
			return err
		}
		for k, v, err := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v, err = c.Next() {
			if err != nil {
				return err
			}
			fmt.Printf("db.Put([]byte(\"%s\"), common.FromHex(\"%x\"), common.FromHex(\"%x\"))\n", bucket, k, v)
		}
		return nil
	}); err != nil {
		panic(err)
	}
}

// Searches 1000 blocks from the given one to try to find the one with the given state root hash
func testBlockHashes(chaindata string, block int, stateRoot libcommon.Hash) {
	ethDb := mdbx.MustOpen(chaindata)
	defer ethDb.Close()
	br, _ := blocksIO(ethDb)
	tool.Check(ethDb.View(context.Background(), func(tx kv.Tx) error {
		blocksToSearch := 10000000
		for i := uint64(block); i < uint64(block+blocksToSearch); i++ {
			header, err := br.HeaderByNumber(context.Background(), tx, i)
			if err != nil {
				panic(err)
			}
			if header.Root == stateRoot || stateRoot == (libcommon.Hash{}) {
				fmt.Printf("\n===============\nCanonical hash for %d: %x\n", i, hash)
				fmt.Printf("Header.Root: %x\n", header.Root)
				fmt.Printf("Header.TxHash: %x\n", header.TxHash)
				fmt.Printf("Header.UncleHash: %x\n", header.UncleHash)
			}
		}
		return nil
	}))
}

func getCurrentBlockNumber(tx kv.Tx) *uint64 {
	return rawdb.ReadCurrentBlockNumber(tx)
}

func printCurrentBlockNumber(chaindata string) {
	ethDb := mdbx.MustOpen(chaindata)
	defer ethDb.Close()
	ethDb.View(context.Background(), func(tx kv.Tx) error {
		if number := getCurrentBlockNumber(tx); number != nil {
			fmt.Printf("Block number: %d\n", *number)
		} else {
			fmt.Println("Block number: <nil>")
		}
		return nil
	})
}

func blocksIO(db kv.RoDB) (services.FullBlockReader, *blockio.BlockWriter) {
	br := freezeblocks.NewBlockReader(freezeblocks.NewRoSnapshots(ethconfig.BlocksFreezing{Enabled: false}, "", 0, log.New()), nil /* BorSnapshots */)
	bw := blockio.NewBlockWriter()
	return br, bw
}

func printTxHashes(chaindata string, block uint64) error {
	db := mdbx.MustOpen(chaindata)
	defer db.Close()
	br, _ := blocksIO(db)
	if err := db.View(context.Background(), func(tx kv.Tx) error {
		for b := block; b < block+1; b++ {
			block, _ := br.BlockByNumber(context.Background(), tx, b)
			if block == nil {
				break
			}
			for i, txn := range block.Transactions() {
				fmt.Printf("%d: %x\n", i, txn.Hash())
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func repairCurrent() {
	historyDb := mdbx.MustOpen("/Volumes/tb4/erigon/ropsten/geth/chaindata")
	defer historyDb.Close()
	currentDb := mdbx.MustOpen("statedb")
	defer currentDb.Close()
	tool.Check(historyDb.Update(context.Background(), func(tx kv.RwTx) error {
		return tx.ClearBucket(kv.HashedStorage)
	}))
	tool.Check(historyDb.Update(context.Background(), func(tx kv.RwTx) error {
		newB, err := tx.RwCursor(kv.HashedStorage)
		if err != nil {
			return err
		}
		count := 0
		if err := currentDb.View(context.Background(), func(ctx kv.Tx) error {
			c, err := ctx.Cursor(kv.HashedStorage)
			if err != nil {
				return err
			}
			for k, v, err := c.First(); k != nil; k, v, err = c.Next() {
				if err != nil {
					return err
				}
				tool.Check(newB.Put(k, v))
				count++
				if count == 10000 {
					fmt.Printf("Copied %d storage items\n", count)
				}
			}
			return nil
		}); err != nil {
			return err
		}
		return nil
	}))
}

func dumpStorage() {
	db := mdbx.MustOpen(paths.DefaultDataDir() + "/geth/chaindata")
	defer db.Close()
	if err := db.View(context.Background(), func(tx kv.Tx) error {
		return tx.ForEach(kv.E2StorageHistory, nil, func(k, v []byte) error {
			fmt.Printf("%x %x\n", k, v)
			return nil
		})
	}); err != nil {
		panic(err)
	}
}

func printBucket(chaindata string) {
	db := mdbx.MustOpen(chaindata)
	defer db.Close()
	f, err := os.Create("bucket.txt")
	tool.Check(err)
	defer f.Close()
	fb := bufio.NewWriter(f)
	defer fb.Flush()
	if err := db.View(context.Background(), func(tx kv.Tx) error {
		c, err := tx.Cursor(kv.E2StorageHistory)
		if err != nil {
			return err
		}
		for k, v, err := c.First(); k != nil; k, v, err = c.Next() {
			if err != nil {
				return err
			}
			fmt.Fprintf(fb, "%x %x\n", k, v)
		}
		return nil
	}); err != nil {
		panic(err)
	}
}

func getBlockTotal(tx kv.Tx, blockFrom uint64, blockTotalOrOffset int64) uint64 {
	if blockTotalOrOffset > 0 {
		return uint64(blockTotalOrOffset)
	}
	if head := getCurrentBlockNumber(tx); head != nil {
		if blockSub := uint64(-blockTotalOrOffset); blockSub <= *head {
			if blockEnd := *head - blockSub; blockEnd > blockFrom {
				return blockEnd - blockFrom + 1
			}
		}
	}
	return 1
}

func extractHashes(chaindata string, blockStep uint64, blockTotalOrOffset int64, name string) error {
	db := mdbx.MustOpen(chaindata)
	defer db.Close()
	br, _ := blocksIO(db)

	f, err := os.Create(fmt.Sprintf("preverified_hashes_%s.go", name))
	if err != nil {
		return err
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	defer w.Flush()

	fmt.Fprintf(w, "package headerdownload\n\n")
	fmt.Fprintf(w, "var %sPreverifiedHashes = []string{\n", name)

	b := uint64(0)
	tool.Check(db.View(context.Background(), func(tx kv.Tx) error {
		blockTotal := getBlockTotal(tx, b, blockTotalOrOffset)
		// Note: blockTotal used here as block number rather than block count
		for b <= blockTotal {
			hash, err := br.CanonicalHash(context.Background(), tx, b)
			if err != nil {
				return err
			}

			if hash == (libcommon.Hash{}) {
				break
			}

			fmt.Fprintf(w, "	\"%x\",\n", hash)
			b += blockStep
		}
		return nil
	}))

	b -= blockStep
	fmt.Fprintf(w, "}\n\n")
	fmt.Fprintf(w, "const %sPreverifiedHeight uint64 = %d\n", name, b)
	fmt.Printf("Last block is %d\n", b)
	return nil
}

func extractHeaders(chaindata string, block uint64, blockTotalOrOffset int64) error {
	db := mdbx.MustOpen(chaindata)
	defer db.Close()
	tx, err := db.BeginRo(context.Background())
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c, err := tx.Cursor(kv.Headers)
	if err != nil {
		return err
	}
	defer c.Close()
	blockEncoded := hexutility.EncodeTs(block)
	blockTotal := getBlockTotal(tx, block, blockTotalOrOffset)
	for k, v, err := c.Seek(blockEncoded); k != nil && blockTotal > 0; k, v, err = c.Next() {
		if err != nil {
			return err
		}
		blockNumber := binary.BigEndian.Uint64(k[:8])
		blockHash := libcommon.BytesToHash(k[8:])
		var header types.Header
		if err = rlp.DecodeBytes(v, &header); err != nil {
			return fmt.Errorf("decoding header from %x: %w", v, err)
		}
		fmt.Printf("Header %d %x: stateRoot %x, parentHash %x, diff %d\n", blockNumber, blockHash, header.Root, header.ParentHash, header.Difficulty)
		blockTotal--
	}
	return nil
}

func extractBodies(datadir string) error {
	snaps := freezeblocks.NewRoSnapshots(ethconfig.BlocksFreezing{
		Enabled:    true,
		KeepBlocks: true,
		ProduceE2:  false,
	}, filepath.Join(datadir, "snapshots"), 0, log.New())
	snaps.ReopenFolder()

	/* method Iterate was removed, need re-implement
	snaps.Bodies.View(func(sns []*snapshotsync.BodySegment) error {
		for _, sn := range sns {
			var firstBlockNum, firstBaseTxNum, firstAmount uint64
			var lastBlockNum, lastBaseTxNum, lastAmount uint64
			var prevBlockNum, prevBaseTxNum, prevAmount uint64
			first := true
			sn.Iterate(func(blockNum uint64, baseTxNum uint64, txCount uint64) error {
				if first {
					firstBlockNum = blockNum
					firstBaseTxNum = baseTxNum
					firstAmount = txCount
					first = false
				} else {
					if blockNum != prevBlockNum+1 {
						fmt.Printf("Discount block Num: %d => %d\n", prevBlockNum, blockNum)
					}
					if baseTxNum != prevBaseTxNum+prevAmount {
						fmt.Printf("Wrong baseTxNum: %d+%d => %d\n", prevBaseTxNum, prevAmount, baseTxNum)
					}
				}
				prevBlockNum = blockNum
				lastBlockNum = blockNum
				prevBaseTxNum = baseTxNum
				lastBaseTxNum = baseTxNum
				prevAmount = txCount
				lastAmount = txCount
				return nil
			})
			fmt.Printf("Seg: [%d, %d, %d] => [%d, %d, %d]\n", firstBlockNum, firstBaseTxNum, firstAmount, lastBlockNum, lastBaseTxNum, lastAmount)
		}
		return nil
	})
	*/
	db := mdbx.MustOpen(filepath.Join(datadir, "chaindata"))
	defer db.Close()
	br, _ := blocksIO(db)

	tx, err := db.BeginRo(context.Background())
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c, err := tx.Cursor(kv.BlockBody)
	if err != nil {
		return err
	}
	defer c.Close()
	i := 0
	var txnID uint64
	for k, _, err := c.First(); k != nil; k, _, err = c.Next() {
		if err != nil {
			return err
		}
		blockNumber := binary.BigEndian.Uint64(k[:8])
		blockHash := libcommon.BytesToHash(k[8:])
		var hash libcommon.Hash
		if hash, err = br.CanonicalHash(context.Background(), tx, blockNumber); err != nil {
			return err
		}
		_, baseTxnID, txCount := rawdb.ReadBody(tx, blockHash, blockNumber)
		fmt.Printf("Body %d %x: baseTxnID %d, txCount %d\n", blockNumber, blockHash, baseTxnID, txCount)
		if hash != blockHash {
			fmt.Printf("Non-canonical\n")
			continue
		}
		i++
		if txnID > 0 {
			if txnID != baseTxnID {
				fmt.Printf("Mismatch txnID for block %d, txnID = %d, baseTxnID = %d\n", blockNumber, txnID, baseTxnID)
			}
		}
		txnID = baseTxnID + uint64(txCount) + 2
		if i == 50 {
			break
		}
	}
	return nil
}

func snapSizes(chaindata string) error {
	db := mdbx.MustOpen(chaindata)
	defer db.Close()

	tx, err := db.BeginRo(context.Background())
	if err != nil {
		return err
	}
	defer tx.Rollback()

	c, _ := tx.Cursor(kv.CliqueSeparate)
	defer c.Close()

	sizes := make(map[int]int)
	differentValues := make(map[string]struct{})

	var (
		total uint64
		k, v  []byte
	)

	for k, v, err = c.First(); k != nil; k, v, err = c.Next() {
		if err != nil {
			return err
		}
		sizes[len(v)]++
		differentValues[string(v)] = struct{}{}
		total += uint64(len(v) + len(k))
	}

	var lens = make([]int, len(sizes))

	i := 0
	for l := range sizes {
		lens[i] = l
		i++
	}
	slices.Sort(lens)

	for _, l := range lens {
		fmt.Printf("%6d - %d\n", l, sizes[l])
	}

	fmt.Printf("Different keys %d\n", len(differentValues))
	fmt.Printf("Total size: %d bytes\n", total)

	return nil
}

func fixTd(chaindata string) error {
	db := mdbx.MustOpen(chaindata)
	defer db.Close()
	tx, err := db.BeginRw(context.Background())
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c, err1 := tx.RwCursor(kv.Headers)
	if err1 != nil {
		return err1
	}
	defer c.Close()
	var k, v []byte
	for k, v, err = c.First(); err == nil && k != nil; k, v, err = c.Next() {
		hv, herr := tx.GetOne(kv.HeaderTD, k)
		if herr != nil {
			return herr
		}
		if hv == nil {
			fmt.Printf("Missing TD record for %x, fixing\n", k)
			var header types.Header
			if err = rlp.DecodeBytes(v, &header); err != nil {
				return fmt.Errorf("decoding header from %x: %w", v, err)
			}
			if header.Number.Uint64() == 0 {
				continue
			}
			var parentK [40]byte
			binary.BigEndian.PutUint64(parentK[:], header.Number.Uint64()-1)
			copy(parentK[8:], header.ParentHash[:])
			var parentTdRec []byte
			if parentTdRec, err = tx.GetOne(kv.HeaderTD, parentK[:]); err != nil {
				return fmt.Errorf("reading parentTd Rec for %d: %w", header.Number.Uint64(), err)
			}
			var parentTd big.Int
			if err = rlp.DecodeBytes(parentTdRec, &parentTd); err != nil {
				return fmt.Errorf("decoding parent Td record for block %d, from %x: %w", header.Number.Uint64(), parentTdRec, err)
			}
			var td big.Int
			td.Add(&parentTd, header.Difficulty)
			var newHv []byte
			if newHv, err = rlp.EncodeToBytes(&td); err != nil {
				return fmt.Errorf("encoding td record for block %d: %w", header.Number.Uint64(), err)
			}
			if err = tx.Put(kv.HeaderTD, k, newHv); err != nil {
				return err
			}
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func advanceExec(chaindata string) error {
	db := mdbx.MustOpen(chaindata)
	defer db.Close()
	tx, err := db.BeginRw(context.Background())
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stageExec, err := stages.GetStageProgress(tx, stages.Execution)
	if err != nil {
		return err
	}
	log.Info("ID exec", "progress", stageExec)
	if err = stages.SaveStageProgress(tx, stages.Execution, stageExec+1); err != nil {
		return err
	}
	stageExec, err = stages.GetStageProgress(tx, stages.Execution)
	if err != nil {
		return err
	}
	log.Info("ID exec", "changed to", stageExec)
	if err = tx.Commit(); err != nil {
		return err
	}
	return nil
}

func backExec(chaindata string) error {
	db := mdbx.MustOpen(chaindata)
	defer db.Close()
	tx, err := db.BeginRw(context.Background())
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stageExec, err := stages.GetStageProgress(tx, stages.Execution)
	if err != nil {
		return err
	}
	log.Info("ID exec", "progress", stageExec)
	if err = stages.SaveStageProgress(tx, stages.Execution, stageExec-1); err != nil {
		return err
	}
	stageExec, err = stages.GetStageProgress(tx, stages.Execution)
	if err != nil {
		return err
	}
	log.Info("ID exec", "changed to", stageExec)
	if err = tx.Commit(); err != nil {
		return err
	}
	return nil
}

func fixState(chaindata string) error {
	db := mdbx.MustOpen(chaindata)
	defer db.Close()
	tx, err := db.BeginRw(context.Background())
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c, err1 := tx.RwCursor(kv.HeaderCanonical)
	if err1 != nil {
		return err1
	}
	defer c.Close()
	var prevHeaderKey [40]byte
	var k, v []byte
	for k, v, err = c.First(); err == nil && k != nil; k, v, err = c.Next() {
		var headerKey [40]byte
		copy(headerKey[:], k)
		copy(headerKey[8:], v)
		hv, herr := tx.GetOne(kv.Headers, headerKey[:])
		if herr != nil {
			return herr
		}
		if hv == nil {
			return fmt.Errorf("missing header record for %x", headerKey)
		}
		var header types.Header
		if err = rlp.DecodeBytes(hv, &header); err != nil {
			return fmt.Errorf("decoding header from %x: %w", v, err)
		}
		if header.Number.Uint64() > 1 {
			var parentK [40]byte
			binary.BigEndian.PutUint64(parentK[:], header.Number.Uint64()-1)
			copy(parentK[8:], header.ParentHash[:])
			if !bytes.Equal(parentK[:], prevHeaderKey[:]) {
				fmt.Printf("broken ancestry from %d %x (parent hash %x): prevKey %x\n", header.Number.Uint64(), v, header.ParentHash, prevHeaderKey)
			}
		}
		copy(prevHeaderKey[:], headerKey[:])
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func trimTxs(chaindata string) error {
	db := mdbx.MustOpen(chaindata)
	defer db.Close()
	tx, err := db.BeginRw(context.Background())
	if err != nil {
		return err
	}
	defer tx.Rollback()
	lastTxId, err := tx.ReadSequence(kv.EthTx)
	if err != nil {
		return err
	}
	txs, err1 := tx.RwCursor(kv.EthTx)
	if err1 != nil {
		return err1
	}
	defer txs.Close()
	bodies, err2 := tx.Cursor(kv.BlockBody)
	if err2 != nil {
		return err
	}
	defer bodies.Close()
	toDelete := roaring64.New()
	toDelete.AddRange(0, lastTxId)
	// Exclude transaction that are used, from the range
	for k, v, err := bodies.First(); k != nil; k, v, err = bodies.Next() {
		if err != nil {
			return err
		}
		var body types.BodyForStorage
		if err = rlp.DecodeBytes(v, &body); err != nil {
			return err
		}
		// Remove from the map
		toDelete.RemoveRange(body.BaseTxnID.U64(), body.BaseTxnID.LastSystemTx(body.TxCount)+1) //+1 to include last system tx in block into delete range
	}
	fmt.Printf("Number of txn records to delete: %d\n", toDelete.GetCardinality())
	// Takes 20min to iterate 1.4b
	toDelete2 := roaring64.New()
	var iterated int
	for k, _, err := txs.First(); k != nil; k, _, err = txs.Next() {
		if err != nil {
			return err
		}
		toDelete2.Add(binary.BigEndian.Uint64(k))
		iterated++
		if iterated%100_000_000 == 0 {
			fmt.Printf("Iterated %d\n", iterated)
		}
	}
	fmt.Printf("Number of txn records: %d\n", toDelete2.GetCardinality())
	toDelete.And(toDelete2)
	fmt.Printf("Number of txn records to delete: %d\n", toDelete.GetCardinality())
	fmt.Printf("Roaring size: %d\n", toDelete.GetSizeInBytes())

	iter := toDelete.Iterator()
	for {
		var deleted int
		for iter.HasNext() {
			txId := iter.Next()
			var key [8]byte
			binary.BigEndian.PutUint64(key[:], txId)
			if err = txs.Delete(key[:]); err != nil {
				return err
			}
			deleted++
			if deleted >= 10_000_000 {
				break
			}
		}
		if deleted == 0 {
			fmt.Printf("Nothing more to delete\n")
			break
		}
		fmt.Printf("Committing after deleting %d records\n", deleted)
		if err = tx.Commit(); err != nil {
			return err
		}
		txs.Close()
		tx, err = db.BeginRw(context.Background())
		if err != nil {
			return err
		}
		defer tx.Rollback()
		txs, err = tx.RwCursor(kv.EthTx)
		if err != nil {
			return err
		}
		defer txs.Close()
	}
	return nil
}

func scanTxs(chaindata string) error {
	db := mdbx.MustOpen(chaindata)
	defer db.Close()
	tx, err := db.BeginRo(context.Background())
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c, err := tx.Cursor(kv.EthTx)
	if err != nil {
		return err
	}
	defer c.Close()
	trTypes := make(map[byte]int)
	trTypesAl := make(map[byte]int)
	for k, v, err := c.First(); k != nil; k, v, err = c.Next() {
		if err != nil {
			return err
		}
		var tr types.Transaction
		if tr, err = types.DecodeTransaction(v); err != nil {
			return err
		}
		if _, ok := trTypes[tr.Type()]; !ok {
			fmt.Printf("Example for type %d:\n%x\n", tr.Type(), v)
		}
		trTypes[tr.Type()]++
		if tr.GetAccessList().StorageKeys() > 0 {
			if _, ok := trTypesAl[tr.Type()]; !ok {
				fmt.Printf("Example for type %d with AL:\n%x\n", tr.Type(), v)
			}
			trTypesAl[tr.Type()]++
		}
	}
	fmt.Printf("Transaction types: %v\n", trTypes)
	return nil
}

func devTx(chaindata string) error {
	db := mdbx.MustOpen(chaindata)
	defer db.Close()
	tx, err := db.BeginRo(context.Background())
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cc := tool.ChainConfig(tx)
	txn := types.NewTransaction(2, libcommon.Address{}, uint256.NewInt(100), 100_000, uint256.NewInt(1), []byte{1})
	signedTx, err := types.SignTx(txn, *types.LatestSigner(cc), core.DevnetSignPrivateKey)
	tool.Check(err)
	buf := bytes.NewBuffer(nil)
	err = signedTx.MarshalBinary(buf)
	tool.Check(err)
	fmt.Printf("%x\n", buf.Bytes())
	return nil
}

func chainConfig(name string) error {
	chainConfig := params.ChainConfigByChainName(name)
	if chainConfig == nil {
		return fmt.Errorf("unknown name: %s", name)
	}
	f, err := os.Create(filepath.Join("params", "chainspecs", fmt.Sprintf("%s.json", name)))
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(chainConfig); err != nil {
		return err
	}
	if err = w.Flush(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return nil
}

func keybytesToHex(str []byte) []byte {
	l := len(str)*2 + 1
	var nibbles = make([]byte, l)
	for i, b := range str {
		nibbles[i*2] = b / 16
		nibbles[i*2+1] = b % 16
	}
	nibbles[l-1] = 16
	return nibbles
}

func rmSnKey(chaindata string) error {
	db := mdbx.MustOpen(chaindata)
	defer db.Close()
	return db.Update(context.Background(), func(tx kv.RwTx) error {
		_ = tx.Delete(kv.DatabaseInfo, rawdb.SnapshotsKey)
		_ = tx.Delete(kv.DatabaseInfo, rawdb.SnapshotsHistoryKey)
		return nil
	})
}

func findLogs(chaindata string, block uint64, blockTotal uint64) error {
	db := mdbx.MustOpen(chaindata)
	defer db.Close()

	tx, txErr := db.BeginRo(context.Background())
	if txErr != nil {
		return txErr
	}
	defer tx.Rollback()
	logs, err := tx.Cursor(kv.Log)
	if err != nil {
		return err
	}
	defer logs.Close()

	reader := bytes.NewReader(nil)
	addrs := map[libcommon.Address]int{}
	topics := map[string]int{}

	for k, v, err := logs.Seek(dbutils.LogKey(block, 0)); k != nil; k, v, err = logs.Next() {
		if err != nil {
			return err
		}

		blockNum := binary.BigEndian.Uint64(k[:8])
		if blockNum >= block+blockTotal {
			break
		}

		var ll types.Logs
		reader.Reset(v)
		if err := cbor.Unmarshal(&ll, reader); err != nil {
			return fmt.Errorf("receipt unmarshal failed: %w, blocl=%d", err, blockNum)
		}

		for _, l := range ll {
			addrs[l.Address]++
			for _, topic := range l.Topics {
				topics[fmt.Sprintf("%x | %x", l.Address, topic)]++
			}
		}
	}
	addrsInv := map[int][]libcommon.Address{}
	topicsInv := map[int][]string{}
	for a, c := range addrs {
		addrsInv[c] = append(addrsInv[c], a)
	}
	counts := make([]int, 0, len(addrsInv))
	for c := range addrsInv {
		counts = append(counts, -c)
	}
	sort.Ints(counts)
	for i := 0; i < 10 && i < len(counts); i++ {
		as := addrsInv[-counts[i]]
		fmt.Printf("%d=%x\n", -counts[i], as)
	}
	for t, c := range topics {
		topicsInv[c] = append(topicsInv[c], t)
	}
	counts = make([]int, 0, len(topicsInv))
	for c := range topicsInv {
		counts = append(counts, -c)
	}
	sort.Ints(counts)
	for i := 0; i < 10 && i < len(counts); i++ {
		as := topicsInv[-counts[i]]
		fmt.Printf("%d=%s\n", -counts[i], as)
	}
	return nil
}

func iterate(filename string, prefix string) error {
	pBytes := common.FromHex(prefix)
	efFilename := filename + ".ef"
	viFilename := filename + ".vi"
	vFilename := filename + ".v"
	efDecomp, err := seg.NewDecompressor(efFilename)
	if err != nil {
		return err
	}
	defer efDecomp.Close()
	viIndex, err := recsplit.OpenIndex(viFilename)
	if err != nil {
		return err
	}
	defer viIndex.Close()
	r := recsplit.NewIndexReader(viIndex)
	vDecomp, err := seg.NewDecompressor(vFilename)
	if err != nil {
		return err
	}
	defer vDecomp.Close()
	gv := vDecomp.MakeGetter()
	g := efDecomp.MakeGetter()
	for g.HasNext() {
		key, _ := g.NextUncompressed()
		if bytes.HasPrefix(key, pBytes) {
			val, _ := g.NextUncompressed()
			ef, _ := eliasfano32.ReadEliasFano(val)
			efIt := ef.Iterator()
			fmt.Printf("[%x] =>", key)
			cnt := 0
			for efIt.HasNext() {
				txNum, err := efIt.Next()
				if err != nil {
					return err
				}
				var txKey [8]byte
				binary.BigEndian.PutUint64(txKey[:], txNum)
				offset, ok := r.Lookup2(txKey[:], key)
				if !ok {
					continue
				}
				gv.Reset(offset)
				v, _ := gv.Next(nil)
				fmt.Printf(" %d", txNum)
				if len(v) == 0 {
					fmt.Printf("*")
				}
				cnt++
				if cnt == 16 {
					fmt.Printf("\n")
					cnt = 0
				}
			}
			fmt.Printf("\n")
		} else {
			g.SkipUncompressed()
		}
	}
	return nil
}

func readSeg(chaindata string) error {
	vDecomp, err := seg.NewDecompressor(chaindata)
	if err != nil {
		return err
	}
	defer vDecomp.Close()
	g := vDecomp.MakeGetter()
	var buf []byte
	var count int
	var offset, nextPos uint64
	for g.HasNext() {
		buf, nextPos = g.Next(buf[:0])
		fmt.Printf("offset: %d, val: %x\n", offset, buf)
		offset = nextPos
		count++
	}
	return nil
}

func main() {
	debug.RaiseFdLimit()
	flag.Parse()

	logging.SetupLogger("hack")

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Error("could not create CPU profile", "err", err)
			return
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Error("could not start CPU profile", "err", err)
			return
		}
		defer pprof.StopCPUProfile()
	}
	go func() {
		if err := http.ListenAndServe("localhost:6960", nil); err != nil {
			log.Error("Failure in running pprof server", "err", err)
		}
	}()

	var err error
	switch *action {
	case "cfg":
		flow.TestGenCfg()

	case "testBlockHashes":
		testBlockHashes(*chaindata, *block, libcommon.HexToHash(*hash))

	case "dumpStorage":
		dumpStorage()

	case "current":
		printCurrentBlockNumber(*chaindata)

	case "bucket":
		printBucket(*chaindata)

	case "slice":
		dbSlice(*chaindata, *bucket, common.FromHex(*hash))

	case "extractHeaders":
		err = extractHeaders(*chaindata, uint64(*block), int64(*blockTotal))

	case "extractHashes":
		err = extractHashes(*chaindata, uint64(*block), int64(*blockTotal), *name)

	case "defrag":
		err = hackdb.Defrag()

	case "textInfo":
		err = hackdb.TextInfo(*chaindata, &strings.Builder{})

	case "extractBodies":
		err = extractBodies(*chaindata)

	case "repairCurrent":
		repairCurrent()

	case "printTxHashes":
		printTxHashes(*chaindata, uint64(*block))

	case "snapSizes":
		err = snapSizes(*chaindata)

	case "fixTd":
		err = fixTd(*chaindata)

	case "advanceExec":
		err = advanceExec(*chaindata)

	case "backExec":
		err = backExec(*chaindata)

	case "fixState":
		err = fixState(*chaindata)

	case "trimTxs":
		err = trimTxs(*chaindata)

	case "scanTxs":
		err = scanTxs(*chaindata)

	case "devTx":
		err = devTx(*chaindata)
	case "chainConfig":
		err = chainConfig(*name)
	case "findLogs":
		err = findLogs(*chaindata, uint64(*block), uint64(*blockTotal))
	case "iterate":
		err = iterate(*chaindata, *account)
	case "rmSnKey":
		err = rmSnKey(*chaindata)
	case "readSeg":
		err = readSeg(*chaindata)
	}

	if err != nil {
		fmt.Printf("Error: %v\n", err)
	}
}
