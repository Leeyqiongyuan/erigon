package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ledgerwatch/erigon-lib/wrap"

	"github.com/c2h5oh/datasize"
	"github.com/spf13/cobra"

	"github.com/ledgerwatch/erigon-lib/log/v3"

	chain2 "github.com/ledgerwatch/erigon-lib/chain"
	common2 "github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/datadir"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/temporal/historyv2"

	"github.com/ledgerwatch/erigon/cmd/hack/tool/fromdb"
	"github.com/ledgerwatch/erigon/cmd/utils"
	"github.com/ledgerwatch/erigon/common/debugprint"
	"github.com/ledgerwatch/erigon/core"
	"github.com/ledgerwatch/erigon/core/state"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/eth/ethconfig"
	"github.com/ledgerwatch/erigon/eth/stagedsync"
	"github.com/ledgerwatch/erigon/eth/stagedsync/stages"
	"github.com/ledgerwatch/erigon/eth/tracers/logger"
	"github.com/ledgerwatch/erigon/node/nodecfg"
	"github.com/ledgerwatch/erigon/params"
	erigoncli "github.com/ledgerwatch/erigon/turbo/cli"
	"github.com/ledgerwatch/erigon/turbo/debug"
	"github.com/ledgerwatch/erigon/turbo/shards"
)

var stateStages = &cobra.Command{
	Use: "state_stages",
	Short: `Run all StateStages (which happen after senders) in loop.
Examples: 
--unwind=1 --unwind.every=10  # 10 blocks forward, 1 block back, 10 blocks forward, ...
--unwind=10 --unwind.every=1  # 1 block forward, 10 blocks back, 1 blocks forward, ...
--unwind=10  # 10 blocks back, then stop
--integrity.fast=false --integrity.slow=false # Performs DB integrity checks each step. You can disable slow or fast checks.
--block # Stop at exact blocks
--chaindata.reference # When finish all cycles, does comparison to this db file.
		`,
	Example: "go run ./cmd/integration state_stages --datadir=... --verbosity=3 --unwind=100 --unwind.every=100000 --block=2000000",
	Run: func(cmd *cobra.Command, args []string) {
		logger := debug.SetupCobra(cmd, "integration")
		ctx, _ := common2.RootContext()
		cfg := &nodecfg.DefaultConfig
		utils.SetNodeConfigCobra(cmd, cfg)
		ethConfig := &ethconfig.Defaults
		ethConfig.Genesis = core.GenesisBlockByChainName(chain)
		erigoncli.ApplyFlagsForEthConfigCobra(cmd.Flags(), ethConfig)
		miningConfig := params.MiningConfig{}
		utils.SetupMinerCobra(cmd, &miningConfig)
		db, err := openDB(dbCfg(kv.ChainDB, chaindata), true, logger)
		if err != nil {
			logger.Error("Opening DB", "error", err)
			return
		}
		defer db.Close()

		if err := syncBySmallSteps(db, miningConfig, ctx, logger); err != nil {
			if !errors.Is(err, context.Canceled) {
				logger.Error(err.Error())
			}
			return
		}

		if referenceChaindata != "" {
			if err := compareStates(ctx, chaindata, referenceChaindata); err != nil {
				if !errors.Is(err, context.Canceled) {
					logger.Error(err.Error())
				}
				return
			}
		}
	},
}

var loopExecCmd = &cobra.Command{
	Use: "loop_exec",
	Run: func(cmd *cobra.Command, args []string) {
		logger := debug.SetupCobra(cmd, "integration")
		ctx, _ := common2.RootContext()
		db, err := openDB(dbCfg(kv.ChainDB, chaindata), true, logger)
		if err != nil {
			logger.Error("Opening DB", "error", err)
			return
		}
		defer db.Close()
		if unwind == 0 {
			unwind = 1
		}
		if err := loopExec(db, ctx, unwind, logger); err != nil {
			if !errors.Is(err, context.Canceled) {
				logger.Error(err.Error())
			}
			return
		}
	},
}

func init() {
	withConfig(stateStages)
	withDataDir2(stateStages)
	withReferenceChaindata(stateStages)
	withUnwind(stateStages)
	withUnwindEvery(stateStages)
	withBlock(stateStages)
	withIntegrityChecks(stateStages)
	withMining(stateStages)
	withChain(stateStages)
	withHeimdall(stateStages)
	withWorkers(stateStages)
	rootCmd.AddCommand(stateStages)

	withConfig(loopExecCmd)
	withDataDir(loopExecCmd)
	withBatchSize(loopExecCmd)
	withUnwind(loopExecCmd)
	withChain(loopExecCmd)
	withHeimdall(loopExecCmd)
	withWorkers(loopExecCmd)
	rootCmd.AddCommand(loopExecCmd)
}

func syncBySmallSteps(db kv.RwDB, miningConfig params.MiningConfig, ctx context.Context, logger1 log.Logger) error {
	dirs := datadir.New(datadirCli)
	if err := datadir.ApplyMigrations(dirs); err != nil {
		return err
	}

	sn, borSn, agg, _ := allSnapshots(ctx, db, logger1)
	defer sn.Close()
	defer borSn.Close()
	defer agg.Close()
	engine, vmConfig, stateStages, miningStages, miner := newSync(ctx, db, &miningConfig, logger1)
	chainConfig, pm := fromdb.ChainConfig(db), fromdb.PruneMode(db)

	tx, err := db.BeginRw(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	quit := ctx.Done()

	var batchSize datasize.ByteSize
	must(batchSize.UnmarshalText([]byte(batchSizeStr)))

	expectedAccountChanges := make(map[uint64]*historyv2.ChangeSet)
	expectedStorageChanges := make(map[uint64]*historyv2.ChangeSet)
	changeSetHook := func(blockNum uint64, csw *state.ChangeSetWriter) {
		if csw == nil {
			return
		}
		accountChanges, err := csw.GetAccountChanges()
		if err != nil {
			panic(err)
		}
		expectedAccountChanges[blockNum] = accountChanges

		storageChanges, err := csw.GetStorageChanges()
		if err != nil {
			panic(err)
		}
		if storageChanges.Len() > 0 {
			expectedStorageChanges[blockNum] = storageChanges
		}
	}

	stateStages.DisableStages(stages.Snapshots, stages.Headers, stages.BlockHashes, stages.Bodies, stages.Senders)
	changesAcc := shards.NewAccumulator()

	genesis := core.GenesisBlockByChainName(chain)
	syncCfg := ethconfig.Defaults.Sync
	syncCfg.ExecWorkerCount = int(workers)
	syncCfg.ReconWorkerCount = int(reconWorkers)

	br, _ := blocksIO(db, logger1)
	execCfg := stagedsync.StageExecuteBlocksCfg(db, pm, batchSize, changeSetHook, chainConfig, engine, vmConfig, changesAcc, false, true, dirs,
		br, nil, genesis, syncCfg, agg, nil)

	execUntilFunc := func(execToBlock uint64) stagedsync.ExecFunc {
		return func(badBlockUnwind bool, s *stagedsync.StageState, unwinder stagedsync.Unwinder, txc wrap.TxContainer, logger log.Logger) error {
			if err := stagedsync.SpawnExecuteBlocksStage(s, unwinder, txc, execToBlock, ctx, execCfg, logger); err != nil {
				return fmt.Errorf("spawnExecuteBlocksStage: %w", err)
			}
			return nil
		}
	}
	senderAtBlock := progress(tx, stages.Senders)
	execAtBlock := progress(tx, stages.Execution)

	var stopAt = senderAtBlock
	onlyOneUnwind := block == 0 && unwindEvery == 0 && unwind > 0
	backward := unwindEvery < unwind
	if onlyOneUnwind {
		stopAt = progress(tx, stages.Execution) - unwind
	} else if block > 0 && block < senderAtBlock {
		stopAt = block
	} else if backward {
		stopAt = 1
	}

	traceStart := func() {
		vmConfig.Tracer = logger.NewStructLogger(&logger.LogConfig{})
		vmConfig.Debug = true
	}
	traceStop := func(id int) {
		if !vmConfig.Debug {
			return
		}
		w, err3 := os.Create(fmt.Sprintf("trace_%d.txt", id))
		if err3 != nil {
			panic(err3)
		}
		encoder := json.NewEncoder(w)
		encoder.SetIndent(" ", " ")
		for _, l := range logger.FormatLogs(vmConfig.Tracer.(*logger.StructLogger).StructLogs()) {
			if err2 := encoder.Encode(l); err2 != nil {
				panic(err2)
			}
		}
		if err2 := w.Close(); err2 != nil {
			panic(err2)
		}

		vmConfig.Tracer = nil
		vmConfig.Debug = false
	}
	_, _ = traceStart, traceStop

	for (!backward && execAtBlock < stopAt) || (backward && execAtBlock > stopAt) {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		if err := tx.Commit(); err != nil {
			return err
		}
		tx, err = db.BeginRw(context.Background())
		if err != nil {
			return err
		}
		defer tx.Rollback()

		// All stages forward to `execStage + unwindEvery` block
		execAtBlock = progress(tx, stages.Execution)
		execToBlock := block
		if unwindEvery > 0 || unwind > 0 {
			if execAtBlock+unwindEvery > unwind {
				execToBlock = execAtBlock + unwindEvery - unwind
			} else {
				break
			}
		}
		if backward {
			if execToBlock < stopAt {
				execToBlock = stopAt
			}
		} else {
			if execToBlock > stopAt {
				execToBlock = stopAt + 1
				unwind = 0
			}
		}

		stateStages.MockExecFunc(stages.Execution, execUntilFunc(execToBlock))
		_ = stateStages.SetCurrentStage(stages.Execution)
		if _, err := stateStages.Run(db, wrap.TxContainer{Tx: tx}, false /* firstCycle */, false); err != nil {
			return err
		}

		//receiptsInDB := rawdb.ReadReceiptsByNumber(tx, progress(tx, stages.Execution)+1)

		if err := tx.Commit(); err != nil {
			return err
		}
		tx, err = db.BeginRw(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		execAtBlock = progress(tx, stages.Execution)

		if execAtBlock == stopAt {
			break
		}

		nextBlock, err := br.BlockByNumber(context.Background(), tx, execAtBlock+1)
		if err != nil {
			panic(err)
		}

		if miner.MiningConfig.Enabled && nextBlock != nil && nextBlock.Coinbase() != (common2.Address{}) {
			miner.MiningConfig.Etherbase = nextBlock.Coinbase()
			miner.MiningConfig.ExtraData = nextBlock.Extra()
			miningStages.MockExecFunc(stages.MiningCreateBlock, func(badBlockUnwind bool, s *stagedsync.StageState, u stagedsync.Unwinder, txc wrap.TxContainer, logger log.Logger) error {
				err = stagedsync.SpawnMiningCreateBlockStage(s, txc.Tx,
					stagedsync.StageMiningCreateBlockCfg(db, miner, *chainConfig, engine, nil, nil, dirs.Tmp, br),
					quit, logger)
				if err != nil {
					return err
				}
				miner.MiningBlock.Uncles = nextBlock.Uncles()
				miner.MiningBlock.Header.Time = nextBlock.Time()
				miner.MiningBlock.Header.GasLimit = nextBlock.GasLimit()
				miner.MiningBlock.Header.Difficulty = nextBlock.Difficulty()
				miner.MiningBlock.Header.Nonce = nextBlock.Nonce()
				miner.MiningBlock.PreparedTxs = types.NewTransactionsFixedOrder(nextBlock.Transactions())
				//debugprint.Headers(miningWorld.Block.Header, nextBlock.Header())
				return err
			})
			//miningStages.MockExecFunc(stages.MiningFinish, func(s *stagedsync.StageState, u stagedsync.Unwinder) error {
			//debugprint.Transactions(nextBlock.Transactions(), miningWorld.Block.Txs)
			//debugprint.Receipts(miningWorld.Block.Receipts, receiptsInDB)
			//return stagedsync.SpawnMiningFinishStage(s, tx, miningWorld.Block, cc.Engine(), chainConfig, quit)
			//})

			_ = miningStages.SetCurrentStage(stages.MiningCreateBlock)
			if _, err := miningStages.Run(db, wrap.TxContainer{Tx: tx}, false /* firstCycle */, false); err != nil {
				return err
			}
			tx.Rollback()
			tx, err = db.BeginRw(context.Background())
			if err != nil {
				return err
			}
			defer tx.Rollback()
			minedBlock := <-miner.MiningResultCh
			checkMinedBlock(nextBlock, minedBlock, chainConfig)
		}

		// Unwind all stages to `execStage - unwind` block
		if unwind == 0 {
			continue
		}

		to := execAtBlock - unwind
		if err := stateStages.UnwindTo(to, stagedsync.StagedUnwind, tx); err != nil {
			return err
		}

		if err := tx.Commit(); err != nil {
			return err
		}
		tx, err = db.BeginRw(context.Background())
		if err != nil {
			return err
		}
		defer tx.Rollback()

		// allow backward loop
		if unwind > 0 && unwindEvery > 0 {
			stopAt -= unwind
		}
	}

	return nil
}

func checkMinedBlock(b1, b2 *types.Block, chainConfig *chain2.Config) {
	if b1.Root() != b2.Root() ||
		(chainConfig.IsByzantium(b1.NumberU64()) && b1.ReceiptHash() != b2.ReceiptHash()) ||
		b1.TxHash() != b2.TxHash() ||
		b1.ParentHash() != b2.ParentHash() ||
		b1.UncleHash() != b2.UncleHash() ||
		b1.GasUsed() != b2.GasUsed() ||
		!bytes.Equal(b1.Extra(), b2.Extra()) { // TODO: Extra() doesn't need to be a copy for a read-only compare
		// Header()'s deep-copy doesn't matter here since it will panic anyway
		debugprint.Headers(b1.Header(), b2.Header())
		panic("blocks are not same")
	}
}

func loopExec(db kv.RwDB, ctx context.Context, unwind uint64, logger log.Logger) error {
	chainConfig := fromdb.ChainConfig(db)
	dirs, pm := datadir.New(datadirCli), fromdb.PruneMode(db)
	sn, borSn, agg, _ := allSnapshots(ctx, db, logger)
	defer sn.Close()
	defer borSn.Close()
	defer agg.Close()
	engine, vmConfig, sync, _, _ := newSync(ctx, db, nil, logger)

	tx, err := db.BeginRw(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	must(tx.Commit())
	tx, err = db.BeginRw(ctx)
	must(err)
	defer tx.Rollback()
	sync.DisableAllStages()
	sync.EnableStages(stages.Execution)
	var batchSize datasize.ByteSize
	must(batchSize.UnmarshalText([]byte(batchSizeStr)))
	from := progress(tx, stages.Execution)
	to := from + unwind

	genesis := core.GenesisBlockByChainName(chain)
	syncCfg := ethconfig.Defaults.Sync
	syncCfg.ExecWorkerCount = int(workers)
	syncCfg.ReconWorkerCount = int(reconWorkers)

	initialCycle := false
	br, _ := blocksIO(db, logger)
	cfg := stagedsync.StageExecuteBlocksCfg(db, pm, batchSize, nil, chainConfig, engine, vmConfig, nil,
		/*stateStream=*/ false,
		/*badBlockHalt=*/ true, dirs, br, nil, genesis, syncCfg, agg, nil)

	// set block limit of execute stage
	sync.MockExecFunc(stages.Execution, func(badBlockUnwind bool, stageState *stagedsync.StageState, unwinder stagedsync.Unwinder, txc wrap.TxContainer, logger log.Logger) error {
		if err = stagedsync.SpawnExecuteBlocksStage(stageState, sync, txc, to, ctx, cfg, logger); err != nil {
			return fmt.Errorf("spawnExecuteBlocksStage: %w", err)
		}
		return nil
	})

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		_ = sync.SetCurrentStage(stages.Execution)
		t := time.Now()
		if _, err = sync.Run(db, wrap.TxContainer{Tx: tx}, initialCycle, false); err != nil {
			return err
		}
		logger.Info("[Integration] ", "loop time", time.Since(t))
		tx.Rollback()
		tx, err = db.BeginRw(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}
}
