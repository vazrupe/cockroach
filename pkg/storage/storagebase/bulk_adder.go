// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package storagebase

import (
	"context"
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
)

// BulkAdderOptions is used to configure the behavior of a BulkAdder.
type BulkAdderOptions struct {
	// Name is used in logging messages to identify this adder or the process on
	// behalf of which it is adding data.
	Name string

	// SSTSize is the size at which an SST will be flushed and a new one started.
	// SSTs are also split during a buffer flush to avoid spanning range bounds so
	// they may be smaller than this limit.
	SSTSize uint64

	// BufferSize is the maximum amount of data to buffer before flushing SSTs.
	BufferSize uint64

	// SkipLocalDuplicates configures handling of duplicate keys within a local
	// sorted batch. When true if the same key/value pair is added more than once
	// subsequent additions will be ignored instead of producing an error. If an
	// attempt to add the same key has a differnet value, it is always an error.
	// Once a batch is flushed – explicitly or automatically – local duplicate
	// detection does not apply.
	SkipDuplicates bool

	// DisallowShadowing controls whether shadowing of existing keys is permitted
	// when the SSTables produced by this adder are ingested.
	DisallowShadowing bool
}

// BulkAdderFactory describes a factory function for BulkAdders.
type BulkAdderFactory func(
	ctx context.Context, db *client.DB, timestamp hlc.Timestamp, opts BulkAdderOptions,
) (BulkAdder, error)

// BulkAdder describes a bulk-adding helper that can be used to add lots of KVs.
type BulkAdder interface {
	// Add adds a KV pair to the adder's buffer, potentially flushing if needed.
	Add(ctx context.Context, key roachpb.Key, value []byte) error
	// Flush explicitly flushes anything remaining in the adder's buffer.
	Flush(ctx context.Context) error
	// CurrentBufferFill returns how full the configured buffer is.
	CurrentBufferFill() float32
	// GetSummary returns a summary of rows/bytes/etc written by this batcher.
	GetSummary() roachpb.BulkOpSummary
	// Close closes the underlying buffers/writers.
	Close(ctx context.Context)
}

// DuplicateKeyError represents a failed attempt to ingest the same key twice
// using a BulkAdder within the same batch.
type DuplicateKeyError struct {
	Key   roachpb.Key
	Value []byte
}

func (d DuplicateKeyError) Error() string {
	return fmt.Sprintf("duplicate key: %s", d.Key)
}
