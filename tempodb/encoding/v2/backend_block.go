package v2

import (
	"bytes"
	"context"
	"fmt"

	"github.com/google/uuid"
	willf_bloom "github.com/willf/bloom"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/grafana/tempo/pkg/cache"
	"github.com/grafana/tempo/pkg/model"
	"github.com/grafana/tempo/pkg/tempopb"
	"github.com/grafana/tempo/pkg/traceql"
	"github.com/grafana/tempo/tempodb/backend"
	"github.com/grafana/tempo/tempodb/encoding/common"
)

// BackendBlock represents a block already in the backend.
type BackendBlock struct {
	meta   *backend.BlockMeta
	reader backend.Reader
}

var (
	_ common.Finder   = (*BackendBlock)(nil)
	_ common.Searcher = (*BackendBlock)(nil)
)

// NewBackendBlock returns a BackendBlock for the given backend.BlockMeta
func NewBackendBlock(meta *backend.BlockMeta, r backend.Reader) (*BackendBlock, error) {
	return &BackendBlock{
		meta:   meta,
		reader: r,
	}, nil
}

// Find searches a block for the ID and returns an object if found.
func (b *BackendBlock) find(ctx context.Context, id common.ID) ([]byte, error) {
	var err error
	ctx, span := tracer.Start(ctx, "BackendBlock.find")
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "")
		}
		span.End()
	}()

	span.SetAttributes(attribute.String("block", b.meta.BlockID.String()))

	shardKey := common.ShardKeyForTraceID(id, int(b.meta.BloomShardCount))
	blockID := b.meta.BlockID
	tenantID := b.meta.TenantID

	nameBloom := common.BloomName(shardKey)
	bloomBytes, err := b.reader.Read(ctx, nameBloom, (uuid.UUID)(blockID), tenantID, &backend.CacheInfo{
		Meta: b.meta,
		Role: cache.RoleBloom,
	})
	if err != nil {
		return nil, fmt.Errorf("error retrieving bloom %s (%s, %s): %w", nameBloom, b.meta.TenantID, b.meta.BlockID, err)
	}

	filter := &willf_bloom.BloomFilter{}
	_, err = filter.ReadFrom(bytes.NewReader(bloomBytes))
	if err != nil {
		return nil, fmt.Errorf("error parsing bloom (%s, %s): %w", b.meta.TenantID, b.meta.BlockID, err)
	}

	if !filter.Test(id) {
		return nil, nil
	}

	indexReaderAt := backend.NewContextReader(b.meta, common.NameIndex, b.reader)
	indexReader, err := NewIndexReader(indexReaderAt, int(b.meta.IndexPageSize), int(b.meta.TotalRecords))
	if err != nil {
		return nil, fmt.Errorf("error building index reader (%s, %s): %w", b.meta.TenantID, b.meta.BlockID, err)
	}

	ra := backend.NewContextReader(b.meta, common.NameObjects, b.reader)
	dataReader, err := NewDataReader(ra, b.meta.Encoding)
	if err != nil {
		return nil, fmt.Errorf("error building page reader (%s, %s): %w", b.meta.TenantID, b.meta.BlockID, err)
	}
	defer dataReader.Close()

	// passing nil for objectCombiner here.  this is fine b/c a backend block should never have dupes
	finder := newPagedFinder(indexReader, dataReader, nil, NewObjectReaderWriter(), b.meta.DataEncoding)
	objectBytes, err := finder.Find(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("error using pageFinder (%s, %s): %w", b.meta.TenantID, b.meta.BlockID, err)
	}

	return objectBytes, nil
}

// Iterator returns an Iterator that iterates over the objects in the block from the backend
func (b *BackendBlock) Iterator(chunkSizeBytes uint32) (BytesIterator, error) {
	// read index
	ra := backend.NewContextReader(b.meta, common.NameObjects, b.reader)
	dataReader, err := NewDataReader(ra, b.meta.Encoding)
	if err != nil {
		return nil, fmt.Errorf("failed to create dataReader (%s, %s): %w", b.meta.TenantID, b.meta.BlockID, err)
	}

	reader, err := b.NewIndexReader()
	if err != nil {
		return nil, err
	}

	return newPagedIterator(chunkSizeBytes, reader, dataReader, NewObjectReaderWriter()), nil
}

func (b *BackendBlock) NewIndexReader() (IndexReader, error) {
	indexReaderAt := backend.NewContextReader(b.meta, common.NameIndex, b.reader)
	reader, err := NewIndexReader(indexReaderAt, int(b.meta.IndexPageSize), int(b.meta.TotalRecords))
	if err != nil {
		return nil, fmt.Errorf("failed to create index reader (%s, %s): %w", b.meta.TenantID, b.meta.BlockID, err)
	}

	return reader, nil
}

func (b *BackendBlock) BlockMeta() *backend.BlockMeta {
	return b.meta
}

func (b *BackendBlock) FindTraceByID(ctx context.Context, id common.ID, _ common.SearchOptions) (*tempopb.TraceByIDResponse, error) {
	ctx, span := tracer.Start(ctx, "BackendBlock.FindTraceByID")
	defer span.End()

	obj, err := b.find(ctx, id)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		// Not found in this block
		return nil, nil
	}

	dec, err := model.NewObjectDecoder(b.meta.DataEncoding)
	if err != nil {
		return nil, err
	}

	trace, err := dec.PrepareForRead(obj)

	return &tempopb.TraceByIDResponse{
		Trace:   trace,
		Metrics: &tempopb.TraceByIDMetrics{},
	}, err
}

func (b *BackendBlock) Search(context.Context, *tempopb.SearchRequest, common.SearchOptions) (resp *tempopb.SearchResponse, err error) {
	return nil, common.ErrUnsupported
}

func (b *BackendBlock) SearchTags(context.Context, traceql.AttributeScope, common.TagsCallback, common.MetricsCallback, common.SearchOptions) error {
	return common.ErrUnsupported
}

func (b *BackendBlock) SearchTagValues(context.Context, string, common.TagValuesCallback, common.MetricsCallback, common.SearchOptions) error {
	return common.ErrUnsupported
}

func (b *BackendBlock) SearchTagValuesV2(context.Context, traceql.Attribute, common.TagValuesCallbackV2, common.MetricsCallback, common.SearchOptions) error {
	return common.ErrUnsupported
}

func (b *BackendBlock) Fetch(context.Context, traceql.FetchSpansRequest, common.SearchOptions) (traceql.FetchSpansResponse, error) {
	return traceql.FetchSpansResponse{}, common.ErrUnsupported
}

func (b *BackendBlock) FetchTagValues(context.Context, traceql.FetchTagValuesRequest, traceql.FetchTagValuesCallback, common.MetricsCallback, common.SearchOptions) error {
	return common.ErrUnsupported
}

func (b *BackendBlock) FetchTagNames(context.Context, traceql.FetchTagsRequest, traceql.FetchTagsCallback, common.MetricsCallback, common.SearchOptions) error {
	return common.ErrUnsupported
}

func (b *BackendBlock) Validate(_ context.Context) error {
	return common.ErrUnsupported
}
