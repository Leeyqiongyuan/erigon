package heimdall

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"sync"

	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/iter"
	"github.com/ledgerwatch/erigon/polygon/polygoncommon"
)

var databaseTablesCfg = kv.TableCfg{
	kv.BorCheckpoints: {},
	kv.BorMilestones:  {},
	kv.BorSpans:       {},
}

type EntityStore[TEntity Entity] interface {
	Prepare(ctx context.Context) error
	Close()
	GetLastEntityId(ctx context.Context) (uint64, bool, error)
	GetLastEntity(ctx context.Context) (TEntity, error)
	GetEntity(ctx context.Context, id uint64) (TEntity, error)
	PutEntity(ctx context.Context, id uint64, entity TEntity) error
	FindByBlockNum(ctx context.Context, blockNum uint64) (TEntity, error)
	RangeFromId(ctx context.Context, startId uint64) ([]TEntity, error)
	RangeFromBlockNum(ctx context.Context, startBlockNum uint64) ([]TEntity, error)
}

type RangeIndexFactory func(ctx context.Context) (*RangeIndex, error)

type mdbxEntityStore[TEntity Entity] struct {
	db    *polygoncommon.Database
	label kv.Label
	table string

	makeEntity func() TEntity

	blockNumToIdIndexFactory RangeIndexFactory
	blockNumToIdIndex        *RangeIndex
	prepareOnce              sync.Once
}

func newMdbxEntityStore[TEntity Entity](
	db *polygoncommon.Database,
	label kv.Label,
	table string,
	makeEntity func() TEntity,
	blockNumToIdIndexFactory RangeIndexFactory,
) *mdbxEntityStore[TEntity] {
	return &mdbxEntityStore[TEntity]{
		db:    db,
		label: label,
		table: table,

		makeEntity: makeEntity,

		blockNumToIdIndexFactory: blockNumToIdIndexFactory,
	}
}

func (s *mdbxEntityStore[TEntity]) Prepare(ctx context.Context) error {
	var err error
	s.prepareOnce.Do(func() {
		err = s.db.OpenOnce(ctx, s.label, databaseTablesCfg)
		if err != nil {
			return
		}
		s.blockNumToIdIndex, err = s.blockNumToIdIndexFactory(ctx)
		if err != nil {
			return
		}
		iteratorFactory := func(tx kv.Tx) (iter.KV, error) { return tx.Range(s.table, nil, nil) }
		err = buildBlockNumToIdIndex(ctx, s.blockNumToIdIndex, s.db.BeginRo, iteratorFactory, s.entityUnmarshalJSON)
	})
	return err
}

func (s *mdbxEntityStore[TEntity]) Close() {
	s.blockNumToIdIndex.Close()
}

func (s *mdbxEntityStore[TEntity]) GetLastEntityId(ctx context.Context) (uint64, bool, error) {
	tx, err := s.db.BeginRo(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	cursor, err := tx.Cursor(s.table)
	if err != nil {
		return 0, false, err
	}
	defer cursor.Close()

	lastKey, _, err := cursor.Last()
	if err != nil {
		return 0, false, err
	}
	// not found
	if lastKey == nil {
		return 0, false, nil
	}

	return entityStoreKeyParse(lastKey), true, nil
}

// Zero value of any type T
// https://stackoverflow.com/questions/70585852/return-default-value-for-generic-type)
// https://go.dev/ref/spec#The_zero_value
func Zero[T any]() T {
	var value T
	return value
}

func (s *mdbxEntityStore[TEntity]) GetLastEntity(ctx context.Context) (TEntity, error) {
	id, ok, err := s.GetLastEntityId(ctx)
	if err != nil {
		return Zero[TEntity](), err
	}
	// not found
	if !ok {
		return Zero[TEntity](), nil
	}
	return s.GetEntity(ctx, id)
}

func entityStoreKey(id uint64) [8]byte {
	var key [8]byte
	binary.BigEndian.PutUint64(key[:], id)
	return key
}

func entityStoreKeyParse(key []byte) uint64 {
	return binary.BigEndian.Uint64(key)
}

func (s *mdbxEntityStore[TEntity]) entityUnmarshalJSON(jsonBytes []byte) (TEntity, error) {
	entity := s.makeEntity()
	if err := json.Unmarshal(jsonBytes, entity); err != nil {
		return Zero[TEntity](), err
	}
	return entity, nil
}

func (s *mdbxEntityStore[TEntity]) GetEntity(ctx context.Context, id uint64) (TEntity, error) {
	tx, err := s.db.BeginRo(ctx)
	if err != nil {
		return Zero[TEntity](), err
	}
	defer tx.Rollback()

	key := entityStoreKey(id)
	jsonBytes, err := tx.GetOne(s.table, key[:])
	if err != nil {
		return Zero[TEntity](), err
	}
	// not found
	if jsonBytes == nil {
		return Zero[TEntity](), nil
	}

	return s.entityUnmarshalJSON(jsonBytes)
}

func (s *mdbxEntityStore[TEntity]) PutEntity(ctx context.Context, id uint64, entity TEntity) error {
	tx, err := s.db.BeginRw(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	jsonBytes, err := json.Marshal(entity)
	if err != nil {
		return err
	}

	key := entityStoreKey(id)
	if err = tx.Put(s.table, key[:], jsonBytes); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}

	// update blockNumToIdIndex
	return s.blockNumToIdIndex.Put(ctx, entity.BlockNumRange(), id)
}

func (s *mdbxEntityStore[TEntity]) FindByBlockNum(ctx context.Context, blockNum uint64) (TEntity, error) {
	id, err := s.blockNumToIdIndex.Lookup(ctx, blockNum)
	if err != nil {
		return Zero[TEntity](), err
	}
	// not found
	if id == 0 {
		return Zero[TEntity](), nil
	}

	return s.GetEntity(ctx, id)
}

func (s *mdbxEntityStore[TEntity]) RangeFromId(ctx context.Context, startId uint64) ([]TEntity, error) {
	tx, err := s.db.BeginRo(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	startKey := entityStoreKey(startId)
	it, err := tx.Range(s.table, startKey[:], nil)
	if err != nil {
		return nil, err
	}

	var entities []TEntity
	for it.HasNext() {
		_, jsonBytes, err := it.Next()
		if err != nil {
			return nil, err
		}

		entity, err := s.entityUnmarshalJSON(jsonBytes)
		if err != nil {
			return nil, err
		}
		entities = append(entities, entity)
	}
	return entities, nil
}

func (s *mdbxEntityStore[TEntity]) RangeFromBlockNum(ctx context.Context, startBlockNum uint64) ([]TEntity, error) {
	id, err := s.blockNumToIdIndex.Lookup(ctx, startBlockNum)
	if err != nil {
		return nil, err
	}
	// not found
	if id == 0 {
		return nil, nil
	}

	return s.RangeFromId(ctx, id)
}

func buildBlockNumToIdIndex[TEntity Entity](
	ctx context.Context,
	index *RangeIndex,
	txFactory func(context.Context) (kv.Tx, error),
	iteratorFactory func(tx kv.Tx) (iter.KV, error),
	entityUnmarshalJSON func([]byte) (TEntity, error),
) error {
	tx, err := txFactory(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	it, err := iteratorFactory(tx)
	if err != nil {
		return err
	}
	defer it.Close()

	for it.HasNext() {
		_, jsonBytes, err := it.Next()
		if err != nil {
			return err
		}

		entity, err := entityUnmarshalJSON(jsonBytes)
		if err != nil {
			return err
		}

		if err = index.Put(ctx, entity.BlockNumRange(), entity.RawId()); err != nil {
			return err
		}
	}

	return nil
}
