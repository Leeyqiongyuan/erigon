package freezeblocks

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/holiman/uint256"
	"github.com/tidwall/btree"
	"golang.org/x/exp/slices"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"

	"github.com/ledgerwatch/erigon-lib/log/v3"

	"github.com/ledgerwatch/erigon-lib/chain"
	"github.com/ledgerwatch/erigon-lib/chain/snapcfg"
	common2 "github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/background"
	"github.com/ledgerwatch/erigon-lib/common/cmp"
	"github.com/ledgerwatch/erigon-lib/common/datadir"
	"github.com/ledgerwatch/erigon-lib/common/dbg"
	dir2 "github.com/ledgerwatch/erigon-lib/common/dir"
	"github.com/ledgerwatch/erigon-lib/common/hexutility"
	"github.com/ledgerwatch/erigon-lib/diagnostics"
	"github.com/ledgerwatch/erigon-lib/downloader/snaptype"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/recsplit"
	"github.com/ledgerwatch/erigon-lib/seg"
	types2 "github.com/ledgerwatch/erigon-lib/types"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/core/rawdb/blockio"
	coresnaptype "github.com/ledgerwatch/erigon/core/snaptype"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/eth/ethconfig"
	"github.com/ledgerwatch/erigon/eth/ethconfig/estimate"
	"github.com/ledgerwatch/erigon/eth/stagedsync/stages"
	"github.com/ledgerwatch/erigon/polygon/heimdall"
	"github.com/ledgerwatch/erigon/rlp"
	"github.com/ledgerwatch/erigon/turbo/services"
	"github.com/ledgerwatch/erigon/turbo/silkworm"
)

type Range struct {
	from, to uint64
}

func (r Range) From() uint64 { return r.from }
func (r Range) To() uint64   { return r.to }

type Ranges []Range

func (r Ranges) String() string {
	return fmt.Sprintf("%d", r)
}

type Segment struct {
	Range
	*seg.Decompressor
	indexes []*recsplit.Index
	segType snaptype.Type
	version snaptype.Version
}

func (s Segment) Type() snaptype.Type {
	return s.segType
}

func (s Segment) Version() snaptype.Version {
	return s.version
}

func (s Segment) Index(index ...snaptype.Index) *recsplit.Index {
	if len(index) == 0 {
		index = []snaptype.Index{{}}
	}

	if len(s.indexes) <= index[0].Offset {
		return nil
	}

	return s.indexes[index[0].Offset]
}

func (s Segment) IsIndexed() bool {
	if len(s.indexes) < len(s.Type().Indexes()) {
		return false
	}

	for _, i := range s.indexes {
		if i == nil {
			return false
		}
	}

	return true
}

func (s Segment) FileName() string {
	return s.Type().FileName(s.version, s.from, s.to)
}

func (s Segment) FileInfo(dir string) snaptype.FileInfo {
	return s.Type().FileInfo(dir, s.from, s.to)
}

func (s *Segment) reopenSeg(dir string) (err error) {
	s.closeSeg()
	s.Decompressor, err = seg.NewDecompressor(filepath.Join(dir, s.FileName()))
	if err != nil {
		return fmt.Errorf("%w, fileName: %s", err, s.FileName())
	}
	return nil
}

func (s *Segment) closeSeg() {
	if s.Decompressor != nil {
		s.Close()
		s.Decompressor = nil
	}
}

func (s *Segment) closeIdx() {
	for _, index := range s.indexes {
		index.Close()
	}

	s.indexes = nil
}

func (s *Segment) close() {
	if s != nil {
		s.closeSeg()
		s.closeIdx()
	}
}

func (s *Segment) openFiles() []string {
	files := make([]string, 0, len(s.indexes)+1)

	if s.IsOpen() {
		files = append(files, s.FilePath())
	}

	for _, index := range s.indexes {
		files = append(files, index.FilePath())
	}

	return files
}

func (s *Segment) reopenIdxIfNeed(dir string, optimistic bool) (err error) {
	if len(s.Type().IdxFileNames(s.version, s.from, s.to)) == 0 {
		return nil
	}

	err = s.reopenIdx(dir)

	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			if optimistic {
				log.Warn("[snapshots] open index", "err", err)
			} else {
				return err
			}
		}
	}

	return nil
}

func (s *Segment) reopenIdx(dir string) (err error) {
	s.closeIdx()
	if s.Decompressor == nil {
		return nil
	}

	for _, fileName := range s.Type().IdxFileNames(s.version, s.from, s.to) {
		index, err := recsplit.OpenIndex(filepath.Join(dir, fileName))

		if err != nil {
			return fmt.Errorf("%w, fileName: %s", err, fileName)
		}

		s.indexes = append(s.indexes, index)
	}

	return nil
}

func (sn *Segment) mappedHeaderSnapshot() *silkworm.MappedHeaderSnapshot {
	segmentRegion := silkworm.NewMemoryMappedRegion(sn.FilePath(), sn.DataHandle(), sn.Size())
	idxRegion := silkworm.NewMemoryMappedRegion(sn.Index().FilePath(), sn.Index().DataHandle(), sn.Index().Size())
	return silkworm.NewMappedHeaderSnapshot(segmentRegion, idxRegion)
}

func (sn *Segment) mappedBodySnapshot() *silkworm.MappedBodySnapshot {
	segmentRegion := silkworm.NewMemoryMappedRegion(sn.FilePath(), sn.DataHandle(), sn.Size())
	idxRegion := silkworm.NewMemoryMappedRegion(sn.Index().FilePath(), sn.Index().DataHandle(), sn.Index().Size())
	return silkworm.NewMappedBodySnapshot(segmentRegion, idxRegion)
}

func (sn *Segment) mappedTxnSnapshot() *silkworm.MappedTxnSnapshot {
	segmentRegion := silkworm.NewMemoryMappedRegion(sn.FilePath(), sn.DataHandle(), sn.Size())
	idxTxnHash := sn.Index(coresnaptype.Indexes.TxnHash)
	idxTxnHashRegion := silkworm.NewMemoryMappedRegion(idxTxnHash.FilePath(), idxTxnHash.DataHandle(), idxTxnHash.Size())
	idxTxnHash2BlockNum := sn.Index(coresnaptype.Indexes.TxnHash2BlockNum)
	idxTxnHash2BlockRegion := silkworm.NewMemoryMappedRegion(idxTxnHash2BlockNum.FilePath(), idxTxnHash2BlockNum.DataHandle(), idxTxnHash2BlockNum.Size())
	return silkworm.NewMappedTxnSnapshot(segmentRegion, idxTxnHashRegion, idxTxnHash2BlockRegion)
}

// headers
// value: first_byte_of_header_hash + header_rlp
// header_hash       -> headers_segment_offset

// bodies
// value: rlp(types.BodyForStorage)
// block_num_u64     -> bodies_segment_offset

// transactions
// value: first_byte_of_transaction_hash + sender_address + transaction_rlp
// transaction_hash  -> transactions_segment_offset
// transaction_hash  -> block_number

type segments struct {
	lock     sync.RWMutex
	segments []*Segment
}

func (s *segments) View(f func(segments []*Segment) error) error {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return f(s.segments)
}

func (s *segments) Segment(blockNum uint64, f func(*Segment) error) (found bool, err error) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	for _, seg := range s.segments {
		if !(blockNum >= seg.from && blockNum < seg.to) {
			continue
		}
		return true, f(seg)
	}
	return false, nil
}

type RoSnapshots struct {
	indicesReady  atomic.Bool
	segmentsReady atomic.Bool

	types    []snaptype.Type
	segments btree.Map[snaptype.Enum, *segments]

	dir         string
	segmentsMax atomic.Uint64 // all types of .seg files are available - up to this number
	idxMax      atomic.Uint64 // all types of .idx files are available - up to this number
	cfg         ethconfig.BlocksFreezing
	logger      log.Logger

	// allows for pruning segments - this is the min availible segment
	segmentsMin atomic.Uint64
}

// NewRoSnapshots - opens all snapshots. But to simplify everything:
//   - it opens snapshots only on App start and immutable after
//   - all snapshots of given blocks range must exist - to make this blocks range available
//   - gaps are not allowed
//   - segment have [from:to) semantic
func NewRoSnapshots(cfg ethconfig.BlocksFreezing, snapDir string, segmentsMin uint64, logger log.Logger) *RoSnapshots {
	return newRoSnapshots(cfg, snapDir, coresnaptype.BlockSnapshotTypes, segmentsMin, logger)
}

func newRoSnapshots(cfg ethconfig.BlocksFreezing, snapDir string, types []snaptype.Type, segmentsMin uint64, logger log.Logger) *RoSnapshots {
	var segs btree.Map[snaptype.Enum, *segments]
	for _, snapType := range types {
		segs.Set(snapType.Enum(), &segments{})
	}

	s := &RoSnapshots{dir: snapDir, cfg: cfg, segments: segs, logger: logger, types: types}
	s.segmentsMin.Store(segmentsMin)

	return s
}

func (s *RoSnapshots) Cfg() ethconfig.BlocksFreezing { return s.cfg }
func (s *RoSnapshots) Dir() string                   { return s.dir }
func (s *RoSnapshots) SegmentsReady() bool           { return s.segmentsReady.Load() }
func (s *RoSnapshots) IndicesReady() bool            { return s.indicesReady.Load() }
func (s *RoSnapshots) IndicesMax() uint64            { return s.idxMax.Load() }
func (s *RoSnapshots) SegmentsMax() uint64           { return s.segmentsMax.Load() }
func (s *RoSnapshots) SegmentsMin() uint64           { return s.segmentsMin.Load() }
func (s *RoSnapshots) SetSegmentsMin(min uint64)     { s.segmentsMin.Store(min) }
func (s *RoSnapshots) BlocksAvailable() uint64 {
	if s == nil {
		return 0
	}

	return s.idxMax.Load()
}
func (s *RoSnapshots) LogStat(label string) {
	var m runtime.MemStats
	dbg.ReadMemStats(&m)
	s.logger.Info(fmt.Sprintf("[snapshots:%s] Stat", label),
		"blocks", fmt.Sprintf("%dk", (s.SegmentsMax()+1)/1000),
		"indices", fmt.Sprintf("%dk", (s.IndicesMax()+1)/1000),
		"alloc", common2.ByteCount(m.Alloc), "sys", common2.ByteCount(m.Sys))
}

func (s *RoSnapshots) EnsureExpectedBlocksAreAvailable(cfg *snapcfg.Cfg) error {
	if s.BlocksAvailable() < cfg.ExpectBlocks {
		return fmt.Errorf("app must wait until all expected snapshots are available. Expected: %d, Available: %d", cfg.ExpectBlocks, s.BlocksAvailable())
	}
	return nil
}

func (s *RoSnapshots) Types() []snaptype.Type { return s.types }
func (s *RoSnapshots) HasType(in snaptype.Type) bool {
	for _, t := range s.types {
		if t.Enum() == in.Enum() {
			return true
		}
	}
	return false
}

// DisableReadAhead - usage: `defer d.EnableReadAhead().DisableReadAhead()`. Please don't use this funcs without `defer` to avoid leak.
func (s *RoSnapshots) DisableReadAhead() *RoSnapshots {
	s.segments.Scan(func(segtype snaptype.Enum, value *segments) bool {
		value.lock.RLock()
		defer value.lock.RUnlock()
		for _, sn := range value.segments {
			sn.DisableReadAhead()
		}
		return true
	})

	return s
}

func (s *RoSnapshots) EnableReadAhead() *RoSnapshots {
	s.segments.Scan(func(segtype snaptype.Enum, value *segments) bool {
		value.lock.RLock()
		defer value.lock.RUnlock()
		for _, sn := range value.segments {
			sn.EnableReadAhead()
		}
		return true
	})

	return s
}

func (s *RoSnapshots) EnableMadvWillNeed() *RoSnapshots {
	s.segments.Scan(func(segtype snaptype.Enum, value *segments) bool {
		value.lock.RLock()
		defer value.lock.RUnlock()
		for _, sn := range value.segments {
			sn.EnableMadvWillNeed()
		}
		return true
	})
	return s
}

// minimax of existing indices
func (s *RoSnapshots) idxAvailability() uint64 {
	// Use-Cases:
	//   1. developers can add new types in future. and users will not have files of this type
	//   2. some types are network-specific. example: borevents exists only on Bor-consensus networks
	//   3. user can manually remove 1 .idx file: `rm snapshots/v1-type1-0000-1000.idx`
	//   4. user can manually remove all .idx files of given type: `rm snapshots/*type1*.idx`
	//   5. file-types may have different height: 10 headers, 10 bodies, 9 trancasctions (for example if `kill -9` came during files building/merge). still need index all 3 types.
	amount := 0
	s.segments.Scan(func(segtype snaptype.Enum, value *segments) bool {
		if len(value.segments) == 0 || !s.HasType(segtype.Type()) {
			return true
		}
		amount++
		return true
	})

	maximums := make([]uint64, amount)
	var i int
	s.segments.Scan(func(segtype snaptype.Enum, value *segments) bool {
		if len(value.segments) == 0 || !s.HasType(segtype.Type()) {
			return true
		}

		for _, seg := range value.segments {
			if !seg.IsIndexed() {
				break
			}

			maximums[i] = seg.to - 1
		}

		i++
		return true
	})

	if len(maximums) == 0 {
		return 0
	}
	return slices.Min(maximums)
}

// OptimisticReopenWithDB - optimistically open snapshots (ignoring error), useful at App startup because:
// - user must be able: delete any snapshot file and Erigon will self-heal by re-downloading
// - RPC return Nil for historical blocks if snapshots are not open
func (s *RoSnapshots) OptimisticReopenWithDB(db kv.RoDB) {
	_ = db.View(context.Background(), func(tx kv.Tx) error {
		snList, _, err := rawdb.ReadSnapshots(tx)
		if err != nil {
			return err
		}
		return s.ReopenList(snList, true)
	})
}

func (s *RoSnapshots) Files() (list []string) {
	maxBlockNumInFiles := s.BlocksAvailable()

	s.segments.Scan(func(segtype snaptype.Enum, value *segments) bool {
		value.lock.RLock()
		defer value.lock.RUnlock()

		for _, seg := range value.segments {
			if seg.Decompressor == nil {
				continue
			}
			if seg.from > maxBlockNumInFiles {
				continue
			}
			list = append(list, seg.FileName())
		}
		return true
	})

	slices.Sort(list)
	return list
}

func (s *RoSnapshots) OpenFiles() (list []string) {
	s.segments.Scan(func(segtype snaptype.Enum, value *segments) bool {
		value.lock.RLock()
		defer value.lock.RUnlock()

		for _, seg := range value.segments {
			list = append(list, seg.openFiles()...)
		}
		return true
	})

	return list
}

// ReopenList stops on optimistic=false, continue opening files on optimistic=true
func (s *RoSnapshots) ReopenList(fileNames []string, optimistic bool) error {
	if err := s.rebuildSegments(fileNames, true, optimistic); err != nil {
		return err
	}
	return nil
}

func (s *RoSnapshots) InitSegments(fileNames []string) error {
	if err := s.rebuildSegments(fileNames, false, true); err != nil {
		return err
	}
	return nil
}

func (s *RoSnapshots) lockSegments() {
	s.segments.Scan(func(segtype snaptype.Enum, value *segments) bool {
		value.lock.Lock()
		return true
	})
}

func (s *RoSnapshots) unlockSegments() {
	s.segments.Scan(func(segtype snaptype.Enum, value *segments) bool {
		value.lock.Unlock()
		return true
	})
}

func (s *RoSnapshots) rebuildSegments(fileNames []string, open bool, optimistic bool) error {
	s.lockSegments()
	defer s.unlockSegments()

	s.closeWhatNotInList(fileNames)
	var segmentsMax uint64
	var segmentsMaxSet bool

	for _, fName := range fileNames {
		f, isState, ok := snaptype.ParseFileName(s.dir, fName)
		if !ok || isState {
			continue
		}
		if !s.HasType(f.Type) {
			continue
		}

		segtype, ok := s.segments.Get(f.Type.Enum())
		if !ok {
			segtype = &segments{}
			s.segments.Set(f.Type.Enum(), segtype)
			segtype.lock.Lock() // this will be unlocked by defer s.unlockSegments() above
		}

		var sn *Segment
		var exists bool
		for _, sn2 := range segtype.segments {
			if sn2.Decompressor == nil { // it's ok if some segment was not able to open
				continue
			}

			if fName == sn2.FileName() {
				sn = sn2
				exists = true
				break
			}
		}

		if !exists {
			sn = &Segment{segType: f.Type, version: f.Version, Range: Range{f.From, f.To}}
		}

		if open {
			if err := sn.reopenSeg(s.dir); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					if optimistic {
						continue
					} else {
						break
					}
				}
				if optimistic {
					continue
				} else {
					return err
				}
			}
		}

		if !exists {
			// it's possible to iterate over .seg file even if you don't have index
			// then make segment available even if index open may fail
			segtype.segments = append(segtype.segments, sn)
		}

		if open {
			if err := sn.reopenIdxIfNeed(s.dir, optimistic); err != nil {
				return err
			}
		}

		if f.To > 0 {
			segmentsMax = f.To - 1
		} else {
			segmentsMax = 0
		}
		segmentsMaxSet = true
	}

	if segmentsMaxSet {
		s.segmentsMax.Store(segmentsMax)
	}
	s.segmentsReady.Store(true)
	s.idxMax.Store(s.idxAvailability())
	s.indicesReady.Store(true)

	return nil
}

func (s *RoSnapshots) Ranges() []Range {
	view := s.View()
	defer view.Close()
	return view.Ranges()
}

func (s *RoSnapshots) OptimisticalyReopenFolder()           { _ = s.ReopenFolder() }
func (s *RoSnapshots) OptimisticalyReopenWithDB(db kv.RoDB) { _ = s.ReopenWithDB(db) }
func (s *RoSnapshots) ReopenFolder() error {
	if err := s.ReopenSegments(s.Types(), false); err != nil {
		return fmt.Errorf("ReopenSegments: %w", err)
	}
	return nil
}

func (s *RoSnapshots) ReopenSegments(types []snaptype.Type, allowGaps bool) error {
	files, _, err := typedSegments(s.dir, s.segmentsMin.Load(), types, allowGaps)

	if err != nil {
		return err
	}
	list := make([]string, 0, len(files))
	for _, f := range files {
		_, fName := filepath.Split(f.Path)
		list = append(list, fName)
	}
	return s.ReopenList(list, false)
}

func (s *RoSnapshots) ReopenWithDB(db kv.RoDB) error {
	if err := db.View(context.Background(), func(tx kv.Tx) error {
		snList, _, err := rawdb.ReadSnapshots(tx)
		if err != nil {
			return err
		}
		return s.ReopenList(snList, true)
	}); err != nil {
		return fmt.Errorf("ReopenWithDB: %w", err)
	}
	return nil
}

func (s *RoSnapshots) Close() {
	if s == nil {
		return
	}
	s.lockSegments()
	defer s.unlockSegments()
	s.closeWhatNotInList(nil)
}

func (s *RoSnapshots) closeWhatNotInList(l []string) {
	s.segments.Scan(func(segtype snaptype.Enum, value *segments) bool {
	Segments:
		for i, sn := range value.segments {
			if sn.Decompressor == nil {
				continue Segments
			}
			_, name := filepath.Split(sn.FilePath())
			for _, fName := range l {
				if fName == name {
					continue Segments
				}
			}
			sn.close()
			value.segments[i] = nil
		}
		return true
	})

	s.segments.Scan(func(segtype snaptype.Enum, value *segments) bool {
		var i int
		for i = 0; i < len(value.segments) && value.segments[i] != nil && value.segments[i].Decompressor != nil; i++ {
		}
		tail := value.segments[i:]
		value.segments = value.segments[:i]
		for i = 0; i < len(tail); i++ {
			if tail[i] != nil {
				tail[i].close()
				tail[i] = nil
			}
		}
		return true
	})
}

func (s *RoSnapshots) removeOverlapsAfterMerge() error {
	s.lockSegments()
	defer s.unlockSegments()

	list, err := snaptype.Segments(s.dir)

	if err != nil {
		return err
	}

	if _, toRemove := findOverlaps(list); len(toRemove) > 0 {
		filesToRemove := make([]string, 0, len(toRemove))

		for _, info := range toRemove {
			filesToRemove = append(filesToRemove, info.Path)
		}

		removeOldFiles(filesToRemove, s.dir)
	}

	return nil
}

func (s *RoSnapshots) buildMissedIndicesIfNeed(ctx context.Context, logPrefix string, notifier services.DBEventNotifier, dirs datadir.Dirs, cc *chain.Config, logger log.Logger) error {
	if s.IndicesMax() >= s.SegmentsMax() {
		return nil
	}
	if !s.Cfg().ProduceE2 && s.IndicesMax() == 0 {
		return fmt.Errorf("please remove --snap.stop, erigon can't work without creating basic indices")
	}
	if !s.Cfg().ProduceE2 {
		return nil
	}
	if !s.SegmentsReady() {
		return fmt.Errorf("not all snapshot segments are available")
	}
	s.LogStat("missed-idx")

	// wait for Downloader service to download all expected snapshots
	indexWorkers := estimate.IndexSnapshot.Workers()
	if err := s.buildMissedIndices(logPrefix, ctx, dirs, cc, indexWorkers, logger); err != nil {
		return fmt.Errorf("can't build missed indices: %w", err)
	}

	if err := s.ReopenFolder(); err != nil {
		return err
	}
	s.LogStat("missed-idx:reopen")
	if notifier != nil {
		notifier.OnNewSnapshot()
	}
	return nil
}

func (s *RoSnapshots) delete(fileName string) error {
	v := s.View()
	defer v.Close()

	_, fName := filepath.Split(fileName)
	var err error
	s.segments.Scan(func(segtype snaptype.Enum, value *segments) bool {
		idxsToRemove := []int{}
		for i, sn := range value.segments {
			if sn.Decompressor == nil {
				continue
			}
			if sn.segType.FileName(sn.version, sn.from, sn.to) != fName {
				continue
			}
			files := sn.openFiles()
			sn.close()
			idxsToRemove = append(idxsToRemove, i)
			for _, f := range files {
				_ = os.Remove(f)
			}
		}
		for i := len(idxsToRemove) - 1; i >= 0; i-- {
			value.segments = append(value.segments[:idxsToRemove[i]], value.segments[idxsToRemove[i]+1:]...)
		}
		return true
	})
	return err
}

func (s *RoSnapshots) Delete(fileName string) error {
	if s == nil {
		return nil
	}
	if err := s.delete(fileName); err != nil {
		return fmt.Errorf("can't delete file: %w", err)
	}
	return s.ReopenFolder()

}

func (s *RoSnapshots) buildMissedIndices(logPrefix string, ctx context.Context, dirs datadir.Dirs, chainConfig *chain.Config, workers int, logger log.Logger) error {
	if s == nil {
		return nil
	}

	dir, tmpDir := dirs.Snap, dirs.Tmp
	//log.Log(lvl, "[snapshots] Build indices", "from", min)

	ps := background.NewProgressSet()
	startIndexingTime := time.Now()

	logEvery := time.NewTicker(20 * time.Second)
	defer logEvery.Stop()

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(workers)
	finish := make(chan struct{})

	go func() {
		for {
			select {
			case <-logEvery.C:
				var m runtime.MemStats
				dbg.ReadMemStats(&m)
				sendDiagnostics(startIndexingTime, ps.DiagnossticsData(), m.Alloc, m.Sys)
				logger.Info(fmt.Sprintf("[%s] Indexing", logPrefix), "progress", ps.String(), "total-indexing-time", time.Since(startIndexingTime).Round(time.Second).String(), "alloc", common2.ByteCount(m.Alloc), "sys", common2.ByteCount(m.Sys))
			case <-finish:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	var fmu sync.Mutex
	failedIndexes := make(map[string]error, 0)

	s.segments.Scan(func(segtype snaptype.Enum, value *segments) bool {
		for _, segment := range value.segments {
			info := segment.FileInfo(dir)

			if segtype.HasIndexFiles(info, logger) {
				continue
			}

			segment.closeIdx()

			g.Go(func() error {
				p := &background.Progress{}
				ps.Add(p)
				defer notifySegmentIndexingFinished(info.Name())
				defer ps.Delete(p)
				if err := segtype.BuildIndexes(gCtx, info, chainConfig, tmpDir, p, log.LvlInfo, logger); err != nil {
					// unsuccessful indexing should allow other indexing to finish
					fmu.Lock()
					failedIndexes[info.Name()] = err
					fmu.Unlock()
				}
				return nil
			})
		}

		return true
	})

	var ie error

	go func() {
		defer close(finish)
		g.Wait()

		fmu.Lock()
		for fname, err := range failedIndexes {
			logger.Error(fmt.Sprintf("[%s] Indexing failed", logPrefix), "file", fname, "error", err)
			ie = fmt.Errorf("%s: %w", fname, err) // report the last one anyway
		}
		fmu.Unlock()
	}()

	// Block main thread
	select {
	case <-finish:
		if err := g.Wait(); err != nil {
			return err
		}
		return ie
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *RoSnapshots) PrintDebug() {
	s.lockSegments()
	defer s.unlockSegments()

	s.segments.Scan(func(key snaptype.Enum, value *segments) bool {
		fmt.Println("    == [dbg] Snapshots,", key.String())
		for _, sn := range value.segments {
			args := make([]any, 0, len(sn.Type().Indexes())+1)
			args = append(args, sn.from)
			for _, index := range sn.Type().Indexes() {
				args = append(args, sn.Index(index) != nil)
			}
			fmt.Println(args...)
		}
		return true
	})
}

func (s *RoSnapshots) AddSnapshotsToSilkworm(silkwormInstance *silkworm.Silkworm) error {
	mappedHeaderSnapshots := make([]*silkworm.MappedHeaderSnapshot, 0)
	if headers, ok := s.segments.Get(coresnaptype.Enums.Headers); ok {
		err := headers.View(func(segments []*Segment) error {
			for _, headerSegment := range segments {
				mappedHeaderSnapshots = append(mappedHeaderSnapshots, headerSegment.mappedHeaderSnapshot())
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	mappedBodySnapshots := make([]*silkworm.MappedBodySnapshot, 0)
	if bodies, ok := s.segments.Get(coresnaptype.Enums.Bodies); ok {
		err := bodies.View(func(segments []*Segment) error {
			for _, bodySegment := range segments {
				mappedBodySnapshots = append(mappedBodySnapshots, bodySegment.mappedBodySnapshot())
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	mappedTxnSnapshots := make([]*silkworm.MappedTxnSnapshot, 0)
	if txs, ok := s.segments.Get(coresnaptype.Enums.Transactions); ok {
		err := txs.View(func(segments []*Segment) error {
			for _, txnSegment := range segments {
				mappedTxnSnapshots = append(mappedTxnSnapshots, txnSegment.mappedTxnSnapshot())
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	if len(mappedHeaderSnapshots) != len(mappedBodySnapshots) || len(mappedBodySnapshots) != len(mappedTxnSnapshots) {
		return fmt.Errorf("addSnapshots: the number of headers/bodies/txs snapshots must be the same")
	}

	for i := 0; i < len(mappedHeaderSnapshots); i++ {
		mappedSnapshot := &silkworm.MappedChainSnapshot{
			Headers: mappedHeaderSnapshots[i],
			Bodies:  mappedBodySnapshots[i],
			Txs:     mappedTxnSnapshots[i],
		}
		err := silkwormInstance.AddSnapshot(mappedSnapshot)
		if err != nil {
			return err
		}
	}

	return nil
}

func buildIdx(ctx context.Context, sn snaptype.FileInfo, chainConfig *chain.Config, tmpDir string, p *background.Progress, lvl log.Lvl, logger log.Logger) error {
	//log.Info("[snapshots] build idx", "file", sn.Name())
	if err := sn.Type.BuildIndexes(ctx, sn, chainConfig, tmpDir, p, lvl, logger); err != nil {
		return fmt.Errorf("buildIdx: %s: %s", sn.Type, err)
	}
	//log.Info("[snapshots] finish build idx", "file", fName)
	return nil
}

func notifySegmentIndexingFinished(name string) {
	diagnostics.Send(
		diagnostics.SnapshotSegmentIndexingFinishedUpdate{
			SegmentName: name,
		},
	)
}

func sendDiagnostics(startIndexingTime time.Time, indexPercent map[string]int, alloc uint64, sys uint64) {
	segmentsStats := make([]diagnostics.SnapshotSegmentIndexingStatistics, 0, len(indexPercent))
	for k, v := range indexPercent {
		segmentsStats = append(segmentsStats, diagnostics.SnapshotSegmentIndexingStatistics{
			SegmentName: k,
			Percent:     v,
			Alloc:       alloc,
			Sys:         sys,
		})
	}
	diagnostics.Send(diagnostics.SnapshotIndexingStatistics{
		Segments:    segmentsStats,
		TimeElapsed: time.Since(startIndexingTime).Round(time.Second).Seconds(),
	})
}

func noGaps(in []snaptype.FileInfo) (out []snaptype.FileInfo, missingSnapshots []Range) {
	if len(in) == 0 {
		return nil, nil
	}
	prevTo := in[0].From
	for _, f := range in {
		if f.To <= prevTo {
			continue
		}
		if f.From != prevTo { // no gaps
			missingSnapshots = append(missingSnapshots, Range{prevTo, f.From})
			continue
		}
		prevTo = f.To
		out = append(out, f)
	}
	return out, missingSnapshots
}

func typeOfSegmentsMustExist(dir string, in []snaptype.FileInfo, types []snaptype.Type) (res []snaptype.FileInfo) {
MainLoop:
	for _, f := range in {
		if f.From == f.To {
			continue
		}
		for _, t := range types {
			p := filepath.Join(dir, snaptype.SegmentFileName(f.Version, f.From, f.To, t.Enum()))
			exists, err := dir2.FileExist(p)
			if err != nil {
				log.Debug("[snapshots] FileExist error", "err", err, "path", p)
				continue MainLoop
			}
			if !exists {
				continue MainLoop
			}
			res = append(res, f)
		}
	}
	return res
}

// noOverlaps - keep largest ranges and avoid overlap
func noOverlaps(in []snaptype.FileInfo) (res []snaptype.FileInfo) {
	for i := range in {
		f := in[i]
		if f.From == f.To {
			continue
		}

		for j := i + 1; j < len(in); j++ { // if there is file with larger range - use it instead
			f2 := in[j]
			if f2.From == f2.To {
				continue
			}
			if f2.From > f.From {
				break
			}
			f = f2
			i++
		}

		res = append(res, f)
	}

	return res
}

func findOverlaps(in []snaptype.FileInfo) (res []snaptype.FileInfo, overlapped []snaptype.FileInfo) {
	for i := 0; i < len(in); i++ {
		f := in[i]

		if f.From == f.To {
			overlapped = append(overlapped, f)
			continue
		}

		for j := i + 1; j < len(in); i, j = i+1, j+1 { // if there is file with larger range - use it instead
			f2 := in[j]

			if f.Type.Enum() != f2.Type.Enum() {
				break
			}

			if f2.From == f2.To {
				overlapped = append(overlapped, f2)
				continue
			}

			if f2.From > f.From && f2.To > f.To {
				break
			}

			if f.To >= f2.To && f.From <= f2.From {
				overlapped = append(overlapped, f2)
				continue
			}

			if i < len(in)-1 && (f2.To >= f.To && f2.From <= f.From) {
				overlapped = append(overlapped, f)
			}

			f = f2
		}

		res = append(res, f)
	}

	return res, overlapped
}

func SegmentsCaplin(dir string, minBlock uint64) (res []snaptype.FileInfo, missingSnapshots []Range, err error) {
	list, err := snaptype.Segments(dir)
	if err != nil {
		return nil, missingSnapshots, err
	}

	{
		var l, lSidecars []snaptype.FileInfo
		var m []Range
		for _, f := range list {
			if f.Type.Enum() != snaptype.CaplinEnums.BeaconBlocks && f.Type.Enum() != snaptype.CaplinEnums.BlobSidecars {
				continue
			}
			if f.Type.Enum() == snaptype.CaplinEnums.BlobSidecars {
				lSidecars = append(lSidecars, f) // blobs are an exception
				continue
			}
			l = append(l, f)
		}
		l, m = noGaps(noOverlaps(l))
		if len(m) > 0 {
			lst := m[len(m)-1]
			log.Debug("[snapshots] see gap", "type", snaptype.CaplinEnums.BeaconBlocks, "from", lst.from)
		}
		res = append(res, l...)
		res = append(res, lSidecars...)
		missingSnapshots = append(missingSnapshots, m...)
	}
	return res, missingSnapshots, nil
}

func Segments(dir string, minBlock uint64) (res []snaptype.FileInfo, missingSnapshots []Range, err error) {
	return typedSegments(dir, minBlock, coresnaptype.BlockSnapshotTypes, true)
}

func typedSegments(dir string, minBlock uint64, types []snaptype.Type, allowGaps bool) (res []snaptype.FileInfo, missingSnapshots []Range, err error) {
	segmentsTypeCheck := func(dir string, in []snaptype.FileInfo) (res []snaptype.FileInfo) {
		return typeOfSegmentsMustExist(dir, in, types)
	}

	list, err := snaptype.Segments(dir)

	if err != nil {
		return nil, missingSnapshots, err
	}

	for _, segType := range types {
		{
			var l []snaptype.FileInfo
			var m []Range
			for _, f := range list {
				if f.Type.Enum() != segType.Enum() {
					continue
				}
				l = append(l, f)
			}

			if allowGaps {
				l = noOverlaps(segmentsTypeCheck(dir, l))
			} else {
				l, m = noGaps(noOverlaps(segmentsTypeCheck(dir, l)))
			}
			if len(m) > 0 {
				lst := m[len(m)-1]
				log.Debug("[snapshots] see gap", "type", segType, "from", lst.from)
			}
			res = append(res, l...)
			if len(m) > 0 {
				lst := m[len(m)-1]
				log.Debug("[snapshots] see gap", "type", segType, "from", lst.from)
			}

			missingSnapshots = append(missingSnapshots, m...)
		}
	}
	return res, missingSnapshots, nil
}

func chooseSegmentEnd(from, to uint64, snapType snaptype.Enum, chainConfig *chain.Config) uint64 {
	var chainName string

	if chainConfig != nil {
		chainName = chainConfig.ChainName
	}
	blocksPerFile := snapcfg.MergeLimit(chainName, snapType, from)

	next := (from/blocksPerFile + 1) * blocksPerFile
	to = min(next, to)

	if to < snaptype.Erigon2MinSegmentSize {
		return to
	}

	return to - (to % snaptype.Erigon2MinSegmentSize) // round down to the nearest 1k
}

type BlockRetire struct {
	maxScheduledBlock     atomic.Uint64
	working               atomic.Bool
	needSaveFilesListInDB atomic.Bool

	// shared semaphore with AggregatorV3 to allow only one type of snapshot building at a time
	snBuildAllowed *semaphore.Weighted

	workers int
	tmpDir  string
	db      kv.RoDB

	notifier    services.DBEventNotifier
	logger      log.Logger
	blockReader services.FullBlockReader
	blockWriter *blockio.BlockWriter
	dirs        datadir.Dirs
	chainConfig *chain.Config
}

func NewBlockRetire(
	compressWorkers int,
	dirs datadir.Dirs,
	blockReader services.FullBlockReader,
	blockWriter *blockio.BlockWriter,
	db kv.RoDB,
	chainConfig *chain.Config,
	notifier services.DBEventNotifier,
	snBuildAllowed *semaphore.Weighted,
	logger log.Logger,
) *BlockRetire {
	return &BlockRetire{
		workers:        compressWorkers,
		tmpDir:         dirs.Tmp,
		dirs:           dirs,
		blockReader:    blockReader,
		blockWriter:    blockWriter,
		db:             db,
		snBuildAllowed: snBuildAllowed,
		chainConfig:    chainConfig,
		notifier:       notifier,
		logger:         logger,
	}
}

func (br *BlockRetire) SetWorkers(workers int) { br.workers = workers }
func (br *BlockRetire) GetWorkers() int        { return br.workers }

func (br *BlockRetire) IO() (services.FullBlockReader, *blockio.BlockWriter) {
	return br.blockReader, br.blockWriter
}

func (br *BlockRetire) Writer() *RoSnapshots { return br.blockReader.Snapshots().(*RoSnapshots) }

func (br *BlockRetire) snapshots() *RoSnapshots { return br.blockReader.Snapshots().(*RoSnapshots) }

func (br *BlockRetire) borSnapshots() *BorRoSnapshots {
	return br.blockReader.BorSnapshots().(*BorRoSnapshots)
}

func (br *BlockRetire) HasNewFrozenFiles() bool {
	return br.needSaveFilesListInDB.CompareAndSwap(true, false)
}

func CanRetire(curBlockNum uint64, blocksInSnapshots uint64, snapType snaptype.Enum, chainConfig *chain.Config) (blockFrom, blockTo uint64, can bool) {
	var keep uint64 = 1024 //TODO: we will increase it to params.FullImmutabilityThreshold after some db optimizations
	if curBlockNum <= keep {
		return
	}
	blockFrom = blocksInSnapshots + 1
	return canRetire(blockFrom, curBlockNum-keep, snapType, chainConfig)
}

func canRetire(from, to uint64, snapType snaptype.Enum, chainConfig *chain.Config) (blockFrom, blockTo uint64, can bool) {
	if to <= from {
		return
	}
	blockFrom = (from / 1_000) * 1_000
	roundedTo1K := (to / 1_000) * 1_000
	var maxJump uint64 = 1_000

	var chainName string

	if chainConfig != nil {
		chainName = chainConfig.ChainName
	}

	mergeLimit := snapcfg.MergeLimit(chainName, snapType, blockFrom)

	if blockFrom%mergeLimit == 0 {
		maxJump = mergeLimit
	} else if blockFrom%100_000 == 0 {
		maxJump = 100_000
	} else if blockFrom%10_000 == 0 {
		maxJump = 10_000
	}
	//roundedTo1K := (to / 1_000) * 1_000
	jump := min(maxJump, roundedTo1K-blockFrom)
	switch { // only next segment sizes are allowed
	case jump >= mergeLimit:
		blockTo = blockFrom + mergeLimit
	case jump >= 100_000:
		blockTo = blockFrom + 100_000
	case jump >= 10_000:
		blockTo = blockFrom + 10_000
	case jump >= 1_000:
		blockTo = blockFrom + 1_000
	default:
		blockTo = blockFrom
	}
	return blockFrom, blockTo, blockTo-blockFrom >= 1_000
}

func CanDeleteTo(curBlockNum uint64, blocksInSnapshots uint64) (blockTo uint64) {
	if blocksInSnapshots == 0 {
		return 0
	}

	var keep uint64 = 1024 // params.FullImmutabilityThreshold //TODO: we will increase this value after db optimizations - about on-chain-tip prune speed
	if curBlockNum+999 < keep {
		// To prevent overflow of uint64 below
		return blocksInSnapshots + 1
	}
	hardLimit := (curBlockNum/1_000)*1_000 - keep
	return min(hardLimit, blocksInSnapshots+1)
}

func (br *BlockRetire) dbHasEnoughDataForBlocksRetire(ctx context.Context) (bool, error) {
	// pre-check if db has enough data
	var haveGap bool
	if err := br.db.View(ctx, func(tx kv.Tx) error {
		firstInDB, ok, err := rawdb.ReadFirstNonGenesisHeaderNumber(tx)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		lastInFiles := br.snapshots().SegmentsMax() + 1
		haveGap = lastInFiles < firstInDB
		if haveGap {
			log.Debug("[snapshots] not enough blocks in db to create snapshots", "lastInFiles", lastInFiles, " firstBlockInDB", firstInDB, "recommendations", "it's ok to ignore this message. can fix by: downloading more files `rm datadir/snapshots/prohibit_new_downloads.lock datdir/snapshots/snapshots-lock.json`, or downloading old blocks to db `integration stage_headers --reset`")
		}
		return nil
	}); err != nil {
		return false, err
	}
	return !haveGap, nil
}

func (br *BlockRetire) retireBlocks(ctx context.Context, minBlockNum uint64, maxBlockNum uint64, lvl log.Lvl, seedNewSnapshots func(downloadRequest []services.DownloadRequest) error, onDelete func(l []string) error) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	default:
	}

	notifier, logger, blockReader, tmpDir, db, workers := br.notifier, br.logger, br.blockReader, br.tmpDir, br.db, br.workers
	snapshots := br.snapshots()

	blockFrom, blockTo, ok := CanRetire(maxBlockNum, minBlockNum, snaptype.Unknown, br.chainConfig)

	if ok {
		if has, err := br.dbHasEnoughDataForBlocksRetire(ctx); err != nil {
			return false, err
		} else if !has {
			return false, nil
		}
		logger.Log(lvl, "[snapshots] Retire Blocks", "range", fmt.Sprintf("%dk-%dk", blockFrom/1000, blockTo/1000))
		// in future we will do it in background
		if err := DumpBlocks(ctx, blockFrom, blockTo, br.chainConfig, tmpDir, snapshots.Dir(), db, workers, lvl, logger, blockReader); err != nil {
			return ok, fmt.Errorf("DumpBlocks: %w", err)
		}

		if err := snapshots.ReopenFolder(); err != nil {
			return ok, fmt.Errorf("reopen: %w", err)
		}
		snapshots.LogStat("blocks:retire")
		if notifier != nil && !reflect.ValueOf(notifier).IsNil() { // notify about new snapshots of any size
			notifier.OnNewSnapshot()
		}
	}

	merger := NewMerger(tmpDir, workers, lvl, db, br.chainConfig, logger)
	rangesToMerge := merger.FindMergeRanges(snapshots.Ranges(), snapshots.BlocksAvailable())
	if len(rangesToMerge) == 0 {
		return ok, nil
	}
	ok = true // have something to merge
	onMerge := func(r Range) error {
		if notifier != nil && !reflect.ValueOf(notifier).IsNil() { // notify about new snapshots of any size
			notifier.OnNewSnapshot()
		}

		if seedNewSnapshots != nil {
			downloadRequest := []services.DownloadRequest{
				services.NewDownloadRequest("", ""),
			}
			if err := seedNewSnapshots(downloadRequest); err != nil {
				return err
			}
		}
		return nil
	}
	err := merger.Merge(ctx, snapshots, snapshots.Types(), rangesToMerge, snapshots.Dir(), true /* doIndex */, onMerge, onDelete)
	if err != nil {
		return ok, err
	}

	// remove old garbage files
	if err := snapshots.removeOverlapsAfterMerge(); err != nil {
		return false, err
	}
	return ok, nil
}

var ErrNothingToPrune = errors.New("nothing to prune")

func (br *BlockRetire) PruneAncientBlocks(tx kv.RwTx, limit int) (deleted int, err error) {
	if br.blockReader.FreezingCfg().KeepBlocks {
		return deleted, nil
	}
	currentProgress, err := stages.GetStageProgress(tx, stages.Senders)
	if err != nil {
		return deleted, err
	}

	if canDeleteTo := CanDeleteTo(currentProgress, br.blockReader.FrozenBlocks()); canDeleteTo > 0 {
		br.logger.Debug("[snapshots] Prune Blocks", "to", canDeleteTo, "limit", limit)
		deletedBlocks, err := br.blockWriter.PruneBlocks(context.Background(), tx, canDeleteTo, limit)
		if err != nil {
			return deleted, err
		}
		deleted += deletedBlocks
	}

	if br.chainConfig.Bor != nil {
		if canDeleteTo := CanDeleteTo(currentProgress, br.blockReader.FrozenBorBlocks()); canDeleteTo > 0 {
			br.logger.Debug("[snapshots] Prune Bor Blocks", "to", canDeleteTo, "limit", limit)
			deletedBorBlocks, err := br.blockWriter.PruneBorBlocks(context.Background(), tx, canDeleteTo, limit,
				func(block uint64) uint64 { return uint64(heimdall.SpanIdAt(block)) })
			if err != nil {
				return deleted, err
			}
			deleted += deletedBorBlocks
		}

	}

	return deleted, nil
}

func (br *BlockRetire) RetireBlocksInBackground(ctx context.Context, minBlockNum, maxBlockNum uint64, lvl log.Lvl, seedNewSnapshots func(downloadRequest []services.DownloadRequest) error, onDeleteSnapshots func(l []string) error, onFinishRetire func() error) {
	if maxBlockNum > br.maxScheduledBlock.Load() {
		br.maxScheduledBlock.Store(maxBlockNum)
	}

	if !br.working.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer br.working.Store(false)

		if br.snBuildAllowed != nil {
			//we are inside own goroutine - it's fine to block here
			if err := br.snBuildAllowed.Acquire(ctx, 1); err != nil {
				br.logger.Warn("[snapshots] retire blocks", "err", err)
				return
			}
			defer br.snBuildAllowed.Release(1)
		}

		err := br.RetireBlocks(ctx, minBlockNum, maxBlockNum, lvl, seedNewSnapshots, onDeleteSnapshots, onFinishRetire)
		if err != nil {
			br.logger.Warn("[snapshots] retire blocks", "err", err)
			return
		}
	}()
}

func (br *BlockRetire) RetireBlocks(ctx context.Context, minBlockNum uint64, maxBlockNum uint64, lvl log.Lvl, seedNewSnapshots func(downloadRequest []services.DownloadRequest) error, onDeleteSnapshots func(l []string) error, onFinish func() error) error {
	if maxBlockNum > br.maxScheduledBlock.Load() {
		br.maxScheduledBlock.Store(maxBlockNum)
	}
	includeBor := br.chainConfig.Bor != nil

	if err := br.BuildMissedIndicesIfNeed(ctx, "RetireBlocks", br.notifier, br.chainConfig); err != nil {
		return err
	}

	var err error
	for {
		var ok, okBor bool

		minBlockNum = max(br.blockReader.FrozenBlocks(), minBlockNum)
		maxBlockNum = br.maxScheduledBlock.Load()

		if includeBor {
			// "bor snaps" can be behind "block snaps", it's ok: for example because of `kill -9` in the middle of merge
			okBor, err = br.retireBorBlocks(ctx, br.blockReader.FrozenBorBlocks(), minBlockNum, lvl, seedNewSnapshots, onDeleteSnapshots)
			if err != nil {
				return err
			}
		}

		ok, err = br.retireBlocks(ctx, minBlockNum, maxBlockNum, lvl, seedNewSnapshots, onDeleteSnapshots)
		if err != nil {
			return err
		}

		if includeBor {
			minBorBlockNum := max(br.blockReader.FrozenBorBlocks(), minBlockNum)
			okBor, err = br.retireBorBlocks(ctx, minBorBlockNum, maxBlockNum, lvl, seedNewSnapshots, onDeleteSnapshots)
			if err != nil {
				return err
			}
		}
		if onFinish != nil {
			if err := onFinish(); err != nil {
				return err
			}
		}

		if !(ok || okBor) {
			break
		}
	}
	return nil
}

func (br *BlockRetire) BuildMissedIndicesIfNeed(ctx context.Context, logPrefix string, notifier services.DBEventNotifier, cc *chain.Config) error {
	if err := br.snapshots().buildMissedIndicesIfNeed(ctx, logPrefix, notifier, br.dirs, cc, br.logger); err != nil {
		return err
	}

	if cc.Bor != nil {
		if err := br.borSnapshots().RoSnapshots.buildMissedIndicesIfNeed(ctx, logPrefix, notifier, br.dirs, cc, br.logger); err != nil {
			return err
		}
	}

	return nil
}

func DumpBlocks(ctx context.Context, blockFrom, blockTo uint64, chainConfig *chain.Config, tmpDir, snapDir string, chainDB kv.RoDB, workers int, lvl log.Lvl, logger log.Logger, blockReader services.FullBlockReader) error {
	firstTxNum := blockReader.FirstTxnNumNotInSnapshots()
	for i := blockFrom; i < blockTo; i = chooseSegmentEnd(i, blockTo, coresnaptype.Enums.Headers, chainConfig) {
		lastTxNum, err := dumpBlocksRange(ctx, i, chooseSegmentEnd(i, blockTo, coresnaptype.Enums.Headers, chainConfig), tmpDir, snapDir, firstTxNum, chainDB, chainConfig, workers, lvl, logger)
		if err != nil {
			return err
		}
		firstTxNum = lastTxNum + 1
	}
	return nil
}

func dumpBlocksRange(ctx context.Context, blockFrom, blockTo uint64, tmpDir, snapDir string, firstTxNum uint64, chainDB kv.RoDB, chainConfig *chain.Config, workers int, lvl log.Lvl, logger log.Logger) (lastTxNum uint64, err error) {
	logEvery := time.NewTicker(20 * time.Second)
	defer logEvery.Stop()

	if _, err = dumpRange(ctx, coresnaptype.Headers.FileInfo(snapDir, blockFrom, blockTo),
		DumpHeaders, nil, chainDB, chainConfig, tmpDir, workers, lvl, logger); err != nil {
		return 0, err
	}

	if lastTxNum, err = dumpRange(ctx, coresnaptype.Bodies.FileInfo(snapDir, blockFrom, blockTo),
		DumpBodies, func(context.Context) uint64 { return firstTxNum }, chainDB, chainConfig, tmpDir, workers, lvl, logger); err != nil {
		return lastTxNum, err
	}

	if _, err = dumpRange(ctx, coresnaptype.Transactions.FileInfo(snapDir, blockFrom, blockTo),
		DumpTxs, func(context.Context) uint64 { return firstTxNum }, chainDB, chainConfig, tmpDir, workers, lvl, logger); err != nil {
		return lastTxNum, err
	}

	return lastTxNum, nil
}

type firstKeyGetter func(ctx context.Context) uint64
type dumpFunc func(ctx context.Context, db kv.RoDB, chainConfig *chain.Config, blockFrom, blockTo uint64, firstKey firstKeyGetter, collecter func(v []byte) error, workers int, lvl log.Lvl, logger log.Logger) (uint64, error)

func dumpRange(ctx context.Context, f snaptype.FileInfo, dumper dumpFunc, firstKey firstKeyGetter, chainDB kv.RoDB, chainConfig *chain.Config, tmpDir string, workers int, lvl log.Lvl, logger log.Logger) (uint64, error) {
	var lastKeyValue uint64

	sn, err := seg.NewCompressor(ctx, "Snapshot "+f.Type.Name(), f.Path, tmpDir, seg.MinPatternScore, workers, log.LvlTrace, logger)

	if err != nil {
		return lastKeyValue, err
	}
	defer sn.Close()

	lastKeyValue, err = dumper(ctx, chainDB, chainConfig, f.From, f.To, firstKey, func(v []byte) error {
		return sn.AddWord(v)
	}, workers, lvl, logger)

	if err != nil {
		return lastKeyValue, fmt.Errorf("DumpBodies: %w", err)
	}

	ext := filepath.Ext(f.Name())
	logger.Log(lvl, "[snapshots] Compression start", "file", f.Name()[:len(f.Name())-len(ext)], "workers", sn.Workers())

	if err := sn.Compress(); err != nil {
		return lastKeyValue, fmt.Errorf("compress: %w", err)
	}

	p := &background.Progress{}

	if err := f.Type.BuildIndexes(ctx, f, chainConfig, tmpDir, p, lvl, logger); err != nil {
		return lastKeyValue, err
	}

	return lastKeyValue, nil
}

var bufPool = sync.Pool{
	New: func() any {
		bytes := [16 * 4096]byte{}
		return &bytes
	},
}

// DumpTxs - [from, to)
// Format: hash[0]_1byte + sender_address_2bytes + txnRlp
func DumpTxs(ctx context.Context, db kv.RoDB, chainConfig *chain.Config, blockFrom, blockTo uint64, _ firstKeyGetter, collect func([]byte) error, workers int, lvl log.Lvl, logger log.Logger) (lastTx uint64, err error) {
	logEvery := time.NewTicker(20 * time.Second)
	defer logEvery.Stop()
	warmupCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	chainID, _ := uint256.FromBig(chainConfig.ChainID)

	numBuf := make([]byte, 8)

	parse := func(ctx *types2.TxParseContext, v, valueBuf []byte, senders []common2.Address, j int) ([]byte, error) {
		var sender [20]byte
		slot := types2.TxSlot{}

		if _, err := ctx.ParseTransaction(v, 0, &slot, sender[:], false /* hasEnvelope */, false /* wrappedWithBlobs */, nil); err != nil {
			return valueBuf, err
		}
		if len(senders) > 0 {
			sender = senders[j]
		}

		valueBuf = valueBuf[:0]
		valueBuf = append(valueBuf, slot.IDHash[:1]...)
		valueBuf = append(valueBuf, sender[:]...)
		valueBuf = append(valueBuf, v...)
		return valueBuf, nil
	}

	addSystemTx := func(ctx *types2.TxParseContext, tx kv.Tx, txId types.BaseTxnID) error {
		binary.BigEndian.PutUint64(numBuf, txId.U64())
		tv, err := tx.GetOne(kv.EthTx, numBuf)
		if err != nil {
			return err
		}
		if tv == nil {
			if err := collect(nil); err != nil {
				return err
			}
			return nil
		}

		ctx.WithSender(false)

		valueBuf := bufPool.Get().(*[16 * 4096]byte)
		defer bufPool.Put(valueBuf)

		parsed, err := parse(ctx, tv, valueBuf[:], nil, 0)
		if err != nil {
			return err
		}
		if err := collect(parsed); err != nil {
			return err
		}
		return nil
	}

	doWarmup, warmupTxs, warmupSenders := blockTo-blockFrom >= 100_000 && workers > 4, &atomic.Bool{}, &atomic.Bool{}
	from := hexutility.EncodeTs(blockFrom)
	if err := kv.BigChunks(db, kv.HeaderCanonical, from, func(tx kv.Tx, k, v []byte) (bool, error) {
		blockNum := binary.BigEndian.Uint64(k)
		if blockNum >= blockTo { // [from, to)
			return false, nil
		}

		h := common2.BytesToHash(v)
		dataRLP := rawdb.ReadStorageBodyRLP(tx, h, blockNum)
		if dataRLP == nil {
			return false, fmt.Errorf("body not found: %d, %x", blockNum, h)
		}
		var body types.BodyForStorage
		if e := rlp.DecodeBytes(dataRLP, &body); e != nil {
			return false, e
		}
		if body.TxCount == 0 {
			return true, nil
		}

		if doWarmup && !warmupSenders.Load() && blockNum%1_000 == 0 {
			clean := kv.ReadAhead(warmupCtx, db, warmupSenders, kv.Senders, hexutility.EncodeTs(blockNum), 10_000)
			defer clean()
		}
		if doWarmup && !warmupTxs.Load() && blockNum%1_000 == 0 {
			clean := kv.ReadAhead(warmupCtx, db, warmupTxs, kv.EthTx, body.BaseTxnID.Bytes(), 100*10_000)
			defer clean()
		}
		senders, err := rawdb.ReadSenders(tx, h, blockNum)
		if err != nil {
			return false, err
		}

		workers := estimate.AlmostAllCPUs()

		if workers > 3 {
			workers = workers / 3 * 2
		}

		if workers > int(body.TxCount-2) {
			if int(body.TxCount-2) > 1 {
				workers = int(body.TxCount - 2)
			} else {
				workers = 1
			}
		}

		parsers := errgroup.Group{}
		parsers.SetLimit(workers)

		valueBufs := make([][]byte, workers)
		parseCtxs := make([]*types2.TxParseContext, workers)

		for i := 0; i < workers; i++ {
			valueBuf := bufPool.Get().(*[16 * 4096]byte)
			defer bufPool.Put(valueBuf)
			valueBufs[i] = valueBuf[:]
			parseCtxs[i] = types2.NewTxParseContext(*chainID)
		}

		if err := addSystemTx(parseCtxs[0], tx, body.BaseTxnID); err != nil {
			return false, err
		}

		binary.BigEndian.PutUint64(numBuf, body.BaseTxnID.First())

		collected := -1
		collectorLock := sync.Mutex{}
		collections := sync.NewCond(&collectorLock)

		var j int

		if err := tx.ForAmount(kv.EthTx, numBuf, body.TxCount-2, func(_, tv []byte) error {
			tx := j
			j++

			parsers.Go(func() error {
				parseCtx := parseCtxs[tx%workers]

				parseCtx.WithSender(len(senders) == 0)
				parseCtx.WithAllowPreEip2s(blockNum <= chainConfig.HomesteadBlock.Uint64())

				valueBuf, err := parse(parseCtx, tv, valueBufs[tx%workers], senders, tx)

				if err != nil {
					return fmt.Errorf("%w, block: %d", err, blockNum)
				}

				collectorLock.Lock()
				defer collectorLock.Unlock()

				for collected < tx-1 {
					collections.Wait()
				}

				// first tx byte => sender adress => tx rlp
				if err := collect(valueBuf); err != nil {
					return err
				}

				collected = tx
				collections.Broadcast()

				return nil
			})

			return nil
		}); err != nil {
			return false, fmt.Errorf("ForAmount: %w", err)
		}

		if err := parsers.Wait(); err != nil {
			return false, fmt.Errorf("ForAmount parser: %w", err)
		}

		if err := addSystemTx(parseCtxs[0], tx, types.BaseTxnID(body.BaseTxnID.LastSystemTx(body.TxCount))); err != nil {
			return false, err
		}

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-logEvery.C:
			var m runtime.MemStats
			if lvl >= log.LvlInfo {
				dbg.ReadMemStats(&m)
			}
			logger.Log(lvl, "[snapshots] Dumping txs", "block num", blockNum,
				"alloc", common2.ByteCount(m.Alloc), "sys", common2.ByteCount(m.Sys),
			)
		default:
		}
		return true, nil
	}); err != nil {
		return 0, fmt.Errorf("BigChunks: %w", err)
	}
	return 0, nil
}

// DumpHeaders - [from, to)
func DumpHeaders(ctx context.Context, db kv.RoDB, _ *chain.Config, blockFrom, blockTo uint64, _ firstKeyGetter, collect func([]byte) error, workers int, lvl log.Lvl, logger log.Logger) (uint64, error) {
	logEvery := time.NewTicker(20 * time.Second)
	defer logEvery.Stop()

	key := make([]byte, 8+32)
	from := hexutility.EncodeTs(blockFrom)
	if err := kv.BigChunks(db, kv.HeaderCanonical, from, func(tx kv.Tx, k, v []byte) (bool, error) {
		blockNum := binary.BigEndian.Uint64(k)
		if blockNum >= blockTo {
			return false, nil
		}
		copy(key, k)
		copy(key[8:], v)
		dataRLP, err := tx.GetOne(kv.Headers, key)
		if err != nil {
			return false, err
		}
		if dataRLP == nil {
			return false, fmt.Errorf("header missed in db: block_num=%d,  hash=%x", blockNum, v)
		}
		h := types.Header{}
		if err := rlp.DecodeBytes(dataRLP, &h); err != nil {
			return false, err
		}

		value := make([]byte, len(dataRLP)+1) // first_byte_of_header_hash + header_rlp
		value[0] = h.Hash()[0]
		copy(value[1:], dataRLP)
		if err := collect(value); err != nil {
			return false, err
		}

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-logEvery.C:
			var m runtime.MemStats
			if lvl >= log.LvlInfo {
				dbg.ReadMemStats(&m)
			}
			logger.Log(lvl, "[snapshots] Dumping headers", "block num", blockNum,
				"alloc", common2.ByteCount(m.Alloc), "sys", common2.ByteCount(m.Sys),
			)
		default:
		}
		return true, nil
	}); err != nil {
		return 0, err
	}
	return 0, nil
}

// DumpBodies - [from, to)
func DumpBodies(ctx context.Context, db kv.RoDB, _ *chain.Config, blockFrom, blockTo uint64, firstTxNum firstKeyGetter, collect func([]byte) error, workers int, lvl log.Lvl, logger log.Logger) (uint64, error) {
	logEvery := time.NewTicker(20 * time.Second)
	defer logEvery.Stop()

	blockNumByteLength := 8
	blockHashByteLength := 32
	key := make([]byte, blockNumByteLength+blockHashByteLength)
	from := hexutility.EncodeTs(blockFrom)

	lastTxNum := firstTxNum(ctx)

	if err := kv.BigChunks(db, kv.HeaderCanonical, from, func(tx kv.Tx, k, v []byte) (bool, error) {
		blockNum := binary.BigEndian.Uint64(k)
		if blockNum >= blockTo {
			return false, nil
		}
		copy(key, k)
		copy(key[8:], v)

		// Important: DB does store canonical and non-canonical txs in same table. And using same body.BaseTxnID
		// But snapshots using canonical TxNum in field body.BaseTxnID
		// So, we manually calc this field here and serialize again.
		//
		// FYI: we also have other table to map canonical BlockNum->TxNum: kv.MaxTxNum
		body, err := rawdb.ReadBodyForStorageByKey(tx, key)
		if err != nil {
			return false, err
		}
		if body == nil {
			logger.Warn("body missed", "block_num", blockNum, "hash", hex.EncodeToString(v))
			return true, nil
		}
		body.BaseTxnID = types.BaseTxnID(lastTxNum)
		lastTxNum = body.BaseTxnID.LastSystemTx(body.TxCount) + 1 // +1 to set it on first systemTxn of next block

		dataRLP, err := rlp.EncodeToBytes(body)
		if err != nil {
			return false, err
		}

		if err := collect(dataRLP); err != nil {
			return false, err
		}

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-logEvery.C:
			var m runtime.MemStats
			if lvl >= log.LvlInfo {
				dbg.ReadMemStats(&m)
			}
			logger.Log(lvl, "[snapshots] Wrote into file", "block num", blockNum,
				"alloc", common2.ByteCount(m.Alloc), "sys", common2.ByteCount(m.Sys),
			)
		default:
		}
		return true, nil
	}); err != nil {
		return lastTxNum, err
	}

	return lastTxNum, nil
}

func ForEachHeader(ctx context.Context, s *RoSnapshots, walker func(header *types.Header) error) error {
	r := bytes.NewReader(nil)
	word := make([]byte, 0, 2*4096)

	view := s.View()
	defer view.Close()

	for _, sn := range view.Headers() {
		if err := sn.WithReadAhead(func() error {
			g := sn.MakeGetter()
			for g.HasNext() {
				word, _ = g.Next(word[:0])
				var header types.Header
				r.Reset(word[1:])
				if err := rlp.Decode(r, &header); err != nil {
					return err
				}
				if err := walker(&header); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}

	return nil
}

type Merger struct {
	lvl             log.Lvl
	compressWorkers int
	tmpDir          string
	chainConfig     *chain.Config
	chainDB         kv.RoDB
	logger          log.Logger
	noFsync         bool // fsync is enabled by default, but tests can manually disable
}

func NewMerger(tmpDir string, compressWorkers int, lvl log.Lvl, chainDB kv.RoDB, chainConfig *chain.Config, logger log.Logger) *Merger {
	return &Merger{tmpDir: tmpDir, compressWorkers: compressWorkers, lvl: lvl, chainDB: chainDB, chainConfig: chainConfig, logger: logger}
}
func (m *Merger) DisableFsync() { m.noFsync = true }

func (m *Merger) FindMergeRanges(currentRanges []Range, maxBlockNum uint64) (toMerge []Range) {
	for i := len(currentRanges) - 1; i > 0; i-- {
		r := currentRanges[i]
		mergeLimit := snapcfg.MergeLimit(m.chainConfig.ChainName, snaptype.Unknown, r.from)
		if r.to-r.from >= mergeLimit {
			continue
		}
		for _, span := range snapcfg.MergeSteps(m.chainConfig.ChainName, snaptype.Unknown, r.from) {
			if r.to%span != 0 {
				continue
			}
			if r.to-r.from == span {
				break
			}
			aggFrom := r.to - span
			toMerge = append(toMerge, Range{from: aggFrom, to: r.to})
			for currentRanges[i].from > aggFrom {
				i--
			}
			break
		}
	}
	slices.SortFunc(toMerge, func(i, j Range) int { return cmp.Compare(i.from, j.from) })
	return toMerge
}

func (m *Merger) filesByRange(snapshots *RoSnapshots, from, to uint64) (map[snaptype.Enum][]string, error) {
	toMerge := map[snaptype.Enum][]string{}

	view := snapshots.View()
	defer view.Close()

	for _, t := range snapshots.Types() {
		toMerge[t.Enum()] = m.filesByRangeOfType(view, from, to, t)
	}

	return toMerge, nil
}

func (m *Merger) filesByRangeOfType(view *View, from, to uint64, snapshotType snaptype.Type) []string {
	paths := make([]string, 0)

	for _, sn := range view.Segments(snapshotType) {
		if sn.from < from {
			continue
		}
		if sn.to > to {
			break
		}

		paths = append(paths, sn.FilePath())
	}

	return paths
}

func (m *Merger) mergeSubSegment(ctx context.Context, sn snaptype.FileInfo, toMerge []string, snapDir string, doIndex bool, onMerge func(r Range) error) (err error) {
	defer func() {
		if err == nil {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("panic: %v", rec)
			}
		}
		if err != nil {
			f := sn.Path
			_ = os.Remove(f)
			_ = os.Remove(f + ".torrent")
			ext := filepath.Ext(f)
			withoutExt := f[:len(f)-len(ext)]
			_ = os.Remove(withoutExt + ".idx")
			isTxnType := strings.HasSuffix(withoutExt, coresnaptype.Transactions.Name())
			if isTxnType {
				_ = os.Remove(withoutExt + "-to-block.idx")
			}
		}
	}()
	if len(toMerge) == 0 {
		return
	}
	if err = m.merge(ctx, toMerge, sn.Path, nil); err != nil {
		err = fmt.Errorf("mergeByAppendSegments: %w", err)
		return
	}

	if doIndex {
		p := &background.Progress{}
		if err = buildIdx(ctx, sn, m.chainConfig, m.tmpDir, p, m.lvl, m.logger); err != nil {
			return
		}
	}

	return
}

// Merge does merge segments in given ranges
func (m *Merger) Merge(ctx context.Context, snapshots *RoSnapshots, snapTypes []snaptype.Type, mergeRanges []Range, snapDir string, doIndex bool, onMerge func(r Range) error, onDelete func(l []string) error) (err error) {
	if len(mergeRanges) == 0 {
		return nil
	}
	logEvery := time.NewTicker(30 * time.Second)
	defer logEvery.Stop()
	for _, r := range mergeRanges {
		toMerge, err := m.filesByRange(snapshots, r.from, r.to)
		if err != nil {
			return err
		}

		for _, t := range snapTypes {
			if err := m.mergeSubSegment(ctx, t.FileInfo(snapDir, r.from, r.to), toMerge[t.Enum()], snapDir, doIndex, onMerge); err != nil {
				return err
			}
		}
		if err := snapshots.ReopenFolder(); err != nil {
			return fmt.Errorf("ReopenSegments: %w", err)
		}

		snapshots.LogStat("merge")

		if onMerge != nil {
			if err := onMerge(r); err != nil {
				return err
			}
		}

		for _, t := range snapTypes {
			if len(toMerge[t.Enum()]) == 0 {
				continue
			}
			if onDelete != nil {
				if err := onDelete(toMerge[t.Enum()]); err != nil {
					return err
				}
			}
			removeOldFiles(toMerge[t.Enum()], snapDir)
		}
	}
	m.logger.Log(m.lvl, "[snapshots] Merge done", "from", mergeRanges[0].from, "to", mergeRanges[0].to)
	return nil
}

func (m *Merger) merge(ctx context.Context, toMerge []string, targetFile string, logEvery *time.Ticker) error {
	var word = make([]byte, 0, 4096)
	var expectedTotal int
	cList := make([]*seg.Decompressor, len(toMerge))
	for i, cFile := range toMerge {
		d, err := seg.NewDecompressor(cFile)
		if err != nil {
			return err
		}
		defer d.Close()
		cList[i] = d
		expectedTotal += d.Count()
	}

	f, err := seg.NewCompressor(ctx, "Snapshots merge", targetFile, m.tmpDir, seg.MinPatternScore, m.compressWorkers, log.LvlTrace, m.logger)
	if err != nil {
		return err
	}
	defer f.Close()
	if m.noFsync {
		f.DisableFsync()
	}

	_, fName := filepath.Split(targetFile)
	m.logger.Debug("[snapshots] merge", "file", fName)

	for _, d := range cList {
		if err := d.WithReadAhead(func() error {
			g := d.MakeGetter()
			for g.HasNext() {
				word, _ = g.Next(word[:0])
				if err := f.AddWord(word); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if f.Count() != expectedTotal {
		return fmt.Errorf("unexpected amount after segments merge. got: %d, expected: %d", f.Count(), expectedTotal)
	}
	if err = f.Compress(); err != nil {
		return err
	}
	return nil
}

func removeOldFiles(toDel []string, snapDir string) {
	for _, f := range toDel {
		_ = os.Remove(f)
		_ = os.Remove(f + ".torrent")
		ext := filepath.Ext(f)
		withoutExt := f[:len(f)-len(ext)]
		_ = os.Remove(withoutExt + ".idx")
		isTxnType := strings.HasSuffix(withoutExt, coresnaptype.Transactions.Name())
		if isTxnType {
			_ = os.Remove(withoutExt + "-to-block.idx")
		}
	}
	tmpFiles, err := snaptype.TmpFiles(snapDir)
	if err != nil {
		return
	}
	for _, f := range tmpFiles {
		_ = os.Remove(f)
	}
}

type View struct {
	s           *RoSnapshots
	baseSegType snaptype.Type
	closed      bool
}

func (s *RoSnapshots) View() *View {
	v := &View{s: s, baseSegType: coresnaptype.Headers}
	s.lockSegments()
	return v
}

func (v *View) Close() {
	if v.closed {
		return
	}
	v.closed = true
	v.s.unlockSegments()
}

func (v *View) Segments(t snaptype.Type) []*Segment {
	if s, ok := v.s.segments.Get(t.Enum()); ok {
		return s.segments
	}
	return nil
}

func (v *View) Headers() []*Segment { return v.Segments(coresnaptype.Headers) }
func (v *View) Bodies() []*Segment  { return v.Segments(coresnaptype.Bodies) }
func (v *View) Txs() []*Segment     { return v.Segments(coresnaptype.Transactions) }

func (v *View) Segment(t snaptype.Type, blockNum uint64) (*Segment, bool) {
	if s, ok := v.s.segments.Get(t.Enum()); ok {
		for _, seg := range s.segments {
			if !(blockNum >= seg.from && blockNum < seg.to) {
				continue
			}
			return seg, true
		}
	}
	return nil, false
}

func (v *View) Ranges() (ranges []Range) {
	for _, sn := range v.Segments(v.baseSegType) {
		ranges = append(ranges, sn.Range)
	}

	return ranges
}

func (v *View) HeadersSegment(blockNum uint64) (*Segment, bool) {
	return v.Segment(coresnaptype.Headers, blockNum)
}

func (v *View) BodiesSegment(blockNum uint64) (*Segment, bool) {
	return v.Segment(coresnaptype.Bodies, blockNum)
}
func (v *View) TxsSegment(blockNum uint64) (*Segment, bool) {
	return v.Segment(coresnaptype.Transactions, blockNum)
}

func RemoveIncompatibleIndices(dirs datadir.Dirs) error {
	l, err := dir2.ListFiles(dirs.Snap, ".idx")
	if err != nil {
		return err
	}
	l1, err := dir2.ListFiles(dirs.SnapAccessors, ".efi")
	if err != nil {
		return err
	}
	l2, err := dir2.ListFiles(dirs.SnapAccessors, ".vi")
	if err != nil {
		return err
	}
	l = append(append(l, l1...), l2...)

	for _, fPath := range l {
		index, err := recsplit.OpenIndex(fPath)
		if err != nil {
			if errors.Is(err, recsplit.IncompatibleErr) {
				_, fName := filepath.Split(fPath)
				if err = os.Remove(fPath); err != nil {
					log.Warn("Removing incompatible index", "file", fName, "err", err)
				} else {
					log.Info("Removing incompatible index", "file", fName)
				}
				continue
			}
			return fmt.Errorf("%w, %s", err, fPath)
		}
		index.Close()
	}
	return nil
}
