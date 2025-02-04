// Package streams defines types used for the data object streams section. The
// streams section holds a list of streams present in the data object.
package streams

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"go.uber.org/atomic"

	"github.com/grafana/loki/v3/pkg/dataobj/internal/dataset"
	"github.com/grafana/loki/v3/pkg/dataobj/internal/encoding"
	"github.com/grafana/loki/v3/pkg/dataobj/internal/metadata/datasetmd"
	"github.com/grafana/loki/v3/pkg/dataobj/internal/metadata/streamsmd"
)

// A Stream is an individual stream within a data object.
type Stream struct {
	// ID to uniquely represent a stream in a data object. Valid IDs start at 1.
	// IDs are used to track streams across multiple sections in the same data
	// object.
	ID int64

	Labels       labels.Labels // Stream labels.
	MinTimestamp time.Time     // Minimum timestamp in the stream.
	MaxTimestamp time.Time     // Maximum timestamp in the stream.
	Rows         int           // Number of rows in the stream.
}

// Streams tracks information about streams in a data object.
type Streams struct {
	lastID atomic.Int64

	lookup map[uint64][]*Stream

	// orderedStreams is used for consistently iterating over the list of
	// streams. It contains streamed added in append order.
	ordered []*Stream
}

// New creates a new Streams section.
func New() *Streams {
	return &Streams{
		lookup: make(map[uint64][]*Stream),
	}
}

// Record a stream record within the Streams section. The provided timestamp is
// used to track the minimum and maximum timestamp of a stream. The number of
// calls to Record is used to track the number of rows for a stream.
//
// The stream ID of the recorded stream is returned.
func (s *Streams) Record(streamLabels labels.Labels, ts time.Time) uint64 {
	ts = ts.UTC()

	stream := s.getOrAddStream(streamLabels)
	if stream.MinTimestamp.IsZero() || ts.Before(stream.MinTimestamp) {
		stream.MinTimestamp = ts
	}
	if stream.MaxTimestamp.IsZero() || ts.After(stream.MaxTimestamp) {
		stream.MaxTimestamp = ts
	}
	stream.Rows++
	return uint64(stream.ID)
}

func (s *Streams) getOrAddStream(streamLabels labels.Labels) *Stream {
	hash := streamLabels.Hash()
	matches, ok := s.lookup[hash]
	if !ok {
		return s.addStream(hash, streamLabels)
	}

	for _, stream := range matches {
		if labels.Equal(stream.Labels, streamLabels) {
			return stream
		}
	}

	return s.addStream(hash, streamLabels)
}

func (s *Streams) addStream(hash uint64, streamLabels labels.Labels) *Stream {
	// Ensure streamLabels are sorted prior to adding to ensure consistent column
	// ordering.
	sort.Sort(streamLabels)

	newStream := &Stream{ID: s.lastID.Add(1), Labels: streamLabels}
	s.lookup[hash] = append(s.lookup[hash], newStream)
	s.ordered = append(s.ordered, newStream)
	return newStream
}

// StreamID returns the stream ID for the provided streamLabels. If the stream
// has not been recorded, StreamID returns 0.
func (s *Streams) StreamID(streamLabels labels.Labels) int64 {
	hash := streamLabels.Hash()
	matches, ok := s.lookup[hash]
	if !ok {
		return 0
	}

	for _, stream := range matches {
		if labels.Equal(stream.Labels, streamLabels) {
			return stream.ID
		}
	}

	return 0
}

// EncodeTo encodes the list of recorded streams to the provided encoder.
// pageSize controls the target sizes for pages and metadata respectively.
// EncodeTo may generate multiple sections if the list of streams is too big to
// fit into a single section.
func (s *Streams) EncodeTo(enc *encoding.Encoder, pageSize int) error {
	// TODO(rfratto): handle one section becoming too large. This can happen when
	// the number of columns is very wide. There are two approaches to handle
	// this:
	//
	// 1. Split streams into multiple sections.
	// 2. Move some columns into an aggregated column which holds multiple label
	//    keys and values.

	idBuilder, err := numberColumnBuilder(pageSize)
	if err != nil {
		return fmt.Errorf("creating ID column: %w", err)
	}
	minTimestampBuilder, err := numberColumnBuilder(pageSize)
	if err != nil {
		return fmt.Errorf("creating minimum timestamp column: %w", err)
	}
	maxTimestampBuilder, err := numberColumnBuilder(pageSize)
	if err != nil {
		return fmt.Errorf("creating maximum timestamp column: %w", err)
	}
	rowsCountBuilder, err := numberColumnBuilder(pageSize)
	if err != nil {
		return fmt.Errorf("creating rows column: %w", err)
	}

	var (
		labelBuilders      []*dataset.ColumnBuilder
		labelBuilderlookup = map[string]int{} // Name to index
	)

	getLabelColumn := func(name string) (*dataset.ColumnBuilder, error) {
		idx, ok := labelBuilderlookup[name]
		if ok {
			return labelBuilders[idx], nil
		}

		builder, err := dataset.NewColumnBuilder(name, dataset.BuilderOptions{
			PageSizeHint: pageSize,
			Value:        datasetmd.VALUE_TYPE_STRING,
			Encoding:     datasetmd.ENCODING_TYPE_PLAIN,
			Compression:  datasetmd.COMPRESSION_TYPE_ZSTD,
		})
		if err != nil {
			return nil, fmt.Errorf("creating label column: %w", err)
		}

		labelBuilders = append(labelBuilders, builder)
		labelBuilderlookup[name] = len(labelBuilders) - 1
		return builder, nil
	}

	// Populate our column builders.
	for i, stream := range s.ordered {
		// Append only fails if the rows are out-of-order, which can't happen here.
		_ = idBuilder.Append(i, dataset.Int64Value(stream.ID))
		_ = minTimestampBuilder.Append(i, dataset.Int64Value(stream.MinTimestamp.UnixNano()))
		_ = maxTimestampBuilder.Append(i, dataset.Int64Value(stream.MaxTimestamp.UnixNano()))
		_ = rowsCountBuilder.Append(i, dataset.Int64Value(int64(stream.Rows)))

		for _, label := range stream.Labels {
			builder, err := getLabelColumn(label.Name)
			if err != nil {
				return fmt.Errorf("getting label column: %w", err)
			}
			_ = builder.Append(i, dataset.StringValue(label.Value))
		}
	}

	// Encode our builders to sections. We ignore errors after enc.OpenStreams
	// (which may fail due to a caller) since we guarantee correct usage of the
	// encoding API.
	streamsEnc, err := enc.OpenStreams()
	if err != nil {
		return fmt.Errorf("opening streams section: %w", err)
	}
	defer func() {
		// Discard on defer for safety. This will return an error if we
		// successfully committed.
		_ = streamsEnc.Discard()
	}()

	{
		var errs []error
		errs = append(errs, encodeColumn(streamsEnc, streamsmd.COLUMN_TYPE_STREAM_ID, idBuilder))
		errs = append(errs, encodeColumn(streamsEnc, streamsmd.COLUMN_TYPE_MIN_TIMESTAMP, minTimestampBuilder))
		errs = append(errs, encodeColumn(streamsEnc, streamsmd.COLUMN_TYPE_MAX_TIMESTAMP, maxTimestampBuilder))
		errs = append(errs, encodeColumn(streamsEnc, streamsmd.COLUMN_TYPE_ROWS, rowsCountBuilder))
		if err := errors.Join(errs...); err != nil {
			return fmt.Errorf("encoding columns: %w", err)
		}
	}

	for _, labelBuilder := range labelBuilders {
		// For consistency we'll make sure each label builder has the same number
		// of rows as the other columns (which is the number of streams).
		labelBuilder.Backfill(len(s.ordered))

		err := encodeColumn(streamsEnc, streamsmd.COLUMN_TYPE_LABEL, labelBuilder)
		if err != nil {
			return fmt.Errorf("encoding label column: %w", err)
		}
	}

	return streamsEnc.Commit()
}

func numberColumnBuilder(pageSize int) (*dataset.ColumnBuilder, error) {
	return dataset.NewColumnBuilder("", dataset.BuilderOptions{
		PageSizeHint: pageSize,
		Value:        datasetmd.VALUE_TYPE_INT64,
		Encoding:     datasetmd.ENCODING_TYPE_DELTA,
		Compression:  datasetmd.COMPRESSION_TYPE_NONE,
	})
}

func encodeColumn(enc *encoding.StreamsEncoder, columnType streamsmd.ColumnType, builder *dataset.ColumnBuilder) error {
	column, err := builder.Flush()
	if err != nil {
		return fmt.Errorf("flushing %s column: %w", columnType, err)
	}

	columnEnc, err := enc.OpenColumn(columnType, &column.Info)
	if err != nil {
		return fmt.Errorf("opening %s column encoder: %w", columnType, err)
	}
	defer func() {
		// Discard on defer for safety. This will return an error if we
		// successfully committed.
		_ = columnEnc.Discard()
	}()

	for _, page := range column.Pages {
		err := columnEnc.AppendPage(page)
		if err != nil {
			return fmt.Errorf("appending %s page: %w", columnType, err)
		}
	}

	return columnEnc.Commit()
}

// Reset resets all state, allowing Streams to be reused.
func (s *Streams) Reset() {
	s.lastID.Store(0)
	clear(s.lookup)
	s.ordered = s.ordered[:0]
}
