package storage

import (
	"sync"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/encoding"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
)

const (
	// The maximum size of values in the block.
	maxBlockSize = 64 * 1024

	// The maximum number of rows per block.
	maxRowsPerBlock = 8 * 1024
)

// Block represents a block of time series values for a single TSID.
type Block struct {
	bh blockHeader

	// nextIdx is the next row index for timestamps and values.
	nextIdx int

	timestamps []int64
	values     []int64

	// Marshaled representation of block header.
	headerData []byte

	// Marshaled representation of timestamps.
	timestampsData []byte

	// Marshaled representation of values.
	valuesData []byte
}

// Reset resets b.
func (b *Block) Reset() {
	b.bh = blockHeader{}
	b.nextIdx = 0
	b.timestamps = b.timestamps[:0]
	b.values = b.values[:0]

	b.headerData = b.headerData[:0]
	b.timestampsData = b.timestampsData[:0]
	b.valuesData = b.valuesData[:0]
}

// CopyFrom copies src to b.
func (b *Block) CopyFrom(src *Block) {
	b.bh = src.bh
	b.nextIdx = 0
	b.timestamps = append(b.timestamps[:0], src.timestamps[src.nextIdx:]...)
	b.values = append(b.values[:0], src.values[src.nextIdx:]...)

	b.headerData = append(b.headerData[:0], src.headerData...)
	b.timestampsData = append(b.timestampsData[:0], src.timestampsData...)
	b.valuesData = append(b.valuesData[:0], src.valuesData...)
}

func getBlock() *Block {
	v := blockPool.Get()
	if v == nil {
		return &Block{}
	}
	return v.(*Block)
}

func putBlock(b *Block) {
	b.Reset()
	blockPool.Put(b)
}

var blockPool sync.Pool

func (b *Block) fixupTimestamps() {
	b.bh.MinTimestamp = b.timestamps[b.nextIdx]
	b.bh.MaxTimestamp = b.timestamps[len(b.timestamps)-1]
}

// RowsCount returns the number of rows in the block.
func (b *Block) RowsCount() int {
	return int(b.bh.RowsCount)
}

// Values returns b values.
func (b *Block) Values() []int64 {
	return b.values
}

// Timestamps returns b timestamps.
func (b *Block) Timestamps() []int64 {
	return b.timestamps
}

// Scale returns the decimal scale used for encoding values in the block.
func (b *Block) Scale() int16 {
	return b.bh.Scale
}

// Init initializes b with the given tsid, timestamps, values and scale.
func (b *Block) Init(tsid *TSID, timestamps, values []int64, scale int16, precisionBits uint8) {
	b.Reset()
	b.bh.TSID = *tsid
	b.bh.Scale = scale
	b.bh.PrecisionBits = precisionBits
	b.timestamps = append(b.timestamps[:0], timestamps...)
	b.values = append(b.values[:0], values...)
}

// nextRow advances to the next row.
//
// Returns false if there are no more rows in the block.
func (b *Block) nextRow() bool {
	if b.nextIdx == len(b.values) {
		return false
	}
	b.nextIdx++
	return true
}

// assertUnmarshaled makes sure the block is unmarshaled.
func (b *Block) assertUnmarshaled() {
	if len(b.valuesData) > 0 {
		logger.Panicf("BUG: valuesData must be empty; got %d bytes", len(b.valuesData))
	}
	if len(b.timestampsData) > 0 {
		logger.Panicf("BUG: timestampsData must be empty; got %d bytes", len(b.timestampsData))
	}
	if len(b.values) != len(b.timestamps) {
		logger.Panicf("BUG: the number of values must match the number of timestamps; got %d vs %d", len(b.values), len(b.timestamps))
	}
	if b.nextIdx > len(b.values) {
		logger.Panicf("BUG: nextIdx cannot exceed the number of values; got %d vs %d", b.nextIdx, len(b.values))
	}
}

// assertMergeable makes sure b and ib are mergeable, i.e. they have the same
// tsid and scale.
func (b *Block) assertMergeable(ib *Block) {
	if b.bh.TSID.MetricID != ib.bh.TSID.MetricID {
		logger.Panicf("BUG: unequal TSID: %q vs %q", &b.bh.TSID, &ib.bh.TSID)
	}
	if b.bh.Scale != ib.bh.Scale {
		logger.Panicf("BUG: unequal Scale: %d vs %d", b.bh.Scale, ib.bh.Scale)
	}
}

// tooBig returns true if the block is too big to be extended.
func (b *Block) tooBig() bool {
	if b.bh.RowsCount >= maxRowsPerBlock || len(b.values[b.nextIdx:]) >= maxRowsPerBlock {
		return true
	}
	if len(b.valuesData) >= maxBlockSize {
		return true
	}
	return false
}

// MarshalData marshals the block into binary representation.
func (b *Block) MarshalData(timestampsBlockOffset, valuesBlockOffset uint64) ([]byte, []byte, []byte) {
	if len(b.values) == 0 {
		// The data has been already marshaled.

		// b.valuesData and b.timestampsData may be empty for certain
		// b.bh.*MarshalType values, so don't check them.

		if b.nextIdx != 0 {
			logger.Panicf("BUG: nextIdx must be zero; got %d", b.nextIdx)
		}
		if int(b.bh.TimestampsBlockSize) != len(b.timestampsData) {
			logger.Panicf("BUG: invalid TimestampsBlockSize; got %d; expecting %d", b.bh.TimestampsBlockSize, len(b.timestampsData))
		}
		if int(b.bh.ValuesBlockSize) != len(b.valuesData) {
			logger.Panicf("BUG: invalid ValuesBlockSize; got %d; expecting %d", b.bh.ValuesBlockSize, len(b.valuesData))
		}
		if b.bh.RowsCount <= 0 {
			logger.Panicf("BUG: RowsCount must be greater than 0; got %d", b.bh.RowsCount)
		}

		// headerData must be always recreated, since it contains timestampsBlockOffset and valuesBlockOffset.
		b.bh.TimestampsBlockOffset = timestampsBlockOffset
		b.bh.ValuesBlockOffset = valuesBlockOffset
		b.headerData = b.bh.Marshal(b.headerData[:0])

		return b.headerData, b.timestampsData, b.valuesData
	}

	if b.nextIdx > len(b.values) {
		logger.Panicf("BUG: nextIdx cannot exceed values size; got %d vs %d", b.nextIdx, len(b.values))
	}

	timestamps := b.timestamps[b.nextIdx:]
	values := b.values[b.nextIdx:]
	if len(values) == 0 {
		logger.Panicf("BUG: values cannot be empty; nextIdx=%d, timestampsBlockOffset=%d, valuesBlockOffset=%d", b.nextIdx, timestampsBlockOffset, valuesBlockOffset)
	}
	if len(values) != len(timestamps) {
		logger.Panicf("BUG: the number of values must match the number of timestamps; got %d vs %d", len(values), len(timestamps))
	}

	b.valuesData, b.bh.ValuesMarshalType, b.bh.FirstValue = encoding.MarshalValues(b.valuesData[:0], values, b.bh.PrecisionBits)
	b.bh.ValuesBlockOffset = valuesBlockOffset
	b.bh.ValuesBlockSize = uint32(len(b.valuesData))
	b.values = b.values[:0]

	b.timestampsData, b.bh.TimestampsMarshalType, b.bh.MinTimestamp = encoding.MarshalTimestamps(b.timestampsData[:0], timestamps, b.bh.PrecisionBits)
	b.bh.TimestampsBlockOffset = timestampsBlockOffset
	b.bh.TimestampsBlockSize = uint32(len(b.timestampsData))
	b.bh.MaxTimestamp = timestamps[len(timestamps)-1]
	b.timestamps = b.timestamps[:0]

	b.bh.RowsCount = uint32(len(values))
	b.headerData = b.bh.Marshal(b.headerData[:0])

	b.nextIdx = 0

	return b.headerData, b.timestampsData, b.valuesData
}

// UnmarshalData unmarshals block data.
func (b *Block) UnmarshalData() error {
	// blockHeader (b.bh) must be already unmarshaled.

	if len(b.values) > 0 {
		// The data has been already unmarshaled.
		if len(b.valuesData) > 0 {
			logger.Panicf("BUG: valuesData must be empty; contains %d bytes", len(b.valuesData))
		}
		if len(b.timestampsData) > 0 {
			logger.Panicf("BUG: timestampsData must be empty; contains %d bytes", len(b.timestampsData))
		}
		return nil
	}

	if b.bh.RowsCount <= 0 {
		logger.Panicf("BUG: RowsCount must be greater than 0; got %d", b.bh.RowsCount)
	}

	var err error

	b.timestamps, err = encoding.UnmarshalTimestamps(b.timestamps[:0], b.timestampsData, b.bh.TimestampsMarshalType, b.bh.MinTimestamp, int(b.bh.RowsCount))
	if err != nil {
		return err
	}
	if b.bh.PrecisionBits < 64 {
		// Recover timestamps order after lossy compression.
		encoding.EnsureNonDecreasingSequence(b.timestamps, b.bh.MinTimestamp, b.bh.MaxTimestamp)
	}
	b.timestampsData = b.timestampsData[:0]

	b.values, err = encoding.UnmarshalValues(b.values[:0], b.valuesData, b.bh.ValuesMarshalType, b.bh.FirstValue, int(b.bh.RowsCount))
	if err != nil {
		return err
	}
	b.valuesData = b.valuesData[:0]

	if len(b.timestamps) != len(b.values) {
		logger.Panicf("BUG: timestamps and values count mismatch; got %d vs %d", len(b.timestamps), len(b.values))
	}

	b.nextIdx = 0

	return nil
}
