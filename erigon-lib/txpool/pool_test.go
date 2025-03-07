/*
   Copyright 2021 The Erigon contributors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package txpool

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"math/big"
	"testing"

	gokzg4844 "github.com/crate-crypto/go-kzg-4844"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ledgerwatch/erigon-lib/common/datadir"
	"github.com/ledgerwatch/erigon-lib/kv/temporal/temporaltest"
	"github.com/ledgerwatch/erigon-lib/log/v3"

	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/fixedgas"
	"github.com/ledgerwatch/erigon-lib/common/hexutility"
	"github.com/ledgerwatch/erigon-lib/common/u256"
	"github.com/ledgerwatch/erigon-lib/crypto/kzg"
	"github.com/ledgerwatch/erigon-lib/gointerfaces"
	remote "github.com/ledgerwatch/erigon-lib/gointerfaces/remoteproto"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/kvcache"
	"github.com/ledgerwatch/erigon-lib/kv/memdb"
	"github.com/ledgerwatch/erigon-lib/txpool/txpoolcfg"
	"github.com/ledgerwatch/erigon-lib/types"
)

func TestNonceFromAddress(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	ch := make(chan types.Announcements, 100)

	coreDB, _ := temporaltest.NewTestDB(t, datadir.New(t.TempDir()))
	db := memdb.NewTestPoolDB(t)

	cfg := txpoolcfg.DefaultConfig
	sendersCache := kvcache.New(kvcache.DefaultCoherentConfig)
	pool, err := New(ch, coreDB, cfg, sendersCache, *u256.N1, nil, nil, nil, fixedgas.DefaultMaxBlobsPerBlock, nil, log.New())
	assert.NoError(err)
	require.True(pool != nil)
	ctx := context.Background()
	var stateVersionID uint64 = 0
	pendingBaseFee := uint64(200000)
	// start blocks from 0, set empty hash - then kvcache will also work on this
	h1 := gointerfaces.ConvertHashToH256([32]byte{})
	change := &remote.StateChangeBatch{
		StateVersionId:      stateVersionID,
		PendingBlockBaseFee: pendingBaseFee,
		BlockGasLimit:       1000000,
		ChangeBatch: []*remote.StateChange{
			{BlockHeight: 0, BlockHash: h1},
		},
	}
	var addr [20]byte
	addr[0] = 1
	v := types.EncodeAccountBytesV3(2, uint256.NewInt(1*common.Ether), make([]byte, 32), 1)
	change.ChangeBatch[0].Changes = append(change.ChangeBatch[0].Changes, &remote.AccountChange{
		Action:  remote.Action_UPSERT,
		Address: gointerfaces.ConvertAddressToH160(addr),
		Data:    v,
	})
	tx, err := db.BeginRw(ctx)
	require.NoError(err)
	defer tx.Rollback()
	err = pool.OnNewBlock(ctx, change, types.TxSlots{}, types.TxSlots{}, types.TxSlots{}, tx)
	assert.NoError(err)

	{
		var txSlots types.TxSlots
		txSlot1 := &types.TxSlot{
			Tip:    *uint256.NewInt(300000),
			FeeCap: *uint256.NewInt(300000),
			Gas:    100000,
			Nonce:  3,
		}
		txSlot1.IDHash[0] = 1
		txSlots.Append(txSlot1, addr[:], true)

		reasons, err := pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		for _, reason := range reasons {
			assert.Equal(txpoolcfg.Success, reason, reason.String())
		}
	}

	{
		txSlots := types.TxSlots{}
		txSlot2 := &types.TxSlot{
			Tip:    *uint256.NewInt(300000),
			FeeCap: *uint256.NewInt(300000),
			Gas:    100000,
			Nonce:  4,
		}
		txSlot2.IDHash[0] = 2
		txSlot3 := &types.TxSlot{
			Tip:    *uint256.NewInt(300000),
			FeeCap: *uint256.NewInt(300000),
			Gas:    100000,
			Nonce:  6,
		}
		txSlot3.IDHash[0] = 3
		txSlots.Append(txSlot2, addr[:], true)
		txSlots.Append(txSlot3, addr[:], true)
		reasons, err := pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		for _, reason := range reasons {
			assert.Equal(txpoolcfg.Success, reason, reason.String())
		}
		nonce, ok := pool.NonceFromAddress(addr)
		assert.True(ok)
		assert.Equal(uint64(6), nonce)
	}
	// test too expensive tx
	{
		var txSlots types.TxSlots
		txSlot1 := &types.TxSlot{
			Tip:    *uint256.NewInt(300000),
			FeeCap: *uint256.NewInt(9 * common.Ether),
			Gas:    100000,
			Nonce:  3,
		}
		txSlot1.IDHash[0] = 4
		txSlots.Append(txSlot1, addr[:], true)
		reasons, err := pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		for _, reason := range reasons {
			assert.Equal(txpoolcfg.InsufficientFunds, reason, reason.String())
		}
	}

	// test too low nonce
	{
		var txSlots types.TxSlots
		txSlot1 := &types.TxSlot{
			Tip:    *uint256.NewInt(300000),
			FeeCap: *uint256.NewInt(300000),
			Gas:    100000,
			Nonce:  1,
		}
		txSlot1.IDHash[0] = 5
		txSlots.Append(txSlot1, addr[:], true)
		reasons, err := pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		for _, reason := range reasons {
			assert.Equal(txpoolcfg.NonceTooLow, reason, reason.String())
		}
	}
}

func TestReplaceWithHigherFee(t *testing.T) {
	t.Skip("TODO")
	assert, require := assert.New(t), require.New(t)
	ch := make(chan types.Announcements, 100)
	coreDB, _ := temporaltest.NewTestDB(t, datadir.New(t.TempDir()))
	db := memdb.NewTestPoolDB(t)

	cfg := txpoolcfg.DefaultConfig
	sendersCache := kvcache.New(kvcache.DefaultCoherentConfig)
	pool, err := New(ch, coreDB, cfg, sendersCache, *u256.N1, nil, nil, nil, fixedgas.DefaultMaxBlobsPerBlock, nil, log.New())
	assert.NoError(err)
	require.NotEqual(nil, pool)
	ctx := context.Background()
	var stateVersionID uint64 = 0
	pendingBaseFee := uint64(200000)
	// start blocks from 0, set empty hash - then kvcache will also work on this
	h1 := gointerfaces.ConvertHashToH256([32]byte{})
	change := &remote.StateChangeBatch{
		StateVersionId:      stateVersionID,
		PendingBlockBaseFee: pendingBaseFee,
		BlockGasLimit:       1000000,
		ChangeBatch: []*remote.StateChange{
			{BlockHeight: 0, BlockHash: h1},
		},
	}
	var addr [20]byte
	addr[0] = 1
	v := make([]byte, types.EncodeSenderLengthForStorage(2, *uint256.NewInt(1 * common.Ether)))
	types.EncodeSender(2, *uint256.NewInt(1 * common.Ether), v)
	change.ChangeBatch[0].Changes = append(change.ChangeBatch[0].Changes, &remote.AccountChange{
		Action:  remote.Action_UPSERT,
		Address: gointerfaces.ConvertAddressToH160(addr),
		Data:    v,
	})
	tx, err := db.BeginRw(ctx)
	require.NoError(err)
	defer tx.Rollback()
	err = pool.OnNewBlock(ctx, change, types.TxSlots{}, types.TxSlots{}, types.TxSlots{}, tx)
	assert.NoError(err)

	{
		var txSlots types.TxSlots
		txSlot := &types.TxSlot{
			Tip:    *uint256.NewInt(300000),
			FeeCap: *uint256.NewInt(300000),
			Gas:    100000,
			Nonce:  3,
		}
		txSlot.IDHash[0] = 1
		txSlots.Append(txSlot, addr[:], true)

		reasons, err := pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		for _, reason := range reasons {
			assert.Equal(txpoolcfg.Success, reason, reason.String())
		}
	}
	// Bumped only feeCap, transaction not accepted
	{
		txSlots := types.TxSlots{}
		txSlot := &types.TxSlot{
			Tip:    *uint256.NewInt(300000),
			FeeCap: *uint256.NewInt(3000000),
			Gas:    100000,
			Nonce:  3,
		}
		txSlot.IDHash[0] = 2
		txSlots.Append(txSlot, addr[:], true)
		reasons, err := pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		for _, reason := range reasons {
			assert.Equal(txpoolcfg.NotReplaced, reason, reason.String())
		}
		nonce, ok := pool.NonceFromAddress(addr)
		assert.True(ok)
		assert.Equal(uint64(3), nonce)
	}
	// Bumped only tip, transaction not accepted
	{
		txSlots := types.TxSlots{}
		txSlot := &types.TxSlot{
			Tip:    *uint256.NewInt(3000000),
			FeeCap: *uint256.NewInt(300000),
			Gas:    100000,
			Nonce:  3,
		}
		txSlot.IDHash[0] = 3
		txSlots.Append(txSlot, addr[:], true)
		reasons, err := pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		for _, reason := range reasons {
			assert.Equal(txpoolcfg.NotReplaced, reason, reason.String())
		}
		nonce, ok := pool.NonceFromAddress(addr)
		assert.True(ok)
		assert.Equal(uint64(3), nonce)
	}
	// Bumped both tip and feeCap by 10%, txn accepted
	{
		txSlots := types.TxSlots{}
		txSlot := &types.TxSlot{
			Tip:    *uint256.NewInt(330000),
			FeeCap: *uint256.NewInt(330000),
			Gas:    100000,
			Nonce:  3,
		}
		txSlot.IDHash[0] = 4
		txSlots.Append(txSlot, addr[:], true)
		reasons, err := pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		for _, reason := range reasons {
			assert.Equal(txpoolcfg.Success, reason, reason.String())
		}
		nonce, ok := pool.NonceFromAddress(addr)
		assert.True(ok)
		assert.Equal(uint64(3), nonce)
	}
}

func TestReverseNonces(t *testing.T) {
	t.Skip("TODO")
	assert, require := assert.New(t), require.New(t)
	ch := make(chan types.Announcements, 100)
	coreDB, _ := temporaltest.NewTestDB(t, datadir.New(t.TempDir()))
	db := memdb.NewTestPoolDB(t)

	cfg := txpoolcfg.DefaultConfig
	sendersCache := kvcache.New(kvcache.DefaultCoherentConfig)
	pool, err := New(ch, coreDB, cfg, sendersCache, *u256.N1, nil, nil, nil, fixedgas.DefaultMaxBlobsPerBlock, nil, log.New())
	assert.NoError(err)
	require.True(pool != nil)
	ctx := context.Background()
	var stateVersionID uint64 = 0
	pendingBaseFee := uint64(1_000_000)
	// start blocks from 0, set empty hash - then kvcache will also work on this
	h1 := gointerfaces.ConvertHashToH256([32]byte{})
	change := &remote.StateChangeBatch{
		StateVersionId:      stateVersionID,
		PendingBlockBaseFee: pendingBaseFee,
		BlockGasLimit:       1000000,
		ChangeBatch: []*remote.StateChange{
			{BlockHeight: 0, BlockHash: h1},
		},
	}
	var addr [20]byte
	addr[0] = 1
	v := make([]byte, types.EncodeSenderLengthForStorage(2, *uint256.NewInt(1 * common.Ether)))
	types.EncodeSender(2, *uint256.NewInt(1 * common.Ether), v)
	change.ChangeBatch[0].Changes = append(change.ChangeBatch[0].Changes, &remote.AccountChange{
		Action:  remote.Action_UPSERT,
		Address: gointerfaces.ConvertAddressToH160(addr),
		Data:    v,
	})
	tx, err := db.BeginRw(ctx)
	require.NoError(err)
	defer tx.Rollback()
	err = pool.OnNewBlock(ctx, change, types.TxSlots{}, types.TxSlots{}, types.TxSlots{}, tx)
	assert.NoError(err)
	// 1. Send high fee transaction with nonce gap
	{
		var txSlots types.TxSlots
		txSlot := &types.TxSlot{
			Tip:    *uint256.NewInt(500_000),
			FeeCap: *uint256.NewInt(3_000_000),
			Gas:    100000,
			Nonce:  3,
		}
		txSlot.IDHash[0] = 1
		txSlots.Append(txSlot, addr[:], true)

		reasons, err := pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		for _, reason := range reasons {
			assert.Equal(txpoolcfg.Success, reason, reason.String())
		}
	}
	select {
	case annoucements := <-ch:
		for i := 0; i < annoucements.Len(); i++ {
			_, _, hash := annoucements.At(i)
			fmt.Printf("propagated hash %x\n", hash)
		}
	default:

	}
	// 2. Send low fee (below base fee) transaction without nonce gap
	{
		var txSlots types.TxSlots
		txSlot := &types.TxSlot{
			Tip:    *uint256.NewInt(500_000),
			FeeCap: *uint256.NewInt(500_000),
			Gas:    100000,
			Nonce:  2,
		}
		txSlot.IDHash[0] = 2
		txSlots.Append(txSlot, addr[:], true)

		reasons, err := pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		for _, reason := range reasons {
			assert.Equal(txpoolcfg.Success, reason, reason.String())
		}
	}
	select {
	case annoucements := <-ch:
		for i := 0; i < annoucements.Len(); i++ {
			_, _, hash := annoucements.At(i)
			fmt.Printf("propagated hash %x\n", hash)
		}
	default:

	}

	{
		var txSlots types.TxSlots
		txSlot := &types.TxSlot{
			Tip:    *uint256.NewInt(600_000),
			FeeCap: *uint256.NewInt(3_000_000),
			Gas:    100000,
			Nonce:  2,
		}
		txSlot.IDHash[0] = 3
		txSlots.Append(txSlot, addr[:], true)

		reasons, err := pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		for _, reason := range reasons {
			assert.Equal(txpoolcfg.Success, reason, reason.String())
		}
	}
	select {
	case annoucements := <-ch:
		for i := 0; i < annoucements.Len(); i++ {
			_, _, hash := annoucements.At(i)
			fmt.Printf("propagated hash %x\n", hash)
		}
	default:

	}
}

// When local transaction is send to the pool, but it cannot replace existing transaction,
// the existing transaction gets "poked" and is getting re-broadcasted
// this is a workaround for cases when transactions are getting stuck for strange reasons
// even though logs show they are broadcast
func TestTxPoke(t *testing.T) {
	t.Skip("TODO")
	assert, require := assert.New(t), require.New(t)
	ch := make(chan types.Announcements, 100)
	coreDB, _ := temporaltest.NewTestDB(t, datadir.New(t.TempDir()))
	db := memdb.NewTestPoolDB(t)

	cfg := txpoolcfg.DefaultConfig
	sendersCache := kvcache.New(kvcache.DefaultCoherentConfig)
	pool, err := New(ch, coreDB, cfg, sendersCache, *u256.N1, nil, nil, nil, fixedgas.DefaultMaxBlobsPerBlock, nil, log.New())
	assert.NoError(err)
	require.True(pool != nil)
	ctx := context.Background()
	var stateVersionID uint64 = 0
	pendingBaseFee := uint64(200000)
	// start blocks from 0, set empty hash - then kvcache will also work on this
	h1 := gointerfaces.ConvertHashToH256([32]byte{})
	change := &remote.StateChangeBatch{
		StateVersionId:      stateVersionID,
		PendingBlockBaseFee: pendingBaseFee,
		BlockGasLimit:       1000000,
		ChangeBatch: []*remote.StateChange{
			{BlockHeight: 0, BlockHash: h1},
		},
	}
	var addr [20]byte
	addr[0] = 1
	v := make([]byte, types.EncodeSenderLengthForStorage(2, *uint256.NewInt(1 * common.Ether)))
	types.EncodeSender(2, *uint256.NewInt(1 * common.Ether), v)
	change.ChangeBatch[0].Changes = append(change.ChangeBatch[0].Changes, &remote.AccountChange{
		Action:  remote.Action_UPSERT,
		Address: gointerfaces.ConvertAddressToH160(addr),
		Data:    v,
	})
	tx, err := db.BeginRw(ctx)
	require.NoError(err)
	defer tx.Rollback()
	err = pool.OnNewBlock(ctx, change, types.TxSlots{}, types.TxSlots{}, types.TxSlots{}, tx)
	assert.NoError(err)

	var idHash types.Hashes
	{
		var txSlots types.TxSlots
		txSlot := &types.TxSlot{
			Tip:    *uint256.NewInt(300000),
			FeeCap: *uint256.NewInt(300000),
			Gas:    100000,
			Nonce:  2,
		}
		txSlot.IDHash[0] = 1
		idHash = append(idHash, txSlot.IDHash[:]...)
		txSlots.Append(txSlot, addr[:], true)

		reasons, err := pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		for _, reason := range reasons {
			assert.Equal(txpoolcfg.Success, reason, reason.String())
		}
	}
	var promoted types.Announcements
	select {
	case promoted = <-ch:
		if !bytes.Equal(idHash, promoted.DedupHashes()) {
			t.Errorf("expected promoted %x, got %x", idHash, promoted)
		}
	default:
		t.Errorf("expected promotion")
	}
	// Send the same transaction, not accepted
	{
		txSlots := types.TxSlots{}
		txSlot := &types.TxSlot{
			Tip:    *uint256.NewInt(300000),
			FeeCap: *uint256.NewInt(300000),
			Gas:    100000,
			Nonce:  2,
		}
		txSlot.IDHash[0] = 1
		txSlots.Append(txSlot, addr[:], true)
		reasons, err := pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		for _, reason := range reasons {
			assert.Equal(txpoolcfg.DuplicateHash, reason, reason.String())
		}
		nonce, ok := pool.NonceFromAddress(addr)
		assert.True(ok)
		assert.Equal(uint64(2), nonce)
	}
	// Even though transaction not replaced, it gets poked
	select {
	case promoted = <-ch:
		if !bytes.Equal(idHash, promoted.Hashes()) {
			t.Errorf("expected promoted %x, got %x", idHash, promoted)
		}
	default:
		t.Errorf("expected promotion")
	}
	// Send different transaction, but only with tip bumped
	{
		txSlots := types.TxSlots{}
		txSlot := &types.TxSlot{
			Tip:    *uint256.NewInt(3000000),
			FeeCap: *uint256.NewInt(300000),
			Gas:    100000,
			Nonce:  2,
		}
		txSlot.IDHash[0] = 2
		txSlots.Append(txSlot, addr[:], true)
		reasons, err := pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		for _, reason := range reasons {
			assert.Equal(txpoolcfg.NotReplaced, reason, reason.String())
		}
		nonce, ok := pool.NonceFromAddress(addr)
		assert.True(ok)
		assert.Equal(uint64(2), nonce)
	}
	// Even though transaction not replaced, it gets poked
	select {
	case promoted = <-ch:
		if !bytes.Equal(idHash, promoted.Hashes()) {
			t.Errorf("expected promoted %x, got %x", idHash, promoted)
		}
	default:
		t.Errorf("expected promotion")
	}

	// Send the same transaction, but as remote
	{
		txSlots := types.TxSlots{}
		txSlot := &types.TxSlot{
			Tip:    *uint256.NewInt(300000),
			FeeCap: *uint256.NewInt(300000),
			Gas:    100000,
			Nonce:  2,
		}
		txSlot.IDHash[0] = 1
		txSlots.Append(txSlot, addr[:], true)
		pool.AddRemoteTxs(ctx, txSlots)
		nonce, ok := pool.NonceFromAddress(addr)
		assert.True(ok)
		assert.Equal(uint64(2), nonce)
	}
	// Remote transactions do not cause pokes
	select {
	case <-ch:
		t.Errorf("remote transactions should not cause re-broadcast")
	default:
	}
	// Send different transaction, but only with tip bumped, as a remote
	{
		txSlots := types.TxSlots{}
		txSlot := &types.TxSlot{
			Tip:    *uint256.NewInt(3000000),
			FeeCap: *uint256.NewInt(3000000),
			Gas:    100000,
			Nonce:  2,
		}
		txSlot.IDHash[0] = 2
		txSlots.Append(txSlot, addr[:], true)
		pool.AddRemoteTxs(ctx, txSlots)
		nonce, ok := pool.NonceFromAddress(addr)
		assert.True(ok)
		assert.Equal(uint64(2), nonce)
	}
	// Remote transactions do not cause pokes
	select {
	case <-ch:
		t.Errorf("remote transactions should not cause re-broadcast")
	default:
	}
}

func TestShanghaiIntrinsicGas(t *testing.T) {
	cases := map[string]struct {
		expected       uint64
		dataLen        uint64
		dataNonZeroLen uint64
		creation       bool
		isShanghai     bool
	}{
		"simple no data": {
			expected:       21000,
			dataLen:        0,
			dataNonZeroLen: 0,
			creation:       false,
			isShanghai:     false,
		},
		"simple with data": {
			expected:       21512,
			dataLen:        32,
			dataNonZeroLen: 32,
			creation:       false,
			isShanghai:     false,
		},
		"creation with data no shanghai": {
			expected:       53512,
			dataLen:        32,
			dataNonZeroLen: 32,
			creation:       true,
			isShanghai:     false,
		},
		"creation with single word and shanghai": {
			expected:       53514, // additional gas for single word
			dataLen:        32,
			dataNonZeroLen: 32,
			creation:       true,
			isShanghai:     true,
		},
		"creation between word 1 and 2 and shanghai": {
			expected:       53532, // additional gas for going into 2nd word although not filling it
			dataLen:        33,
			dataNonZeroLen: 33,
			creation:       true,
			isShanghai:     true,
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			gas, reason := txpoolcfg.CalcIntrinsicGas(c.dataLen, c.dataNonZeroLen, nil, c.creation, true, true, c.isShanghai)
			if reason != txpoolcfg.Success {
				t.Errorf("expected success but got reason %v", reason)
			}
			if gas != c.expected {
				t.Errorf("expected %v but got %v", c.expected, gas)
			}
		})
	}
}

func TestShanghaiValidateTx(t *testing.T) {
	asrt := assert.New(t)
	tests := map[string]struct {
		expected   txpoolcfg.DiscardReason
		dataLen    int
		isShanghai bool
		creation   bool
	}{
		"no shanghai": {
			expected:   txpoolcfg.Success,
			dataLen:    32,
			isShanghai: false,
			creation:   true,
		},
		"shanghai within bounds": {
			expected:   txpoolcfg.Success,
			dataLen:    32,
			isShanghai: true,
			creation:   true,
		},
		"shanghai exactly on bound - create tx": {
			expected:   txpoolcfg.Success,
			dataLen:    fixedgas.MaxInitCodeSize,
			isShanghai: true,
			creation:   true,
		},
		"shanghai one over bound - create tx": {
			expected:   txpoolcfg.InitCodeTooLarge,
			dataLen:    fixedgas.MaxInitCodeSize + 1,
			isShanghai: true,
			creation:   true,
		},
		"shanghai exactly on bound - calldata tx": {
			expected:   txpoolcfg.Success,
			dataLen:    fixedgas.MaxInitCodeSize,
			isShanghai: true,
			creation:   false,
		},
		"shanghai one over bound - calldata tx": {
			expected:   txpoolcfg.Success,
			dataLen:    fixedgas.MaxInitCodeSize + 1,
			isShanghai: true,
			creation:   false,
		},
	}

	logger := log.New()

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			ch := make(chan types.Announcements, 100)
			coreDB, _ := temporaltest.NewTestDB(t, datadir.New(t.TempDir()))

			cfg := txpoolcfg.DefaultConfig

			var shanghaiTime *big.Int
			if test.isShanghai {
				shanghaiTime = big.NewInt(0)
			}

			cache := &kvcache.DummyCache{}
			pool, err := New(ch, coreDB, cfg, cache, *u256.N1, shanghaiTime, nil /* agraBlock */, nil /* cancunTime */, fixedgas.DefaultMaxBlobsPerBlock, nil, logger)
			asrt.NoError(err)
			ctx := context.Background()
			tx, err := coreDB.BeginRw(ctx)
			defer tx.Rollback()
			asrt.NoError(err)

			sndr := sender{nonce: 0, balance: *uint256.NewInt(math.MaxUint64)}
			sndrBytes := make([]byte, types.EncodeSenderLengthForStorage(sndr.nonce, sndr.balance))
			types.EncodeSender(sndr.nonce, sndr.balance, sndrBytes)
			err = tx.Put(kv.PlainState, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, sndrBytes)
			asrt.NoError(err)

			txn := &types.TxSlot{
				DataLen:  test.dataLen,
				FeeCap:   *uint256.NewInt(21000),
				Gas:      500000,
				SenderID: 0,
				Creation: test.creation,
			}

			txns := types.TxSlots{
				Txs:     append([]*types.TxSlot{}, txn),
				Senders: types.Addresses{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
			}
			err = pool.senders.registerNewSenders(&txns, logger)
			asrt.NoError(err)
			view, err := cache.View(ctx, tx)
			asrt.NoError(err)

			reason := pool.validateTx(txn, false, view)

			if reason != test.expected {
				t.Errorf("expected %v, got %v", test.expected, reason)
			}
		})
	}
}

// Blob gas price bump + other requirements to replace existing txns in the pool
func TestBlobTxReplacement(t *testing.T) {
	t.Skip("TODO")
	assert, require := assert.New(t), require.New(t)
	ch := make(chan types.Announcements, 5)
	coreDB, _ := temporaltest.NewTestDB(t, datadir.New(t.TempDir()))
	db := memdb.NewTestPoolDB(t)

	cfg := txpoolcfg.DefaultConfig
	sendersCache := kvcache.New(kvcache.DefaultCoherentConfig)
	pool, err := New(ch, coreDB, cfg, sendersCache, *u256.N1, common.Big0, nil, common.Big0, fixedgas.DefaultMaxBlobsPerBlock, nil, log.New())
	assert.NoError(err)
	require.True(pool != nil)
	ctx := context.Background()
	var stateVersionID uint64 = 0

	h1 := gointerfaces.ConvertHashToH256([32]byte{})
	change := &remote.StateChangeBatch{
		StateVersionId:       stateVersionID,
		PendingBlockBaseFee:  200_000,
		BlockGasLimit:        1000000,
		PendingBlobFeePerGas: 100_000,
		ChangeBatch: []*remote.StateChange{
			{BlockHeight: 0, BlockHash: h1},
		},
	}
	var addr [20]byte
	addr[0] = 1

	// Add 1 eth to the user account, as a part of change
	v := make([]byte, types.EncodeSenderLengthForStorage(2, *uint256.NewInt(1 * common.Ether)))
	types.EncodeSender(2, *uint256.NewInt(1 * common.Ether), v)

	change.ChangeBatch[0].Changes = append(change.ChangeBatch[0].Changes, &remote.AccountChange{
		Action:  remote.Action_UPSERT,
		Address: gointerfaces.ConvertAddressToH160(addr),
		Data:    v,
	})
	tx, err := db.BeginRw(ctx)
	require.NoError(err)
	defer tx.Rollback()
	err = pool.OnNewBlock(ctx, change, types.TxSlots{}, types.TxSlots{}, types.TxSlots{}, tx)
	assert.NoError(err)

	tip, feeCap, blobFeeCap := uint256.NewInt(100_000), uint256.NewInt(200_000), uint256.NewInt(200_000)

	//add a blob txn to the pool
	{
		txSlots := types.TxSlots{}
		blobTxn := makeBlobTx()

		blobTxn.IDHash[0] = 0x00
		blobTxn.Nonce = 0x2
		txSlots.Append(&blobTxn, addr[:], true)
		reasons, err := pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		t.Logf("Reasons %v", reasons)
		for _, reason := range reasons {
			assert.Equal(txpoolcfg.Success, reason, reason.String())
		}
	}

	{
		// try to replace it with 5% extra blob gas, 2x higher txn fee - should fail
		txSlots := types.TxSlots{}
		blobTxn := makeBlobTx()
		blobTxn.Nonce = 0x2
		blobTxn.FeeCap.Mul(uint256.NewInt(2), feeCap)
		blobTxn.Tip.Mul(uint256.NewInt(2), tip)
		//increase blobFeeCap by 10% - no good
		blobTxn.BlobFeeCap.Add(blobFeeCap, uint256.NewInt(1).Div(blobFeeCap, uint256.NewInt(10)))
		blobTxn.IDHash[0] = 0x01
		txSlots.Append(&blobTxn, addr[:], true)
		reasons, err := pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		t.Logf("Reasons %v", reasons)
		for _, reason := range reasons {
			assert.Equal(txpoolcfg.ReplaceUnderpriced, reason, reason.String())
		}
	}

	{
		txSlots := types.TxSlots{}
		//try to replace it with a regular txn - should fail
		regularTx := types.TxSlot{
			DataLen:    32,
			FeeCap:     *uint256.NewInt(1).Mul(uint256.NewInt(10), feeCap), //10x the previous
			Tip:        *uint256.NewInt(1).Mul(uint256.NewInt(10), tip),
			BlobFeeCap: *uint256.NewInt(1).Mul(uint256.NewInt(10), blobFeeCap),
			Gas:        500000,
			SenderID:   0,
			Creation:   true,
			Nonce:      0x2,
		}
		regularTx.IDHash[0] = 0x02
		txSlots.Append(&regularTx, addr[:], true)
		reasons, err := pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		t.Logf("Reasons %v", reasons)
		for _, reason := range reasons {
			assert.Equal(txpoolcfg.BlobTxReplace, reason, reason.String())
		}
	}

	// Try to replace it with required price bump (configured in pool.cfg.BlobPriceBump for blob txns) to all transaction fields - should be successful only if all are bumped
	{
		blobTxn := makeBlobTx()
		origTip := blobTxn.Tip
		origFee := blobTxn.FeeCap
		blobTxn.Nonce = 0x2
		blobTxn.IDHash[0] = 0x03
		txSlots := types.TxSlots{}
		txSlots.Append(&blobTxn, addr[:], true)

		// Get the config of the pool for BlobPriceBump and bump prices
		requiredPriceBump := pool.cfg.BlobPriceBump

		// Bump the tip only
		blobTxn.Tip.MulDivOverflow(tip, uint256.NewInt(requiredPriceBump+100), uint256.NewInt(100))
		reasons, err := pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		assert.Equal(txpoolcfg.ReplaceUnderpriced, reasons[0], reasons[0].String())

		// Bump the fee + tip
		blobTxn.FeeCap.MulDivOverflow(feeCap, uint256.NewInt(requiredPriceBump+100), uint256.NewInt(100))
		reasons, err = pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		assert.Equal(txpoolcfg.ReplaceUnderpriced, reasons[0], reasons[0].String())

		// Bump only Feecap
		blobTxn.Tip = origTip
		reasons, err = pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		assert.Equal(txpoolcfg.ReplaceUnderpriced, reasons[0], reasons[0].String())

		// Bump fee cap + blobFee cap
		blobTxn.BlobFeeCap.MulDivOverflow(blobFeeCap, uint256.NewInt(requiredPriceBump+100), uint256.NewInt(100))
		reasons, err = pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		assert.Equal(txpoolcfg.NotReplaced, reasons[0], reasons[0].String())

		// Bump only blobFee cap
		blobTxn.FeeCap = origFee
		reasons, err = pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		assert.Equal(txpoolcfg.NotReplaced, reasons[0], reasons[0].String())

		// Bump all prices
		blobTxn.Tip.MulDivOverflow(tip, uint256.NewInt(requiredPriceBump+100), uint256.NewInt(100))
		blobTxn.FeeCap.MulDivOverflow(feeCap, uint256.NewInt(requiredPriceBump+100), uint256.NewInt(100))
		reasons, err = pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		assert.Equal(txpoolcfg.Success, reasons[0], reasons[0].String())
	}
}

// Todo, make the txn more realistic with good values
func makeBlobTx() types.TxSlot {

	bodyRlp := hexutility.MustDecodeHex(BodyRlpHex)

	blobsRlpPrefix := hexutility.MustDecodeHex("fa040008")
	blobRlpPrefix := hexutility.MustDecodeHex("ba020000")

	var blob0, blob1 = gokzg4844.Blob{}, gokzg4844.Blob{}
	copy(blob0[:], hexutility.MustDecodeHex(ValidBlob1Hex))
	copy(blob1[:], hexutility.MustDecodeHex(ValidBlob2Hex))

	var err error
	proofsRlpPrefix := hexutility.MustDecodeHex("f862")
	commitment0, _ := kzg.Ctx().BlobToKZGCommitment(blob0, 0)
	commitment1, _ := kzg.Ctx().BlobToKZGCommitment(blob1, 0)

	proof0, err := kzg.Ctx().ComputeBlobKZGProof(blob0, commitment0, 0)
	if err != nil {
		fmt.Println("error", err)
	}
	proof1, err := kzg.Ctx().ComputeBlobKZGProof(blob1, commitment1, 0)
	if err != nil {
		fmt.Println("error", err)
	}

	wrapperRlp := hexutility.MustDecodeHex("03fa0401fe")
	wrapperRlp = append(wrapperRlp, bodyRlp...)
	wrapperRlp = append(wrapperRlp, blobsRlpPrefix...)
	wrapperRlp = append(wrapperRlp, blobRlpPrefix...)
	wrapperRlp = append(wrapperRlp, blob0[:]...)
	wrapperRlp = append(wrapperRlp, blobRlpPrefix...)
	wrapperRlp = append(wrapperRlp, blob1[:]...)
	wrapperRlp = append(wrapperRlp, proofsRlpPrefix...)
	wrapperRlp = append(wrapperRlp, 0xb0)
	wrapperRlp = append(wrapperRlp, commitment0[:]...)
	wrapperRlp = append(wrapperRlp, 0xb0)
	wrapperRlp = append(wrapperRlp, commitment1[:]...)
	wrapperRlp = append(wrapperRlp, proofsRlpPrefix...)
	wrapperRlp = append(wrapperRlp, 0xb0)
	wrapperRlp = append(wrapperRlp, proof0[:]...)
	wrapperRlp = append(wrapperRlp, 0xb0)
	wrapperRlp = append(wrapperRlp, proof1[:]...)

	tip, feeCap, blobFeeCap := uint256.NewInt(100_000), uint256.NewInt(200_000), uint256.NewInt(200_000)

	blobTx := types.TxSlot{}
	tctx := types.NewTxParseContext(*uint256.NewInt(5))
	tctx.WithSender(false)
	tctx.ParseTransaction(wrapperRlp, 0, &blobTx, nil, false, true, nil)
	blobTx.BlobHashes = make([]common.Hash, 2)
	blobTx.BlobHashes[0] = common.Hash(kzg.KZGToVersionedHash(commitment0))
	blobTx.BlobHashes[1] = common.Hash(kzg.KZGToVersionedHash(commitment1))

	blobTx.Tip = *tip
	blobTx.FeeCap = *feeCap
	blobTx.BlobFeeCap = *blobFeeCap
	return blobTx
}

func TestDropRemoteAtNoGossip(t *testing.T) {
	t.Skip("TODO")
	assert, require := assert.New(t), require.New(t)
	ch := make(chan types.Announcements, 100)
	coreDB, _ := temporaltest.NewTestDB(t, datadir.New(t.TempDir()))
	db := memdb.NewTestPoolDB(t)

	cfg := txpoolcfg.DefaultConfig
	cfg.NoGossip = true

	logger := log.New()
	sendersCache := kvcache.New(kvcache.DefaultCoherentConfig)

	txPool, err := New(ch, coreDB, cfg, sendersCache, *u256.N1, big.NewInt(0), big.NewInt(0), nil, fixedgas.DefaultMaxBlobsPerBlock, nil, logger)
	assert.NoError(err)
	require.True(txPool != nil)

	ctx := context.Background()

	err = txPool.Start(ctx, db)
	assert.NoError(err)

	var stateVersionID uint64 = 0
	pendingBaseFee := uint64(1_000_000)
	// start blocks from 0, set empty hash - then kvcache will also work on this
	h1 := gointerfaces.ConvertHashToH256([32]byte{})
	change := &remote.StateChangeBatch{
		StateVersionId:      stateVersionID,
		PendingBlockBaseFee: pendingBaseFee,
		BlockGasLimit:       1000000,
		ChangeBatch: []*remote.StateChange{
			{BlockHeight: 0, BlockHash: h1},
		},
	}
	var addr [20]byte
	addr[0] = 1
	v := make([]byte, types.EncodeSenderLengthForStorage(2, *uint256.NewInt(1 * common.Ether)))
	types.EncodeSender(2, *uint256.NewInt(1 * common.Ether), v)
	change.ChangeBatch[0].Changes = append(change.ChangeBatch[0].Changes, &remote.AccountChange{
		Action:  remote.Action_UPSERT,
		Address: gointerfaces.ConvertAddressToH160(addr),
		Data:    v,
	})
	tx, err := db.BeginRw(ctx)
	require.NoError(err)
	defer tx.Rollback()
	err = txPool.OnNewBlock(ctx, change, types.TxSlots{}, types.TxSlots{}, types.TxSlots{}, tx)
	assert.NoError(err)
	// 1. Try Local Tx
	{
		var txSlots types.TxSlots
		txSlot := &types.TxSlot{
			Tip:    *uint256.NewInt(500_000),
			FeeCap: *uint256.NewInt(3_000_000),
			Gas:    100000,
			Nonce:  3,
		}
		txSlot.IDHash[0] = 1
		txSlots.Append(txSlot, addr[:], true)

		reasons, err := txPool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		for _, reason := range reasons {
			assert.Equal(txpoolcfg.Success, reason, reason.String())
		}
	}
	select {
	case annoucements := <-ch:
		for i := 0; i < annoucements.Len(); i++ {
			_, _, hash := annoucements.At(i)
			fmt.Printf("propagated hash %x\n", hash)
		}
	default:

	}

	// 2. Try same Tx, but as remote; txn must be dropped
	{
		var txSlots types.TxSlots
		txSlot := &types.TxSlot{
			Tip:    *uint256.NewInt(500_000),
			FeeCap: *uint256.NewInt(3_000_000),
			Gas:    100000,
			Nonce:  3,
		}
		txSlot.IDHash[0] = 1
		txSlots.Append(txSlot, addr[:], true)

		txPool.AddRemoteTxs(ctx, txSlots)
	}

	// empty because AddRemoteTxs logic is intentionally empty
	assert.Equal(0, len(txPool.unprocessedRemoteByHash))
	assert.Equal(0, len(txPool.unprocessedRemoteTxs.Txs))

	assert.NoError(txPool.processRemoteTxs(ctx))

	checkAnnouncementEmpty := func() bool {
		select {
		case <-ch:
			return false
		default:
			return true
		}
	}
	// no announcement because unprocessedRemoteTxs is already empty
	assert.True(checkAnnouncementEmpty())
}

func TestBlobSlots(t *testing.T) {
	t.Skip("TODO")
	assert, require := assert.New(t), require.New(t)
	ch := make(chan types.Announcements, 5)
	coreDB, _ := temporaltest.NewTestDB(t, datadir.New(t.TempDir()))
	db := memdb.NewTestPoolDB(t)
	cfg := txpoolcfg.DefaultConfig

	//Setting limits for blobs in the pool
	cfg.TotalBlobPoolLimit = 20

	sendersCache := kvcache.New(kvcache.DefaultCoherentConfig)
	pool, err := New(ch, coreDB, cfg, sendersCache, *u256.N1, common.Big0, nil, common.Big0, fixedgas.DefaultMaxBlobsPerBlock, nil, log.New())
	assert.NoError(err)
	require.True(pool != nil)
	ctx := context.Background()
	var stateVersionID uint64 = 0

	h1 := gointerfaces.ConvertHashToH256([32]byte{})
	change := &remote.StateChangeBatch{
		StateVersionId:       stateVersionID,
		PendingBlockBaseFee:  200_000,
		BlockGasLimit:        1000000,
		PendingBlobFeePerGas: 100_000,
		ChangeBatch: []*remote.StateChange{
			{BlockHeight: 0, BlockHash: h1},
		},
	}
	var addr [20]byte

	// Add 1 eth to the user account, as a part of change
	v := make([]byte, types.EncodeSenderLengthForStorage(0, *uint256.NewInt(1 * common.Ether)))
	types.EncodeSender(0, *uint256.NewInt(1 * common.Ether), v)

	for i := 0; i < 11; i++ {
		addr[0] = uint8(i + 1)
		change.ChangeBatch[0].Changes = append(change.ChangeBatch[0].Changes, &remote.AccountChange{
			Action:  remote.Action_UPSERT,
			Address: gointerfaces.ConvertAddressToH160(addr),
			Data:    v,
		})
	}

	tx, err := db.BeginRw(ctx)
	require.NoError(err)
	defer tx.Rollback()
	err = pool.OnNewBlock(ctx, change, types.TxSlots{}, types.TxSlots{}, types.TxSlots{}, tx)
	assert.NoError(err)

	//Adding 20 blobs from 10 different accounts
	for i := 0; i < int(cfg.TotalBlobPoolLimit/2); i++ {
		txSlots := types.TxSlots{}
		addr[0] = uint8(i + 1)
		blobTxn := makeBlobTx() // makes a txn with 2 blobs
		blobTxn.IDHash[0] = uint8(2*i + 1)
		blobTxn.Nonce = 0
		txSlots.Append(&blobTxn, addr[:], true)
		reasons, err := pool.AddLocalTxs(ctx, txSlots, tx)
		assert.NoError(err)
		for _, reason := range reasons {
			assert.Equal(txpoolcfg.Success, reason, reason.String())
		}
	}

	// Adding another blob txn should reject
	txSlots := types.TxSlots{}
	addr[0] = 11
	blobTxn := makeBlobTx()
	blobTxn.IDHash[0] = uint8(21)
	blobTxn.Nonce = 0

	txSlots.Append(&blobTxn, addr[:], true)
	reasons, err := pool.AddLocalTxs(ctx, txSlots, tx)
	assert.NoError(err)
	for _, reason := range reasons {
		assert.Equal(txpoolcfg.BlobPoolOverflow, reason, reason.String())
	}
}

func TestGasLimitChanged(t *testing.T) {
	t.Skip("TODO")
	assert, require := assert.New(t), require.New(t)
	ch := make(chan types.Announcements, 100)

	coreDB, _ := temporaltest.NewTestDB(t, datadir.New(t.TempDir()))
	db := memdb.NewTestPoolDB(t)
	cfg := txpoolcfg.DefaultConfig
	sendersCache := kvcache.New(kvcache.DefaultCoherentConfig)
	pool, err := New(ch, coreDB, cfg, sendersCache, *u256.N1, nil, nil, nil, fixedgas.DefaultMaxBlobsPerBlock, nil, log.New())
	assert.NoError(err)
	require.True(pool != nil)
	ctx := context.Background()
	var stateVersionID uint64 = 0
	pendingBaseFee := uint64(200000)
	// start blocks from 0, set empty hash - then kvcache will also work on this
	h1 := gointerfaces.ConvertHashToH256([32]byte{})
	var addr [20]byte
	addr[0] = 1
	v := make([]byte, types.EncodeSenderLengthForStorage(2, *uint256.NewInt(1 * common.Ether)))
	types.EncodeSender(2, *uint256.NewInt(1 * common.Ether), v)
	tx, err := db.BeginRw(ctx)
	require.NoError(err)
	defer tx.Rollback()

	change := &remote.StateChangeBatch{
		StateVersionId:      stateVersionID,
		PendingBlockBaseFee: pendingBaseFee,
		BlockGasLimit:       50_000,
		ChangeBatch: []*remote.StateChange{
			{BlockHeight: 0, BlockHash: h1},
		},
	}
	change.ChangeBatch[0].Changes = append(change.ChangeBatch[0].Changes, &remote.AccountChange{
		Action:  remote.Action_UPSERT,
		Address: gointerfaces.ConvertAddressToH160(addr),
		Data:    v,
	})
	err = pool.OnNewBlock(ctx, change, types.TxSlots{}, types.TxSlots{}, types.TxSlots{}, tx)
	assert.NoError(err)

	var txSlots types.TxSlots
	txSlot1 := &types.TxSlot{
		Tip:    *uint256.NewInt(300000),
		FeeCap: *uint256.NewInt(300000),
		Gas:    100_000,
		Nonce:  3,
	}
	txSlot1.IDHash[0] = 1
	txSlots.Append(txSlot1, addr[:], true)

	reasons, err := pool.AddLocalTxs(ctx, txSlots, tx)
	assert.NoError(err)
	for _, reason := range reasons {
		assert.Equal(txpoolcfg.Success, reason, reason.String())
	}

	mtx, ok := pool.byHash[string(txSlot1.IDHash[:])]
	assert.True(ok)
	assert.Zero(mtx.subPool&NotTooMuchGas, "Should be insufficient block space for the tx")

	change.ChangeBatch[0].Changes = nil
	change.BlockGasLimit = 150_000
	err = pool.OnNewBlock(ctx, change, types.TxSlots{}, types.TxSlots{}, types.TxSlots{}, tx)
	assert.NoError(err)

	assert.NotZero(mtx.subPool&NotTooMuchGas, "Should now have block space for the tx")

	change.BlockGasLimit = 50_000
	err = pool.OnNewBlock(ctx, change, types.TxSlots{}, types.TxSlots{}, types.TxSlots{}, tx)
	assert.NoError(err)

	assert.Zero(mtx.subPool&NotTooMuchGas, "Should now have block space (again) for the tx")
}
