package blocklist

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path"
	"strconv"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	uuid "github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/atomic"

	"github.com/grafana/tempo/pkg/boundedwaitgroup"
	"github.com/grafana/tempo/tempodb/backend"
)

const (
	blockStatusLiveLabel      = "live"
	blockStatusCompactedLabel = "compacted"
)

var tracer = otel.Tracer("tempodb/blocklist")

var (
	metricBackendObjects = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "tempodb",
		Name:      "backend_objects_total",
		Help:      "Total number of objects (traces) in the backend",
	}, []string{"tenant", "status"})
	metricBackendBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "tempodb",
		Name:      "backend_bytes_total",
		Help:      "Total number of bytes in the backend",
	}, []string{"tenant", "status"})
	metricBlocklistErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tempodb",
		Name:      "blocklist_poll_errors_total",
		Help:      "Total number of times an error occurred while polling the blocklist.",
	}, []string{"tenant"})
	metricBlocklistPollDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace:                       "tempodb",
		Name:                            "blocklist_poll_duration_seconds",
		Help:                            "Records the amount of time to poll and update the blocklist.",
		Buckets:                         prometheus.DefBuckets,
		NativeHistogramBucketFactor:     1.1,
		NativeHistogramMaxBucketNumber:  100,
		NativeHistogramMinResetDuration: 1 * time.Hour,
	})
	metricBlocklistLength = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "tempodb",
		Name:      "blocklist_length",
		Help:      "Total number of blocks per tenant.",
	}, []string{"tenant"})
	metricTenantIndexErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "tempodb",
		Name:      "blocklist_tenant_index_errors_total",
		Help:      "Total number of times an error occurred while retrieving or building the tenant index.",
	}, []string{"tenant"})
	metricTenantIndexBuilder = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "tempodb",
		Name:      "blocklist_tenant_index_builder",
		Help:      "A value of 1 indicates this instance of tempodb is building the tenant index.",
	}, []string{"tenant"})
	metricTenantIndexAgeSeconds = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "tempodb",
		Name:      "blocklist_tenant_index_age_seconds",
		Help:      "Age in seconds of the last pulled tenant index.",
	}, []string{"tenant"})
)

// Config is used to configure the poller
type PollerConfig struct {
	PollConcurrency            uint
	TenantPollConcurrency      uint
	PollFallback               bool
	TenantIndexBuilders        int
	StaleTenantIndex           time.Duration
	PollJitterMs               int
	TolerateConsecutiveErrors  int
	TolerateTenantFailures     int
	EmptyTenantDeletionAge     time.Duration
	EmptyTenantDeletionEnabled bool
	SkipNoCompactBlocks        bool
}

// JobSharder is used to determine if a particular job is owned by this process
type JobSharder interface {
	// Owns is used to ask if a job, identified by a string, is owned by this process
	Owns(string) bool
}

// OwnsNothingSharder owns nothing. You do not want this developer on your team.
var OwnsNothingSharder = ownsNothingSharder{}

type ownsNothingSharder struct{}

func (ownsNothingSharder) Owns(_ string) bool {
	return false
}

const jobPrefix = "build-tenant-index-"

// Poller retrieves the blocklist
type Poller struct {
	reader    backend.Reader
	writer    backend.Writer
	compactor backend.Compactor

	cfg *PollerConfig

	sharder JobSharder
	logger  log.Logger
}

// NewPoller creates the Poller
func NewPoller(cfg *PollerConfig, sharder JobSharder, reader backend.Reader, compactor backend.Compactor, writer backend.Writer, logger log.Logger) *Poller {
	return &Poller{
		reader:    reader,
		compactor: compactor,
		writer:    writer,

		cfg:     cfg,
		sharder: sharder,
		logger:  logger,
	}
}

// Do does the doing of getting a blocklist
func (p *Poller) Do(parentCtx context.Context, previous *List) (PerTenant, PerTenantCompacted, error) {
	start := time.Now()
	defer func() {
		backend.ClearDedicatedColumns()
	}()

	parentCtx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	parentCtx, parentSpan := tracer.Start(parentCtx, "Poller.Do")
	defer parentSpan.End()

	tenants, err := p.reader.Tenants(parentCtx)
	if err != nil {
		metricBlocklistErrors.WithLabelValues("").Inc()
		return nil, nil, err
	}

	var (
		wg  = boundedwaitgroup.New(p.cfg.TenantPollConcurrency)
		mtx = sync.Mutex{}

		blocklist          = PerTenant{}
		compactedBlocklist = PerTenantCompacted{}

		tenantFailuresRemaining = atomic.NewInt32(int32(p.cfg.TolerateTenantFailures))

		link  = trace.LinkFromContext(parentCtx)
		bgCtx = context.Background()
	)

	for _, tenantID := range tenants {
		// Do not continue if we have been canceled.
		if parentCtx.Err() != nil {
			// Wait for our work to complete.
			wg.Wait()
			return nil, nil, parentCtx.Err()
		}

		// Exit early if we have exceeded our tolerance for number of failing tenants.
		if tenantFailuresRemaining.Load() < 0 {
			level.Error(p.logger).Log("msg", "exiting polling loop early because too many errors")
			break
		}

		wg.Add(1)
		go func(tenantID string) {
			defer wg.Done()

			bgCtx, bgSpan := tracer.Start(bgCtx, "Poller.Do.func")
			defer bgSpan.End()

			bgSpan.SetAttributes(attribute.String("tenant", tenantID))
			bgSpan.AddLink(link)

			var (
				consecutiveErrorsRemaining = p.cfg.TolerateConsecutiveErrors
				newBlockList               = make([]*backend.BlockMeta, 0)
				newCompactedBlockList      = make([]*backend.CompactedBlockMeta, 0)
				err                        error
			)

			for consecutiveErrorsRemaining >= 0 {
				newBlockList, newCompactedBlockList, err = p.pollTenantAndCreateIndex(bgCtx, tenantID, previous)
				if err == nil {
					break
				}

				consecutiveErrorsRemaining--
			}

			mtx.Lock()
			defer mtx.Unlock()

			if err != nil {
				level.Error(p.logger).Log("msg", "failed to poll or create index for tenant", "tenant", tenantID, "err", err)
				blocklist[tenantID] = previous.Metas(tenantID)
				compactedBlocklist[tenantID] = previous.CompactedMetas(tenantID)

				tenantFailuresRemaining.Dec()

				return
			}

			if len(newBlockList) > 0 || len(newCompactedBlockList) > 0 {
				blocklist[tenantID] = newBlockList
				compactedBlocklist[tenantID] = newCompactedBlockList

				metricBlocklistLength.WithLabelValues(tenantID).Set(float64(len(newBlockList)))

				backendMetaMetrics := sumTotalBackendMetaMetrics(newBlockList, newCompactedBlockList)
				metricBackendObjects.WithLabelValues(tenantID, blockStatusLiveLabel).Set(float64(backendMetaMetrics.blockMetaTotalObjects))
				metricBackendObjects.WithLabelValues(tenantID, blockStatusCompactedLabel).Set(float64(backendMetaMetrics.compactedBlockMetaTotalObjects))
				metricBackendBytes.WithLabelValues(tenantID, blockStatusLiveLabel).Set(float64(backendMetaMetrics.blockMetaTotalBytes))
				metricBackendBytes.WithLabelValues(tenantID, blockStatusCompactedLabel).Set(float64(backendMetaMetrics.compactedBlockMetaTotalBytes))
				return
			}
			metricBlocklistLength.DeleteLabelValues(tenantID)
			metricBackendObjects.DeleteLabelValues(tenantID)
			metricBackendObjects.DeleteLabelValues(tenantID)
			metricBackendBytes.DeleteLabelValues(tenantID)
		}(tenantID)
	}

	wg.Wait()

	if tenantFailuresRemaining.Load() < 0 {
		return nil, nil, errors.New("too many tenant failures; abandoning polling cycle")
	}

	diff := time.Since(start).Seconds()
	metricBlocklistPollDuration.Observe(diff)
	level.Info(p.logger).Log("msg", "blocklist poll complete", "seconds", diff)

	return blocklist, compactedBlocklist, nil
}

func (p *Poller) pollTenantAndCreateIndex(
	ctx context.Context,
	tenantID string,
	previous *List,
) ([]*backend.BlockMeta, []*backend.CompactedBlockMeta, error) {
	derivedCtx, span := tracer.Start(ctx, "Poller.pollTenantAndCreateIndex", trace.WithAttributes(attribute.String("tenant", tenantID)))
	defer span.End()

	// are we a tenant index builder?
	builder := p.tenantIndexBuilder(tenantID)
	span.SetAttributes(attribute.Bool("tenant_index_builder", builder))
	if !builder {
		metricTenantIndexBuilder.WithLabelValues(tenantID).Set(0)

		i, err := p.reader.TenantIndex(derivedCtx, tenantID)
		err = p.tenantIndexPollError(i, err)
		if err == nil {
			// success! return the retrieved index
			metricTenantIndexAgeSeconds.WithLabelValues(tenantID).Set(float64(time.Since(i.CreatedAt) / time.Second))
			level.Info(p.logger).Log("msg", "successfully pulled tenant index", "tenant", tenantID, "createdAt", i.CreatedAt, "metas", len(i.Meta), "compactedMetas", len(i.CompactedMeta))

			span.SetAttributes(attribute.Int("metas", len(i.Meta)))
			span.SetAttributes(attribute.Int("compactedMetas", len(i.CompactedMeta)))
			return i.Meta, i.CompactedMeta, nil
		}

		metricTenantIndexErrors.WithLabelValues(tenantID).Inc()
		span.RecordError(err)

		// there was an error, return the error if we're not supposed to fallback to polling
		if !p.cfg.PollFallback {
			return nil, nil, fmt.Errorf("failed to pull tenant index and no fallback configured: %w", err)
		}

		// polling fallback is true, log the error and continue in this method to completely poll the backend
		level.Error(p.logger).Log("msg", "failed to pull bucket index for tenant. falling back to polling", "tenant", tenantID, "err", err)
	}

	// if we're here then we have been configured to be a tenant index builder OR
	// there was a failure to pull the tenant index and we are configured to fall
	// back to polling.
	metricTenantIndexBuilder.WithLabelValues(tenantID).Set(1)
	blocklist, compactedBlocklist, err := p.pollTenantBlocks(derivedCtx, tenantID, previous)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to poll tenant blocks: %w", err)
	}

	// everything is happy, write this tenant index
	level.Info(p.logger).Log("msg", "writing tenant index", "tenant", tenantID, "metas", len(blocklist), "compactedMetas", len(compactedBlocklist))
	err = p.writer.WriteTenantIndex(ctx, tenantID, blocklist, compactedBlocklist)
	if err != nil {
		metricTenantIndexErrors.WithLabelValues(tenantID).Inc()
		level.Error(p.logger).Log("msg", "failed to write tenant index", "tenant", tenantID, "err", err)
	}

	if len(blocklist) == 0 && len(compactedBlocklist) == 0 {
		err := p.deleteTenant(ctx, tenantID)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to delete tenant: %w", err)
		}
	}

	metricTenantIndexAgeSeconds.WithLabelValues(tenantID).Set(0)

	return blocklist, compactedBlocklist, nil
}

func (p *Poller) pollTenantBlocks(
	ctx context.Context,
	tenantID string,
	previous *List,
) ([]*backend.BlockMeta, []*backend.CompactedBlockMeta, error) {
	derivedCtx, span := tracer.Start(ctx, "Poller.pollTenantBlocks")
	defer span.End()

	currentBlockIDs, currentCompactedBlockIDs, err := p.reader.Blocks(derivedCtx, tenantID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed listing tenant blocks: %w", err)
	}

	var (
		metas                 = previous.Metas(tenantID)
		compactedMetas        = previous.CompactedMetas(tenantID)
		mm                    = make(map[backend.UUID]*backend.BlockMeta, len(metas))
		cm                    = make(map[backend.UUID]*backend.CompactedBlockMeta, len(compactedMetas))
		newBlockList          = make([]*backend.BlockMeta, 0, len(currentBlockIDs))
		newCompactedBlocklist = make([]*backend.CompactedBlockMeta, 0, len(currentCompactedBlockIDs))
		unknownBlockIDs       = make(map[uuid.UUID]bool, 1000)
	)

	span.SetAttributes(attribute.Int("metas", len(metas)))
	span.SetAttributes(attribute.Int("compactedMetas", len(compactedMetas)))

	for _, i := range metas {
		mm[i.BlockID] = i
	}

	for _, i := range compactedMetas {
		cm[i.BlockID] = i
	}

	// The boolean here to track if we know the block has been compacted
	for _, blockID := range currentBlockIDs {
		// if we already have this block id in our previous list, use the existing data.
		if v, ok := mm[backend.UUID(blockID)]; ok {
			newBlockList = append(newBlockList, v)
			continue
		}
		unknownBlockIDs[blockID] = false

	}

	for _, blockID := range currentCompactedBlockIDs {
		// if we already have this block id in our previous list, use the existing data.
		if v, ok := cm[backend.UUID(blockID)]; ok {
			newCompactedBlocklist = append(newCompactedBlocklist, v)
			continue
		}

		// TODO: Review the ability  to avoid polling for compacted blocks that we
		// know about.  We need to know the compacted time, but perhaps there is
		// another way to get that, like the object creation time.

		unknownBlockIDs[blockID] = true

	}

	newM, newCm, err := p.pollUnknown(derivedCtx, unknownBlockIDs, tenantID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed reading unknown blocks: %w", err)
	}

	newBlockList = append(newBlockList, newM...)
	newCompactedBlocklist = append(newCompactedBlocklist, newCm...)

	return newBlockList, newCompactedBlocklist, nil
}

func (p *Poller) pollUnknown(
	ctx context.Context,
	unknownBlocks map[uuid.UUID]bool,
	tenantID string,
) ([]*backend.BlockMeta, []*backend.CompactedBlockMeta, error) {
	derivedCtx, span := tracer.Start(ctx, "pollUnknown", trace.WithAttributes(
		attribute.Int("unknownBlockIDs", len(unknownBlocks)),
	))
	defer span.End()

	var (
		err                   error
		errs                  []error
		mtx                   sync.Mutex
		bg                    = boundedwaitgroup.New(p.cfg.PollConcurrency)
		newBlockList          = make([]*backend.BlockMeta, 0, len(unknownBlocks))
		newCompactedBlocklist = make([]*backend.CompactedBlockMeta, 0, len(unknownBlocks))
	)

	for blockID, compacted := range unknownBlocks {
		// Avoid polling if we've already encountered an error
		mtx.Lock()
		if len(errs) > 0 {
			mtx.Unlock()
			break
		}
		mtx.Unlock()

		bg.Add(1)
		go func(id uuid.UUID, compacted bool) {
			defer bg.Done()

			if p.cfg.PollJitterMs > 0 {
				time.Sleep(time.Duration(rand.Intn(p.cfg.PollJitterMs)) * time.Millisecond)
			}

			m, cm, pollBlockErr := p.pollBlock(derivedCtx, tenantID, id, compacted)
			mtx.Lock()
			defer mtx.Unlock()
			if m != nil {
				newBlockList = append(newBlockList, m)
				return
			}

			if cm != nil {
				newCompactedBlocklist = append(newCompactedBlocklist, cm)
				return
			}

			if pollBlockErr != nil {
				errs = append(errs, pollBlockErr)
			}
		}(blockID, compacted)
	}

	bg.Wait()

	if len(errs) > 0 {
		metricTenantIndexErrors.WithLabelValues(tenantID).Inc()
		err = errors.Join(errs...)
		span.SetStatus(codes.Error, "")
		span.RecordError(err)

		return nil, nil, err
	}

	return newBlockList, newCompactedBlocklist, nil
}

func (p *Poller) pollBlock(
	ctx context.Context,
	tenantID string,
	blockID uuid.UUID,
	compacted bool,
) (*backend.BlockMeta, *backend.CompactedBlockMeta, error) {
	derivedCtx, span := tracer.Start(ctx, "Poller.pollBlock")
	defer span.End()
	var err error

	span.SetAttributes(attribute.String("tenant", tenantID))
	span.SetAttributes(attribute.String("block", blockID.String()))

	var blockMeta *backend.BlockMeta
	var compactedBlockMeta *backend.CompactedBlockMeta

	if !compacted && p.cfg.SkipNoCompactBlocks {
		noCompact, flagErr := p.reader.HasNoCompactFlag(derivedCtx, blockID, tenantID)
		if flagErr != nil {
			return nil, nil, fmt.Errorf("failed to check nocompact flag: %w", flagErr)
		}
		if noCompact {
			return nil, nil, nil
		}
	}
	if !compacted {
		blockMeta, err = p.reader.BlockMeta(derivedCtx, blockID, tenantID)
	}
	// if the normal meta doesn't exist maybe it's compacted.
	if errors.Is(err, backend.ErrDoesNotExist) || compacted {
		blockMeta = nil
		compactedBlockMeta, err = p.compactor.CompactedBlockMeta(blockID, tenantID)
	}

	// blocks in intermediate states may not have a compacted or normal block meta.
	//   this is not necessarily an error, just bail out
	if errors.Is(err, backend.ErrDoesNotExist) {
		return nil, nil, nil
	}

	if err != nil {
		return nil, nil, err
	}

	return blockMeta, compactedBlockMeta, nil
}

// tenantIndexBuilder returns true if this poller owns this tenant
func (p *Poller) tenantIndexBuilder(tenant string) bool {
	for i := 0; i < p.cfg.TenantIndexBuilders; i++ {
		job := jobPrefix + strconv.Itoa(i) + "-" + tenant
		if p.sharder.Owns(job) {
			return true
		}
	}

	return false
}

func (p *Poller) tenantIndexPollError(idx *backend.TenantIndex, err error) error {
	if err != nil {
		return err
	}

	if p.cfg.StaleTenantIndex != 0 && time.Since(idx.CreatedAt) > p.cfg.StaleTenantIndex {
		return fmt.Errorf("tenant index created at %s is stale", idx.CreatedAt)
	}

	return nil
}

// deleteTenant will delete all of a tenant's objects if there is not a tenant index present.
func (p *Poller) deleteTenant(ctx context.Context, tenantID string) error {
	// If we have not enabled empty tenant deletion, do nothing.
	if !p.cfg.EmptyTenantDeletionEnabled {
		return nil
	}

	level.Info(p.logger).Log("msg", "deleting tenant", "tenant", tenantID)

	if p.cfg.EmptyTenantDeletionAge == 0 {
		return fmt.Errorf("empty tenant deletion age must be greater than 0")
	}

	var (
		foundObjects  []string
		recentObjects int
	)
	err := p.reader.Find(ctx, backend.KeyPath{tenantID}, func(opts backend.FindMatch) {
		level.Info(p.logger).Log("msg", "checking object for deletion", "object", opts.Key, "modified", opts.Modified)

		if time.Since(opts.Modified) > p.cfg.EmptyTenantDeletionAge {
			foundObjects = append(foundObjects, opts.Key)
		} else {
			recentObjects++
		}
	})
	if err != nil {
		return err
	}

	// do nothing if there are recent objects for this tenant.
	if recentObjects > 0 {
		return nil
	}

	// do nothing if the tenant index has appeared.
	_, err = p.reader.TenantIndex(ctx, tenantID)
	// If we have any error other than that which indicates that the tenant index
	// call was made successfully, and that it does not exist, do nothing.  Only
	// proceed if we know that the index does not exist.
	if !errors.Is(err, backend.ErrDoesNotExist) {
		return nil
	}

	for _, object := range foundObjects {
		dir, name := path.Split(object)
		level.Info(p.logger).Log("msg", "deleting", "tenant", tenantID, "object", object)
		err = p.writer.Delete(ctx, name, backend.KeyPath{dir})
		if err != nil {
			return err
		}
	}

	return nil
}

type backendMetaMetrics struct {
	blockMetaTotalObjects          int
	compactedBlockMetaTotalObjects int
	blockMetaTotalBytes            uint64
	compactedBlockMetaTotalBytes   uint64
}

func sumTotalBackendMetaMetrics(
	blockMeta []*backend.BlockMeta,
	compactedBlockMeta []*backend.CompactedBlockMeta,
) backendMetaMetrics {
	var sumTotalObjectsBM int
	var sumTotalObjectsCBM int
	var sumTotalBytesBM uint64
	var sumTotalBytesCBM uint64

	for _, bm := range blockMeta {
		sumTotalObjectsBM += int(bm.TotalObjects)
		sumTotalBytesBM += bm.Size_
	}

	for _, cbm := range compactedBlockMeta {
		sumTotalObjectsCBM += int(cbm.TotalObjects)
		sumTotalBytesCBM += cbm.Size_
	}

	return backendMetaMetrics{
		blockMetaTotalObjects:          sumTotalObjectsBM,
		compactedBlockMetaTotalObjects: sumTotalObjectsCBM,
		blockMetaTotalBytes:            sumTotalBytesBM,
		compactedBlockMetaTotalBytes:   sumTotalBytesCBM,
	}
}
