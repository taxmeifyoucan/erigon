package olddb

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"time"
	"unsafe"

	lru "github.com/hashicorp/golang-lru"
	"github.com/ledgerwatch/erigon-lib/etl"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon/ethdb"
	"github.com/ledgerwatch/log/v3"
)

type mapmutation struct {
	puts    map[string]map[string][]byte
	tableId map[string]byte
	db      kv.RwTx
	quit    <-chan struct{}
	clean   func()
	mu      sync.RWMutex
	size    int
	count   uint64
	cache   *lru.Cache
	tmpdir  string
}

// NewBatch - starts in-mem batch
//
// Common pattern:
//
// batch := db.NewBatch()
// defer batch.Rollback()
// ... some calculations on `batch`
// batch.Commit()
func NewHashBatch(tx kv.RwTx, quit <-chan struct{}, cache *lru.Cache, tmpdir string) *mapmutation {
	clean := func() {}
	if quit == nil {
		ch := make(chan struct{})
		clean = func() { close(ch) }
		quit = ch
	}
	return &mapmutation{
		db:      tx,
		puts:    make(map[string]map[string][]byte),
		quit:    quit,
		clean:   clean,
		tmpdir:  tmpdir,
		cache:   cache,
		tableId: make(map[string]byte),
	}
}

func (m *mapmutation) RwKV() kv.RwDB {
	if casted, ok := m.db.(ethdb.HasRwKV); ok {
		return casted.RwKV()
	}
	return nil
}

func createCacheKey(id byte, key []byte) string {
	return string(append(key, id))
}

func (m *mapmutation) getMem(table string, key []byte) ([]byte, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.puts[table]; !ok {
		return nil, false
	}
	var value []byte
	var ok bool
	if value, ok = m.puts[table][*(*string)(unsafe.Pointer(&key))]; !ok {
		return nil, false
	}

	if m.cache != nil {
		value, ok := m.cache.Get(createCacheKey(m.tableId[table], key))
		if ok {
			return value.([]byte), ok
		}
	}
	return value, ok
}

func (m *mapmutation) IncrementSequence(bucket string, amount uint64) (res uint64, err error) {
	v, ok := m.getMem(kv.Sequence, []byte(bucket))
	if !ok && m.db != nil {
		v, err = m.db.GetOne(kv.Sequence, []byte(bucket))
		if err != nil {
			return 0, err
		}
	}

	var currentV uint64 = 0
	if len(v) > 0 {
		currentV = binary.BigEndian.Uint64(v)
	}

	newVBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(newVBytes, currentV+amount)
	if err = m.Put(kv.Sequence, []byte(bucket), newVBytes); err != nil {
		return 0, err
	}

	return currentV, nil
}
func (m *mapmutation) ReadSequence(bucket string) (res uint64, err error) {
	v, ok := m.getMem(kv.Sequence, []byte(bucket))
	if !ok && m.db != nil {
		v, err = m.db.GetOne(kv.Sequence, []byte(bucket))
		if err != nil {
			return 0, err
		}
	}
	var currentV uint64 = 0
	if len(v) > 0 {
		currentV = binary.BigEndian.Uint64(v)
	}

	return currentV, nil
}

// Can only be called from the worker thread
func (m *mapmutation) GetOne(table string, key []byte) ([]byte, error) {
	if value, ok := m.getMem(table, key); ok {
		if value == nil {
			return nil, nil
		}
		return value, nil
	}
	if m.db != nil {
		// TODO: simplify when tx can no longer be parent of mutation
		value, err := m.db.GetOne(table, key)
		if err != nil {
			return nil, err
		}
		if m.cache != nil {
			var id byte
			var ok bool
			if id, ok = m.tableId[table]; !ok {
				id = byte(len(m.tableId))
				m.tableId[table] = id
			}
			m.cache.Add(createCacheKey(id, key), value)
		}
		return value, nil
	}

	return nil, nil
}

// Can only be called from the worker thread
func (m *mapmutation) Get(table string, key []byte) ([]byte, error) {
	value, err := m.GetOne(table, key)
	if err != nil {
		return nil, err
	}

	if value == nil {
		return nil, ethdb.ErrKeyNotFound
	}

	return value, nil
}

func (m *mapmutation) Last(table string) ([]byte, []byte, error) {
	c, err := m.db.Cursor(table)
	if err != nil {
		return nil, nil, err
	}
	defer c.Close()
	return c.Last()
}

func (m *mapmutation) Has(table string, key []byte) (bool, error) {
	if _, ok := m.getMem(table, key); ok {
		return true, nil
	}
	if m.db != nil {
		return m.db.Has(table, key)
	}
	return false, nil
}

func (m *mapmutation) Put(table string, key []byte, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.puts[table]; !ok {
		m.puts[table] = make(map[string][]byte)
		m.tableId[table] = byte(len(m.tableId))
	}
	var ok bool
	keyString := *(*string)(unsafe.Pointer(&key))
	if _, ok = m.puts[table][keyString]; !ok {
		m.size += len(value) - len(m.puts[table][keyString])
		m.puts[table][keyString] = value
		m.count++
	} else {
		m.puts[table][keyString] = value
		m.size += len(key) + len(value)
	}
	if m.cache != nil {
		var id byte
		if id, ok = m.tableId[table]; !ok {
			id = byte(len(m.tableId))
			m.tableId[table] = id
		}
		m.cache.Add(createCacheKey(id, key), value)
	}

	return nil
}

func (m *mapmutation) Append(table string, key []byte, value []byte) error {
	return m.Put(table, key, value)
}

func (m *mapmutation) AppendDup(table string, key []byte, value []byte) error {
	return m.Put(table, key, value)
}

func (m *mapmutation) BatchSize() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.size
}

func (m *mapmutation) ForEach(bucket string, fromPrefix []byte, walker func(k, v []byte) error) error {
	m.panicOnEmptyDB()
	return m.db.ForEach(bucket, fromPrefix, walker)
}

func (m *mapmutation) ForPrefix(bucket string, prefix []byte, walker func(k, v []byte) error) error {
	m.panicOnEmptyDB()
	return m.db.ForPrefix(bucket, prefix, walker)
}

func (m *mapmutation) ForAmount(bucket string, prefix []byte, amount uint32, walker func(k, v []byte) error) error {
	m.panicOnEmptyDB()
	return m.db.ForAmount(bucket, prefix, amount, walker)
}

func (m *mapmutation) Delete(table string, k, v []byte) error {
	if v != nil {
		return m.db.Delete(table, k, v) // TODO: mutation to support DupSort deletes
	}
	return m.Put(table, k, nil)
}

func (m *mapmutation) doCommit(tx kv.RwTx) error {
	logEvery := time.NewTicker(30 * time.Second)
	defer logEvery.Stop()
	count := 0
	total := float64(m.count)

	for table, bucket := range m.puts {
		collector := etl.NewCollector("", m.tmpdir, etl.NewSortableBuffer(etl.BufferOptimalSize))
		defer collector.Close()
		for key, value := range bucket {
			collector.Collect([]byte(key), value)
			count++
			select {
			default:
			case <-logEvery.C:
				progress := fmt.Sprintf("%.1fM/%.1fM", float64(count)/1_000_000, total/1_000_000)
				log.Info("Write to db", "progress", progress, "current table", table)
				tx.CollectMetrics()
			}
		}
		if err := collector.Load(m.db, table, etl.IdentityLoadFunc, etl.TransformArgs{Quit: m.quit}); err != nil {
			return err
		}
	}

	tx.CollectMetrics()
	return nil
}

func (m *mapmutation) Commit() error {
	if m.db == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.doCommit(m.db); err != nil {
		return err
	}

	m.puts = map[string]map[string][]byte{}
	m.size = 0
	m.count = 0
	m.clean()
	return nil
}

func (m *mapmutation) Rollback() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts = map[string]map[string][]byte{}
	m.size = 0
	m.count = 0
	m.size = 0
	m.clean()
}

func (m *mapmutation) Close() {
	m.Rollback()
}

func (m *mapmutation) Begin(ctx context.Context, flags ethdb.TxFlags) (ethdb.DbWithPendingMutations, error) {
	panic("mutation can't start transaction, because doesn't own it")
}

func (m *mapmutation) panicOnEmptyDB() {
	if m.db == nil {
		panic("Not implemented")
	}
}

func (m *mapmutation) SetRwKV(kv kv.RwDB) {
	m.db.(ethdb.HasRwKV).SetRwKV(kv)
}
