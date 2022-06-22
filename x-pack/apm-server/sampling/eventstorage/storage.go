// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

package eventstorage

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/dgraph-io/badger/v2"

	"github.com/elastic/apm-server/model"
)

const (
	// NOTE(axw) these values (and their meanings) must remain stable
	// over time, to avoid misinterpreting historical data.
	entryMetaTraceSampled   = 's'
	entryMetaTraceUnsampled = 'u'
	entryMetaTraceEvent     = 'e'
)

const (
	storageLimitThreshold = 0.90 // Allow 90% of the quota to be used.
)

var (
	// ErrNotFound is returned by by the Storage.IsTraceSampled method,
	// for non-existing trace IDs.
	ErrNotFound = errors.New("key not found")

	// ErrLimitReached is returned by the ReadWriter.Flush method when
	// the configured StorageLimiter.Limit is true.
	ErrLimitReached = fmt.Errorf("configured storage limit reached: %w", syscall.ENOSPC)
)

// Storage provides storage for sampled transactions and spans,
// and for recording trace sampling decisions.
type Storage struct {
	db    *badger.DB
	codec Codec
	ttl   time.Duration
	limit int64
}

// Codec provides methods for encoding and decoding events.
type Codec interface {
	DecodeEvent([]byte, *model.APMEvent) error
	EncodeEvent(*model.APMEvent) ([]byte, error)
}

// New returns a new Storage using db and codec.
//
// Storage entries expire after ttl.
// The amount of storage that can be consumed can be limited by passing in a
// limit value greater than zero. The hard limit on storage is set to 90% of
// the limit to account for delay in the size reporting by badger.
// https://github.com/dgraph-io/badger/blob/82b00f27e3827022082225221ae05c03f0d37620/db.go#L1302-L1319.
func New(db *badger.DB, codec Codec, ttl time.Duration, limit int64) *Storage {
	if limit > 1 {
		limit = int64(float64(limit) * storageLimitThreshold)
	}
	return &Storage{db: db, codec: codec, ttl: ttl, limit: limit}
}

// NewShardedReadWriter returns a new ShardedReadWriter, for sharded
// reading and writing.
//
// The returned ShardedReadWriter must be closed when it is no longer
// needed.
func (s *Storage) NewShardedReadWriter() *ShardedReadWriter {
	return newShardedReadWriter(s)
}

// NewReadWriter returns a new ReadWriter for reading events from and
// writing events to storage.
//
// The returned ReadWriter must be closed when it is no longer needed.
func (s *Storage) NewReadWriter() *ReadWriter {
	return &ReadWriter{
		s:   s,
		txn: s.db.NewTransaction(true),
	}
}

func (s *Storage) limitReached() bool {
	if s.limit == 0 {
		return false
	}
	// The badger database has an async size reconciliation, with a 1 minute
	// ticker that keeps the lsm and vlog sizes updated in an in-memory map.
	// It's OK to call call s.db.Size() on the hot path, since the memory
	// lookup is cheap.
	lsm, vlog := s.db.Size()
	current := lsm + vlog
	return current >= s.limit
}

// ReadWriter provides a means of reading events from storage, and batched
// writing of events to storage.
//
// ReadWriter is not safe for concurrent access. All operations that involve
// a given trace ID should be performed with the same ReadWriter in order to
// avoid conflicts, e.g. by using consistent hashing to distribute to one of
// a set of ReadWriters, such as implemented by ShardedReadWriter.
type ReadWriter struct {
	s   *Storage
	txn *badger.Txn

	// readKeyBuf is a reusable buffer for keys used in read operations.
	// This must not be used in write operations, as keys are expected to
	// be unmodified until the end of a transaction.
	readKeyBuf    []byte
	pendingWrites int
}

// Close closes the writer. Any writes that have not been flushed may be lost.
//
// This must be called when the writer is no longer needed, in order to reclaim
// resources.
func (rw *ReadWriter) Close() {
	rw.txn.Discard()
}

const flushErrFmt = "flush pending writes: %w"

// Flush waits for preceding writes to be committed to storage.
//
// Flush must be called to ensure writes are committed to storage.
// If Flush is not called before the writer is closed, then writes
// may be lost.
// Flush returns ErrLimitReached when the StorageLimiter reports that
// the size of LSM and Vlog files exceeds the configured threshold.
func (rw *ReadWriter) Flush() error {
	if rw.s.limitReached() {
		return fmt.Errorf(flushErrFmt, ErrLimitReached)
	}
	err := rw.txn.Commit()
	rw.txn = rw.s.db.NewTransaction(true)
	rw.pendingWrites = 0
	if err != nil {
		return fmt.Errorf(flushErrFmt, err)
	}
	return nil
}

// WriteTraceSampled records the tail-sampling decision for the given trace ID.
func (rw *ReadWriter) WriteTraceSampled(traceID string, sampled bool) error {
	key := []byte(traceID)
	var meta uint8 = entryMetaTraceUnsampled
	if sampled {
		meta = entryMetaTraceSampled
	}
	entry := badger.NewEntry(key[:], nil).WithMeta(meta)
	return rw.writeEntry(entry.WithTTL(rw.s.ttl))
}

// IsTraceSampled reports whether traceID belongs to a trace that is sampled
// or unsampled. If no sampling decision has been recorded, IsTraceSampled
// returns ErrNotFound.
func (rw *ReadWriter) IsTraceSampled(traceID string) (bool, error) {
	rw.readKeyBuf = append(rw.readKeyBuf[:0], traceID...)
	item, err := rw.txn.Get(rw.readKeyBuf)
	if err != nil {
		if err == badger.ErrKeyNotFound {
			return false, ErrNotFound
		}
		return false, err
	}
	return item.UserMeta() == entryMetaTraceSampled, nil
}

// WriteTraceEvent writes a trace event to storage.
//
// WriteTraceEvent may return before the write is committed to storage.
// Call Flush to ensure the write is committed.
func (rw *ReadWriter) WriteTraceEvent(traceID string, id string, event *model.APMEvent) error {
	key := append(append([]byte(traceID), ':'), id...)
	data, err := rw.s.codec.EncodeEvent(event)
	if err != nil {
		return err
	}
	return rw.writeEntry(badger.NewEntry(key[:], data).
		WithMeta(entryMetaTraceEvent).
		WithTTL(rw.s.ttl),
	)
}

func (rw *ReadWriter) writeEntry(e *badger.Entry) error {
	rw.pendingWrites++
	err := rw.txn.SetEntry(e)
	// Attempt to flush if there are 200 or more uncommitted writes.
	// This ensures calls to ReadTraceEvents are not slowed down;
	// ReadTraceEvents uses an iterator, which must sort all keys
	// of uncommitted writes.
	// The 200 value yielded a good balance between read and write speed:
	// https://github.com/elastic/apm-server/pull/8407#issuecomment-1162994643
	if rw.pendingWrites >= 200 {
		if err := rw.Flush(); err != nil {
			return err
		}
	}
	// If the transaction is already too big to accommodate the new entry, flush
	// the existing transaction and set the entry on a new one, otherwise,
	// returns early.
	if err != badger.ErrTxnTooBig {
		return err
	}
	if err := rw.Flush(); err != nil {
		return err
	}
	return rw.txn.SetEntry(e)
}

// DeleteTraceEvent deletes the trace event from storage.
func (rw *ReadWriter) DeleteTraceEvent(traceID, id string) error {
	key := append(append([]byte(traceID), ':'), id...)
	return rw.txn.Delete(key)
}

// ReadTraceEvents reads trace events with the given trace ID from storage into out.
func (rw *ReadWriter) ReadTraceEvents(traceID string, out *model.Batch) error {
	opts := badger.DefaultIteratorOptions
	rw.readKeyBuf = append(append(rw.readKeyBuf[:0], traceID...), ':')
	opts.Prefix = rw.readKeyBuf

	iter := rw.txn.NewIterator(opts)
	defer iter.Close()
	for iter.Rewind(); iter.Valid(); iter.Next() {
		item := iter.Item()
		if item.IsDeletedOrExpired() {
			continue
		}
		switch item.UserMeta() {
		case entryMetaTraceEvent:
			var event model.APMEvent
			if err := item.Value(func(data []byte) error {
				return rw.s.codec.DecodeEvent(data, &event)
			}); err != nil {
				return err
			}
			*out = append(*out, event)
		default:
			// Unknown entry meta: ignore.
			continue
		}
	}
	return nil
}
