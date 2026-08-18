package usagestats

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	bolt "go.etcd.io/bbolt"
)

var (
	metaBucket              = []byte("meta")
	hoursBucket             = []byte("hours")
	requestsBucket          = []byte("requests")
	schemaKey               = []byte("schema_version")
	sinceKey                = []byte("since_unix_nano")
	lastUsedKey             = []byte("last_used_unix_nano")
	requestSequenceKey      = []byte("request_sequence")
	modelPricesKey          = []byte("model_prices")
	modelPriceRevisionKey   = []byte("model_price_revision")
	modelPriceSettingsKey   = []byte("model_price_sync_settings")
	modelPriceLastSyncKey   = []byte("model_price_last_sync")
	dashboardPreferencesKey = []byte("dashboard_preferences")
	cryptoKeyIDKey          = []byte("crypto_key_id")
	apiKeyHashVersionKey    = []byte("api_key_hash_version")
	apiKeyGenerationsKey    = []byte("api_key_crypto_generations")
	apiKeyNextGenerationKey = []byte("api_key_next_generation")
	apiKeyLabelsKey         = []byte("api_key_labels")
)

const persistenceSchemaVersion uint64 = 9

const (
	maxAPIKeyLabels     = 10_000
	maxAPIKeyLabelRunes = 120
)

type recordCommand struct {
	usage normalizedUsage
	resp  chan error
}

type statsQueryMode uint8

const (
	statsQueryFull statsQueryMode = iota
	statsQueryInitial
	statsQueryTrend
	statsQueryGroups
)

type queryCommand struct {
	queryRange usageRange
	filter     usageFilter
	mode       statsQueryMode
	resp       chan queryResult
}

type queryResult struct {
	stats   StatsResponse
	initial InitialStatsResponse
	trend   StatsTrendResponse
	groups  GroupStatsPage
	err     error
}

type requestQueryCommand struct {
	queryRange usageRange
	offset     int
	limit      int
	model      string
	filter     usageFilter
	result     string
	resp       chan requestQueryResult
}

type requestQueryResult struct {
	page RequestPage
	err  error
}

type priceQueryCommand struct{ resp chan priceQueryResult }
type priceQueryResult struct {
	response ModelPricesResponse
	err      error
}
type savePricesCommand struct {
	prices   map[string]ModelPrice
	settings *PriceSyncSettings
	resp     chan priceQueryResult
}
type syncPricesCommand struct {
	prices           map[string]ModelPrice
	settings         PriceSyncSettings
	metadata         PriceSyncMetadata
	expectedRevision uint64
	resp             chan priceQueryResult
}
type observedModelsCommand struct {
	now  time.Time
	resp chan observedModelsResult
}
type observedModelsResult struct {
	models []string
	err    error
}
type costSnapshotCommand struct {
	queryRange usageRange
	filter     usageFilter
	resp       chan costSnapshotResult
}
type costSnapshotResult struct {
	snapshot costQuerySnapshot
	err      error
}
type preferencesQueryCommand struct{ resp chan preferencesResult }
type savePreferencesCommand struct {
	preferences DashboardPreferences
	resp        chan preferencesResult
}
type preferencesResult struct {
	preferences DashboardPreferences
	err         error
}

type labelQueryCommand struct{ resp chan labelQueryResult }
type labelQueryResult struct {
	labels map[string]string
	err    error
}
type labelSetCommand struct {
	hash  string
	label string
	resp  chan error
}
type apiKeyResolveCommand struct {
	hash string
	resp chan apiKeyResolveResult
}
type apiKeyResolveResult struct {
	ref string
	err error
}

type resetCommand struct{ resp chan resetResult }
type resetResult struct {
	generation  uint64
	generations map[uint64]APIKeyCryptoGeneration
	err         error
}
type configCommand struct {
	config Config
	crypto cryptoContext
	resp   chan configResult
}
type configResult struct {
	generation  uint64
	generations map[uint64]APIKeyCryptoGeneration
	err         error
}
type closeCommand struct{ resp chan error }

const maxDatabaseBackupBytes = 64 << 20

type backupCommand struct{ resp chan backupResult }
type backupResult struct {
	data []byte
	err  error
}

type restoreCommand struct {
	backup []byte
	resp   chan restoreResult
}
type restoreResult struct {
	db          *bolt.DB
	generation  uint64
	generations map[uint64]APIKeyCryptoGeneration
	err         error
}

// limitedBuffer rejects WriteTo output once it exceeds maxDatabaseBackupBytes.
type limitedBuffer struct {
	buf []byte
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.max > 0 && len(b.buf)+len(p) > b.max {
		return 0, fmt.Errorf("database backup exceeds %d bytes", b.max)
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

type Store struct {
	db               *bolt.DB
	lease            *storeLease
	commands         chan any
	done             chan struct{}
	closeOnce        sync.Once
	stateMu          sync.RWMutex
	costMu           sync.Mutex
	costCache        map[costCacheKey]CostResponse
	costOrder        []costCacheKey
	costFlights      map[costCacheKey]*costFlight
	costScanHook     func()
	closed           bool
	closeErr         error
	cryptoMu         sync.RWMutex
	activeGeneration uint64
	generations      map[uint64]APIKeyCryptoGeneration
}

type storeActor struct {
	db                   *bolt.DB
	config               Config
	crypto               cryptoContext
	data                 map[aggregateKey]Counters
	dirty                map[aggregateKey]struct{}
	since                time.Time
	lastUsed             time.Time
	pending              int
	lastPruneAt          time.Time
	lastFlushErr         error
	pendingRequests      []RequestDetail
	nextRequestSeq       uint64
	modelPrices          map[string]ModelPrice
	priceRevision        uint64
	priceSyncSettings    PriceSyncSettings
	lastPriceSync        *PriceSyncMetadata
	costGeneration       uint64
	dashboardPreferences DashboardPreferences
	apiKeyCiphertexts    map[string]string
	apiKeyLabels         map[string]string
	activeGeneration     uint64
	generations          map[uint64]APIKeyCryptoGeneration
}

func openStore(config Config) (*Store, error) {
	crypto, err := deriveCryptoContext(config.APIKeySecret)
	if err != nil {
		return nil, err
	}
	return openStoreWithCrypto(config, crypto)
}

func openStoreWithCrypto(config Config, crypto cryptoContext) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(config.DataPath), 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	if err := recoverInterruptedRestore(config.DataPath); err != nil {
		return nil, err
	}
	db, lease, err := openStoreDatabase(config.DataPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	actor := &storeActor{
		db:                   db,
		config:               config,
		crypto:               crypto,
		data:                 make(map[aggregateKey]Counters),
		dirty:                make(map[aggregateKey]struct{}),
		dashboardPreferences: defaultDashboardPreferences(),
		apiKeyCiphertexts:    make(map[string]string),
		apiKeyLabels:         make(map[string]string),
	}
	if err := actor.initialize(); err != nil {
		_ = db.Close()
		lease.release()
		return nil, err
	}

	store := &Store{
		db:               db,
		lease:            lease,
		commands:         make(chan any, 256),
		done:             make(chan struct{}),
		costCache:        make(map[costCacheKey]CostResponse),
		costFlights:      make(map[costCacheKey]*costFlight),
		activeGeneration: actor.activeGeneration,
		generations:      cloneAPIKeyGenerations(actor.generations),
	}
	go store.run(actor)
	go lease.monitor(store)
	return store, nil
}

func (s *Store) Record(usage normalizedUsage) error {
	resp := make(chan error, 1)
	if err := s.send(recordCommand{usage: usage, resp: resp}); err != nil {
		return err
	}
	return <-resp
}

func (s *Store) Query(rangeName string) (StatsResponse, error) {
	queryRange, err := presetUsageRange(rangeName, time.Now().UTC())
	if err != nil {
		return StatsResponse{}, err
	}
	return s.queryStats(queryRange)
}

func (s *Store) queryStats(queryRange usageRange) (StatsResponse, error) {
	return s.queryStatsByFilter(queryRange, usageFilter{})
}

func (s *Store) queryStatsBySource(queryRange usageRange, source string) (StatsResponse, error) {
	return s.queryStatsByFilter(queryRange, newUsageFilter(source, ""))
}

func (s *Store) queryStatsByFilter(queryRange usageRange, filter usageFilter) (StatsResponse, error) {
	resp := make(chan queryResult, 1)
	if err := s.send(queryCommand{queryRange: queryRange, filter: filter, resp: resp}); err != nil {
		return StatsResponse{}, err
	}
	result := <-resp
	return result.stats, result.err
}

func (s *Store) queryInitialStatsByFilter(queryRange usageRange, filter usageFilter) (InitialStatsResponse, error) {
	resp := make(chan queryResult, 1)
	if err := s.send(queryCommand{queryRange: queryRange, filter: filter, mode: statsQueryInitial, resp: resp}); err != nil {
		return InitialStatsResponse{}, err
	}
	result := <-resp
	return result.initial, result.err
}

func (s *Store) queryStatsTrendByFilter(queryRange usageRange, filter usageFilter) (StatsTrendResponse, error) {
	resp := make(chan queryResult, 1)
	if err := s.send(queryCommand{queryRange: queryRange, filter: filter, mode: statsQueryTrend, resp: resp}); err != nil {
		return StatsTrendResponse{}, err
	}
	result := <-resp
	return result.trend, result.err
}

func (s *Store) queryGroupsByFilter(queryRange usageRange, filter usageFilter) (GroupStatsPage, error) {
	resp := make(chan queryResult, 1)
	if err := s.send(queryCommand{queryRange: queryRange, filter: filter, mode: statsQueryGroups, resp: resp}); err != nil {
		return GroupStatsPage{}, err
	}
	result := <-resp
	return result.groups, result.err
}

func (s *Store) QueryRequests(rangeName string, offset, limit int, model string) (RequestPage, error) {
	queryRange, err := presetUsageRange(rangeName, time.Now().UTC())
	if err != nil {
		return RequestPage{}, err
	}
	return s.queryRequestPage(queryRange, offset, limit, model)
}

func (s *Store) queryRequestPage(queryRange usageRange, offset, limit int, model string) (RequestPage, error) {
	return s.queryRequestPageByFilter(queryRange, offset, limit, model, usageFilter{}, "")
}

func (s *Store) queryRequestPageBySource(queryRange usageRange, offset, limit int, model, source, resultFilter string) (RequestPage, error) {
	return s.queryRequestPageByFilter(queryRange, offset, limit, model, newUsageFilter(source, ""), resultFilter)
}

func (s *Store) queryRequestPageByFilter(queryRange usageRange, offset, limit int, model string, filter usageFilter, resultFilter string) (RequestPage, error) {
	resp := make(chan requestQueryResult, 1)
	if err := s.send(requestQueryCommand{queryRange: queryRange, offset: offset, limit: limit, model: model, filter: filter, result: resultFilter, resp: resp}); err != nil {
		return RequestPage{}, err
	}
	result := <-resp
	return result.page, result.err
}

func (s *Store) QueryDashboardPreferences() (DashboardPreferences, error) {
	resp := make(chan preferencesResult, 1)
	if err := s.send(preferencesQueryCommand{resp: resp}); err != nil {
		return DashboardPreferences{}, err
	}
	result := <-resp
	return result.preferences, result.err
}

func (s *Store) APIKeyLabels() (map[string]string, error) {
	resp := make(chan labelQueryResult, 1)
	if err := s.send(labelQueryCommand{resp: resp}); err != nil {
		return nil, err
	}
	result := <-resp
	return result.labels, result.err
}

func (s *Store) SetAPIKeyLabel(hash, label string) error {
	resp := make(chan error, 1)
	if err := s.send(labelSetCommand{hash: hash, label: label, resp: resp}); err != nil {
		return err
	}
	return <-resp
}

func (s *Store) ResolveAPIKeyHash(hash string) (string, error) {
	resp := make(chan apiKeyResolveResult, 1)
	if err := s.send(apiKeyResolveCommand{hash: hash, resp: resp}); err != nil {
		return "", err
	}
	result := <-resp
	return result.ref, result.err
}

func (s *Store) SaveDashboardPreferences(preferences DashboardPreferences) (DashboardPreferences, error) {
	normalized, err := normalizeDashboardPreferences(preferences)
	if err != nil {
		return DashboardPreferences{}, withStatus(400, "%v", err)
	}
	resp := make(chan preferencesResult, 1)
	if err := s.send(savePreferencesCommand{preferences: normalized, resp: resp}); err != nil {
		return DashboardPreferences{}, err
	}
	result := <-resp
	return result.preferences, result.err
}

func (s *Store) QueryModelPrices() (map[string]ModelPrice, error) {
	response, err := s.QueryPriceBook()
	return response.Prices, err
}

func (s *Store) QueryPriceBook() (ModelPricesResponse, error) {
	resp := make(chan priceQueryResult, 1)
	if err := s.send(priceQueryCommand{resp: resp}); err != nil {
		return ModelPricesResponse{}, err
	}
	result := <-resp
	return result.response, result.err
}

func (s *Store) SaveModelPrices(prices map[string]ModelPrice) (map[string]ModelPrice, error) {
	response, err := s.SavePriceBook(prices, nil)
	return response.Prices, err
}

func (s *Store) SavePriceBook(prices map[string]ModelPrice, settings *PriceSyncSettings) (ModelPricesResponse, error) {
	normalized, err := normalizeModelPrices(cloneModelPrices(prices))
	if err != nil {
		return ModelPricesResponse{}, withStatus(400, "%v", err)
	}
	if settings != nil {
		normalizedSettings, err := normalizePriceSyncSettings(*settings)
		if err != nil {
			return ModelPricesResponse{}, withStatus(400, "%v", err)
		}
		settings = &normalizedSettings
	}
	resp := make(chan priceQueryResult, 1)
	if err := s.send(savePricesCommand{prices: normalized, settings: settings, resp: resp}); err != nil {
		return ModelPricesResponse{}, err
	}
	result := <-resp
	return result.response, result.err
}

func (s *Store) ApplyModelPriceSync(prices map[string]ModelPrice, settings PriceSyncSettings, metadata PriceSyncMetadata, expectedRevision uint64) (ModelPricesResponse, error) {
	normalized, err := normalizeModelPrices(prices)
	if err != nil {
		return ModelPricesResponse{}, withStatus(400, "%v", err)
	}
	normalizedSettings, err := normalizePriceSyncSettings(settings)
	if err != nil {
		return ModelPricesResponse{}, withStatus(400, "%v", err)
	}
	resp := make(chan priceQueryResult, 1)
	if err := s.send(syncPricesCommand{prices: normalized, settings: normalizedSettings, metadata: metadata, expectedRevision: expectedRevision, resp: resp}); err != nil {
		return ModelPricesResponse{}, err
	}
	result := <-resp
	return result.response, result.err
}

func (s *Store) ObservedModels() ([]string, error) {
	resp := make(chan observedModelsResult, 1)
	if err := s.send(observedModelsCommand{now: time.Now().UTC(), resp: resp}); err != nil {
		return nil, err
	}
	result := <-resp
	return result.models, result.err
}

func (s *Store) Reset() error {
	resp := make(chan resetResult, 1)
	if err := s.send(resetCommand{resp: resp}); err != nil {
		return err
	}
	result := <-resp
	if result.err != nil {
		return result.err
	}
	s.cryptoMu.Lock()
	s.activeGeneration = result.generation
	s.generations = cloneAPIKeyGenerations(result.generations)
	s.cryptoMu.Unlock()
	return nil
}

func (s *Store) Reconfigure(config Config) error {
	crypto, err := deriveCryptoContext(config.APIKeySecret)
	if err != nil {
		return err
	}
	return s.ReconfigureWithCrypto(config, crypto)
}

func (s *Store) ReconfigureWithCrypto(config Config, crypto cryptoContext) error {
	resp := make(chan configResult, 1)
	if err := s.send(configCommand{config: config, crypto: crypto, resp: resp}); err != nil {
		return err
	}
	result := <-resp
	if result.err != nil {
		return result.err
	}
	s.cryptoMu.Lock()
	s.activeGeneration = result.generation
	s.generations = cloneAPIKeyGenerations(result.generations)
	s.cryptoMu.Unlock()
	return nil
}

func (s *Store) APIKeyCryptoState() (uint64, map[uint64]APIKeyCryptoGeneration) {
	s.cryptoMu.RLock()
	defer s.cryptoMu.RUnlock()
	return s.activeGeneration, cloneAPIKeyGenerations(s.generations)
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		resp := make(chan error, 1)
		s.stateMu.Lock()
		if s.closed {
			s.stateMu.Unlock()
			return
		}
		s.closed = true
		s.commands <- closeCommand{resp: resp}
		s.stateMu.Unlock()
		s.closeErr = <-resp
		<-s.done
		if s.lease != nil {
			s.lease.release()
		}
	})
	return s.closeErr
}

func (s *Store) Backup() ([]byte, error) {
	resp := make(chan backupResult, 1)
	if err := s.send(backupCommand{resp: resp}); err != nil {
		return nil, err
	}
	result := <-resp
	return result.data, result.err
}

// RestoreBackup replaces the live database with a previously exported backup while
// keeping the store actor and handover lease alive. It locks stateMu directly so
// concurrent cost scans cannot observe a half-swapped *bolt.DB handle.
func (s *Store) RestoreBackup(backup []byte) error {
	if len(backup) == 0 {
		return withStatus(400, "backup body must not be empty")
	}
	if len(backup) > maxDatabaseBackupBytes {
		return withStatus(413, "backup body exceeds %d bytes", maxDatabaseBackupBytes)
	}

	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.closed {
		return errors.New("store is closed")
	}

	resp := make(chan restoreResult, 1)
	s.commands <- restoreCommand{backup: append([]byte(nil), backup...), resp: resp}
	result := <-resp
	if result.db != nil {
		s.db = result.db
		s.cryptoMu.Lock()
		s.activeGeneration = result.generation
		s.generations = cloneAPIKeyGenerations(result.generations)
		s.cryptoMu.Unlock()
		s.costMu.Lock()
		s.costCache = make(map[costCacheKey]CostResponse)
		s.costOrder = nil
		s.costFlights = make(map[costCacheKey]*costFlight)
		s.costMu.Unlock()
	}
	return result.err
}

func (s *Store) send(command any) error {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	if s.closed {
		return errors.New("store is closed")
	}
	s.commands <- command
	return nil
}

func (s *Store) run(actor *storeActor) {
	ticker := time.NewTicker(actor.config.FlushInterval)
	defer ticker.Stop()
	defer close(s.done)

	for {
		select {
		case command := <-s.commands:
			switch item := command.(type) {
			case recordCommand:
				// Always accept the new usage into the dirty in-memory aggregate. A
				// previous transient flush failure must not make subsequent usage vanish.
				item.resp <- actor.record(item.usage)
			case queryCommand:
				now := time.Now().UTC()
				if requiresExactStats(item.queryRange) {
					if err := actor.flush(now, true); err != nil {
						actor.lastFlushErr = err
						item.resp <- queryResult{err: err}
						continue
					}
					stats, err := actor.queryExactStats(item.queryRange, item.filter, now)
					if err != nil {
						item.resp <- queryResult{err: err}
						continue
					}
					switch item.mode {
					case statsQueryInitial:
						item.resp <- queryResult{initial: initialStatsFromFull(stats, item.queryRange)}
					case statsQueryTrend:
						item.resp <- queryResult{trend: trendStatsFromFull(stats, item.queryRange)}
					case statsQueryGroups:
						item.resp <- queryResult{groups: GroupStatsPage{SchemaVersion: 2, GeneratedAt: stats.GeneratedAt, Range: stats.Range, Items: stats.Groups, Total: len(stats.Groups)}}
					default:
						item.resp <- queryResult{stats: stats}
					}
					continue
				}
				if err := actor.retryFailedFlush(now); err != nil {
					item.resp <- queryResult{err: err}
					continue
				}
				if err := item.queryRange.validate(); err != nil {
					item.resp <- queryResult{err: withStatus(400, "%v", err)}
					continue
				}
				switch item.mode {
				case statsQueryInitial:
					item.resp <- queryResult{initial: buildInitialStatsForRange(actor.data, actor.since, actor.lastUsed, item.queryRange, item.filter, now, actor.apiKeyCiphertexts)}
				case statsQueryTrend:
					item.resp <- queryResult{trend: buildStatsTrendForRange(actor.data, actor.since, item.queryRange, item.filter, now)}
				case statsQueryGroups:
					item.resp <- queryResult{groups: buildGroupsForRange(actor.data, item.queryRange, item.filter, now, actor.apiKeyCiphertexts)}
				default:
					stats := buildStatsForRangeWithFilter(actor.data, actor.since, actor.lastUsed, item.queryRange, item.filter, now, actor.apiKeyCiphertexts)
					item.resp <- queryResult{stats: stats}
				}
			case requestQueryCommand:
				now := time.Now().UTC()
				if err := actor.flush(now, true); err != nil {
					actor.lastFlushErr = err
					item.resp <- requestQueryResult{err: err}
					continue
				}
				page, err := actor.queryRequests(item.queryRange, item.offset, item.limit, item.model, item.filter, item.result, now)
				item.resp <- requestQueryResult{page: page, err: err}
			case preferencesQueryCommand:
				item.resp <- preferencesResult{preferences: cloneDashboardPreferences(actor.dashboardPreferences)}
			case savePreferencesCommand:
				preferences, err := actor.saveDashboardPreferences(item.preferences)
				item.resp <- preferencesResult{preferences: preferences, err: err}
			case labelQueryCommand:
				item.resp <- labelQueryResult{labels: cloneStringMap(actor.apiKeyLabels)}
			case labelSetCommand:
				item.resp <- actor.setAPIKeyLabel(item.hash, item.label)
			case apiKeyResolveCommand:
				ref, err := actor.resolveAPIKeyHash(item.hash)
				item.resp <- apiKeyResolveResult{ref: ref, err: err}
			case priceQueryCommand:
				item.resp <- priceQueryResult{response: actor.priceBookResponse()}
			case savePricesCommand:
				response, err := actor.saveModelPrices(item.prices, item.settings)
				item.resp <- priceQueryResult{response: response, err: err}
			case syncPricesCommand:
				response, err := actor.applyModelPriceSync(item.prices, item.settings, item.metadata, item.expectedRevision)
				item.resp <- priceQueryResult{response: response, err: err}
			case observedModelsCommand:
				if err := actor.flush(item.now, true); err != nil {
					actor.lastFlushErr = err
					item.resp <- observedModelsResult{err: err}
					continue
				}
				item.resp <- observedModelsResult{models: actor.observedModels(item.now)}
			case costSnapshotCommand:
				now := time.Now().UTC()
				if err := actor.flush(now, true); err != nil {
					actor.lastFlushErr = err
					item.resp <- costSnapshotResult{err: err}
					continue
				}
				err := item.queryRange.validate()
				item.resp <- costSnapshotResult{snapshot: costQuerySnapshot{
					Range:         item.queryRange.Name,
					Start:         item.queryRange.Start,
					End:           item.queryRange.End,
					GeneratedAt:   now,
					Prices:        cloneModelPrices(actor.modelPrices),
					PriceSettings: clonePriceSyncSettings(actor.priceSyncSettings),
					PriceRevision: actor.priceRevision,
					HighWater:     actor.nextRequestSeq,
					Generation:    actor.costGeneration,
					Filter:        item.filter,
				}, err: err}
			case resetCommand:
				if err := actor.retryFailedFlush(time.Now().UTC()); err != nil {
					item.resp <- resetResult{err: err}
					continue
				}
				err := actor.reset()
				item.resp <- resetResult{generation: actor.activeGeneration, generations: cloneAPIKeyGenerations(actor.generations), err: err}
			case configCommand:
				if err := actor.retryFailedFlush(time.Now().UTC()); err != nil {
					item.resp <- configResult{err: err}
					continue
				}
				err := actor.reconfigure(item.config, item.crypto)
				if err == nil {
					ticker.Reset(item.config.FlushInterval)
				}
				item.resp <- configResult{generation: actor.activeGeneration, generations: cloneAPIKeyGenerations(actor.generations), err: err}
			case backupCommand:
				now := time.Now().UTC()
				if err := actor.flush(now, true); err != nil {
					actor.lastFlushErr = err
					item.resp <- backupResult{err: err}
					continue
				}
				buffer := &limitedBuffer{max: maxDatabaseBackupBytes}
				err := actor.db.View(func(tx *bolt.Tx) error {
					_, writeErr := tx.WriteTo(buffer)
					return writeErr
				})
				if err != nil {
					item.resp <- backupResult{err: fmt.Errorf("backup database: %w", err)}
					continue
				}
				item.resp <- backupResult{data: buffer.buf}
			case restoreCommand:
				db, err := actor.restoreBackup(item.backup)
				item.resp <- restoreResult{db: db, generation: actor.activeGeneration, generations: cloneAPIKeyGenerations(actor.generations), err: err}
			case closeCommand:
				flushErr := actor.flush(time.Now().UTC(), true)
				closeErr := actor.db.Close()
				item.resp <- errors.Join(flushErr, closeErr)
				return
			}
		case now := <-ticker.C:
			actor.lastFlushErr = actor.flush(now.UTC(), false)
		}
	}
}

func (a *storeActor) initialize() error {
	now := time.Now().UTC()
	var generations map[uint64]APIKeyCryptoGeneration
	var activeGeneration uint64
	if err := a.db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists(metaBucket)
		if err != nil {
			return err
		}
		hours, err := tx.CreateBucketIfNotExists(hoursBucket)
		if err != nil {
			return err
		}
		requests, err := tx.CreateBucketIfNotExists(requestsBucket)
		if err != nil {
			return err
		}
		version := decodeUint64(meta.Get(schemaKey))
		if version > persistenceSchemaVersion {
			return fmt.Errorf("unsupported database schema version %d", version)
		}
		if err := migratePriceMetadata(meta, version); err != nil {
			return err
		}
		var since time.Time
		if raw := meta.Get(sinceKey); len(raw) == 8 {
			since = time.Unix(0, decodeInt64(raw)).UTC()
		} else {
			since = now
		}
		cutoff := retentionCutoff(a.config, now)
		cutoffTime := time.Unix(cutoff, 0).UTC()
		if cutoffTime.After(since) {
			since = cutoffTime
		}
		if err := meta.Put(sinceKey, encodeInt64(since.UnixNano())); err != nil {
			return err
		}
		if err := pruneHoursBucket(hours, cutoff); err != nil {
			return err
		}
		if err := pruneRequestsBucket(requests, time.Unix(cutoff, 0).UTC().UnixNano()); err != nil {
			return err
		}
		if err := migrateUsageSources(hours, requests, version); err != nil {
			return err
		}
		if err := migrateAPIKeyCryptoSchema(meta, hours, requests, version, now); err != nil {
			return err
		}
		loadedGenerations, loadErr := loadAPIKeyGenerations(meta)
		if loadErr != nil {
			return loadErr
		}
		generations = loadedGenerations
		activatedGeneration, activatedGenerations, activateErr := activateAPIKeyGeneration(meta, generations, a.crypto, now)
		if activateErr != nil {
			return activateErr
		}
		activeGeneration = activatedGeneration
		generations = activatedGenerations
		return meta.Put(schemaKey, encodeUint64(persistenceSchemaVersion))
	}); err != nil {
		return fmt.Errorf("initialize database: %w", err)
	}
	a.generations = generations
	a.activeGeneration = activeGeneration

	return a.reload()
}

// reload loads actor state from the current database handle without creating
// buckets, migrating schema, or mutating on-disk data.
func (a *storeActor) reload() error {
	a.data = make(map[aggregateKey]Counters)
	a.dirty = make(map[aggregateKey]struct{})
	a.pending = 0
	a.pendingRequests = nil
	a.lastFlushErr = nil
	a.lastPruneAt = time.Time{}
	a.since = time.Time{}
	a.lastUsed = time.Time{}
	a.nextRequestSeq = 0
	a.modelPrices = make(map[string]ModelPrice)
	a.priceRevision = 0
	a.priceSyncSettings = defaultPriceSyncSettings()
	a.lastPriceSync = nil
	a.dashboardPreferences = defaultDashboardPreferences()
	a.apiKeyCiphertexts = make(map[string]string)
	a.apiKeyLabels = make(map[string]string)
	a.generations = make(map[uint64]APIKeyCryptoGeneration)
	a.activeGeneration = 0

	retainedHashes := make(map[string]struct{})
	err := a.db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		hours := tx.Bucket(hoursBucket)
		requests := tx.Bucket(requestsBucket)
		if meta == nil || hours == nil || requests == nil {
			return errors.New("database buckets are missing")
		}
		if decodeUint64(meta.Get(schemaKey)) != persistenceSchemaVersion {
			return errors.New("database schema is not initialized")
		}
		generations, err := loadAPIKeyGenerations(meta)
		if err != nil {
			return err
		}
		if err := validateAPIKeyGenerationReferences(hours, requests, generations); err != nil {
			return err
		}
		a.generations = generations
		a.activeGeneration = findAPIKeyGeneration(generations, a.crypto)
		a.since = time.Unix(0, decodeInt64(meta.Get(sinceKey))).UTC()
		a.nextRequestSeq = decodeUint64(meta.Get(requestSequenceKey))
		if raw := meta.Get(modelPricesKey); len(raw) > 0 {
			var stored map[string]ModelPrice
			if err := json.Unmarshal(raw, &stored); err != nil {
				return fmt.Errorf("decode model prices: %w", err)
			}
			normalized, err := normalizeModelPrices(stored)
			if err != nil {
				return fmt.Errorf("validate model prices: %w", err)
			}
			a.modelPrices = normalized
		}
		if raw := meta.Get(dashboardPreferencesKey); len(raw) > 0 {
			var stored DashboardPreferences
			if err := json.Unmarshal(raw, &stored); err != nil {
				return fmt.Errorf("decode dashboard preferences: %w", err)
			}
			normalized, err := normalizeDashboardPreferences(stored)
			if err != nil {
				return fmt.Errorf("validate dashboard preferences: %w", err)
			}
			a.dashboardPreferences = normalized
		}
		if raw := meta.Get(apiKeyLabelsKey); len(raw) > 0 {
			var labels map[string]string
			if err := json.Unmarshal(raw, &labels); err != nil {
				return fmt.Errorf("decode API key labels: %w", err)
			}
			if err := validateAPIKeyLabels(labels); err != nil {
				return fmt.Errorf("validate API key labels: %w", err)
			}
			a.apiKeyLabels = cloneStringMap(labels)
		}
		a.priceRevision = decodeUint64(meta.Get(modelPriceRevisionKey))
		if a.priceRevision == 0 && len(a.modelPrices) > 0 {
			a.priceRevision = 1
		}
		if raw := meta.Get(modelPriceSettingsKey); len(raw) > 0 {
			var stored PriceSyncSettings
			if err := json.Unmarshal(raw, &stored); err != nil {
				return fmt.Errorf("decode model price sync settings: %w", err)
			}
			normalized, err := normalizePriceSyncSettings(stored)
			if err != nil {
				return fmt.Errorf("validate model price sync settings: %w", err)
			}
			a.priceSyncSettings = normalized
		}
		if raw := meta.Get(modelPriceLastSyncKey); len(raw) > 0 {
			var stored PriceSyncMetadata
			if err := json.Unmarshal(raw, &stored); err != nil {
				return fmt.Errorf("decode model price last sync: %w", err)
			}
			a.lastPriceSync = &stored
		}
		if raw := meta.Get(lastUsedKey); len(raw) > 0 {
			a.lastUsed = time.Unix(0, decodeInt64(raw)).UTC()
		}
		if err := hours.ForEach(func(hourKey, value []byte) error {
			if value != nil {
				return nil
			}
			hourBucket := hours.Bucket(hourKey)
			if hourBucket == nil {
				return nil
			}
			hour := decodeInt64(hourKey)
			return hourBucket.ForEach(func(dimensionKey, counterValue []byte) error {
				var dimensions Dimensions
				if err := json.Unmarshal(dimensionKey, &dimensions); err != nil {
					return fmt.Errorf("decode dimensions: %w", err)
				}
				var counters Counters
				if err := json.Unmarshal(counterValue, &counters); err != nil {
					return fmt.Errorf("decode counters: %w", err)
				}
				a.data[aggregateKey{Hour: hour, Dimensions: dimensions}] = counters
				if ref := apiKeyRef(dimensions.APIKeyGeneration, dimensions.APIKeyHash); ref != "" {
					retainedHashes[ref] = struct{}{}
				}
				return nil
			})
		}); err != nil {
			return err
		}
		return requests.ForEach(func(_, value []byte) error {
			if value == nil {
				return errors.New("request bucket contains nested bucket")
			}
			var request RequestDetail
			if err := json.Unmarshal(value, &request); err != nil {
				return fmt.Errorf("decode request detail: %w", err)
			}
			ref := apiKeyRef(request.APIKeyGeneration, request.APIKeyHash)
			if ref != "" && request.APIKey != "" {
				a.apiKeyCiphertexts[ref] = request.APIKey
			}
			if ref != "" {
				retainedHashes[ref] = struct{}{}
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	for hash := range a.apiKeyLabels {
		if _, retained := retainedHashes[hash]; !retained {
			return fmt.Errorf("API key label references data outside retention: %s", hash)
		}
	}
	return nil
}

func databaseHasAPIKeyData(hours, requests *bolt.Bucket) (bool, error) {
	if hours != nil {
		found := false
		err := hours.ForEach(func(hourKey, value []byte) error {
			if found || value != nil {
				return nil
			}
			hour := hours.Bucket(hourKey)
			if hour == nil {
				return nil
			}
			return hour.ForEach(func(key, value []byte) error {
				if value == nil {
					return nil
				}
				var dimensions Dimensions
				if err := json.Unmarshal(key, &dimensions); err != nil {
					return fmt.Errorf("decode dimensions while checking crypto identity: %w", err)
				}
				if dimensions.APIKey != "" || dimensions.APIKeyHash != "" {
					found = true
				}
				return nil
			})
		})
		if err != nil || found {
			return found, err
		}
	}
	if requests != nil {
		found := false
		err := requests.ForEach(func(_, value []byte) error {
			if found || value == nil {
				return nil
			}
			var request RequestDetail
			if err := json.Unmarshal(value, &request); err != nil {
				return fmt.Errorf("decode request while checking crypto identity: %w", err)
			}
			if request.APIKey != "" || request.APIKeyHash != "" {
				found = true
			}
			return nil
		})
		return found, err
	}
	return false, nil
}

func cloneAPIKeyGenerations(values map[uint64]APIKeyCryptoGeneration) map[uint64]APIKeyCryptoGeneration {
	cloned := make(map[uint64]APIKeyCryptoGeneration, len(values))
	for id, generation := range values {
		cloned[id] = generation
	}
	return cloned
}

func validAPIKeyKeyID(value string) bool {
	return validAPIKeyHash(value)
}

func loadAPIKeyGenerations(meta *bolt.Bucket) (map[uint64]APIKeyCryptoGeneration, error) {
	generations := make(map[uint64]APIKeyCryptoGeneration)
	identities := make(map[string]uint64)
	raw := meta.Get(apiKeyGenerationsKey)
	if len(raw) == 0 {
		return generations, nil
	}
	var stored []APIKeyCryptoGeneration
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("decode API key crypto generations: %w", err)
	}
	for _, generation := range stored {
		if generation.ID == 0 {
			return nil, errors.New("API key crypto generation ID must not be zero")
		}
		if _, duplicate := generations[generation.ID]; duplicate {
			return nil, fmt.Errorf("duplicate API key crypto generation %d", generation.ID)
		}
		if generation.IdentityMissing {
			if generation.KeyID != "" || generation.HashVersion != "" {
				return nil, fmt.Errorf("API key crypto generation %d has inconsistent missing identity", generation.ID)
			}
		} else if !validAPIKeyKeyID(generation.KeyID) || generation.HashVersion == "" {
			return nil, fmt.Errorf("API key crypto generation %d is invalid", generation.ID)
		}
		if !generation.IdentityMissing {
			identity := generation.KeyID + "\x00" + generation.HashVersion
			if existing := identities[identity]; existing != 0 {
				return nil, fmt.Errorf("API key crypto generations %d and %d have duplicate identities", existing, generation.ID)
			}
			identities[identity] = generation.ID
		}
		generations[generation.ID] = generation
	}
	return generations, nil
}

func saveAPIKeyGenerations(meta *bolt.Bucket, generations map[uint64]APIKeyCryptoGeneration) error {
	values := make([]APIKeyCryptoGeneration, 0, len(generations))
	for _, generation := range generations {
		values = append(values, generation)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].ID < values[j].ID })
	encoded, err := json.Marshal(values)
	if err != nil {
		return fmt.Errorf("encode API key crypto generations: %w", err)
	}
	if err := meta.Put(apiKeyGenerationsKey, encoded); err != nil {
		return err
	}
	return nil
}

func findAPIKeyGeneration(generations map[uint64]APIKeyCryptoGeneration, crypto cryptoContext) uint64 {
	if !crypto.enabled {
		return 0
	}
	for id, generation := range generations {
		if !generation.IdentityMissing && generation.KeyID == crypto.keyID && generation.HashVersion == apiKeyHashVersion {
			return id
		}
	}
	return 0
}

func activateAPIKeyGeneration(meta *bolt.Bucket, generations map[uint64]APIKeyCryptoGeneration, crypto cryptoContext, now time.Time) (uint64, map[uint64]APIKeyCryptoGeneration, error) {
	rawNext := meta.Get(apiKeyNextGenerationKey)
	if len(rawNext) != 0 && len(rawNext) != 8 {
		return 0, nil, errors.New("invalid API key crypto generation sequence metadata")
	}
	if !crypto.enabled {
		return 0, generations, nil
	}
	if id := findAPIKeyGeneration(generations, crypto); id != 0 {
		return id, generations, nil
	}
	next := decodeUint64(rawNext)
	if next == 0 {
		next = 1
		for id := range generations {
			if id == ^uint64(0) {
				return 0, nil, errors.New("API key crypto generation sequence exhausted")
			}
			if id >= next {
				next = id + 1
			}
		}
	} else {
		for id := range generations {
			if id >= next {
				return 0, nil, fmt.Errorf("API key next generation %d does not follow existing generation %d", next, id)
			}
		}
	}
	if next == 0 || next == ^uint64(0) {
		return 0, nil, errors.New("API key crypto generation sequence exhausted")
	}
	updated := cloneAPIKeyGenerations(generations)
	updated[next] = APIKeyCryptoGeneration{ID: next, KeyID: crypto.keyID, HashVersion: apiKeyHashVersion, CreatedAt: now.UTC()}
	if err := saveAPIKeyGenerations(meta, updated); err != nil {
		return 0, nil, err
	}
	if err := meta.Put(apiKeyNextGenerationKey, encodeUint64(next+1)); err != nil {
		return 0, nil, err
	}
	return next, updated, nil
}

func migrateAPIKeyCryptoSchema(meta, hours, requests *bolt.Bucket, version uint64, now time.Time) error {
	if version >= 8 {
		generations, err := loadAPIKeyGenerations(meta)
		if err != nil {
			return err
		}
		return validateAPIKeyGenerationReferences(hours, requests, generations)
	}
	hasData, err := databaseHasAPIKeyData(hours, requests)
	if err != nil {
		return err
	}
	keyID := string(meta.Get(cryptoKeyIDKey))
	hashVersion := string(meta.Get(apiKeyHashVersionKey))
	hasIdentity := validAPIKeyKeyID(keyID) && hashVersion != ""
	generations := make(map[uint64]APIKeyCryptoGeneration)
	legacyGeneration := uint64(0)
	if hasData || keyID != "" || hashVersion != "" {
		legacyGeneration = 1
		generation := APIKeyCryptoGeneration{ID: legacyGeneration, CreatedAt: now.UTC(), IdentityMissing: !hasIdentity}
		if hasIdentity {
			generation.KeyID = keyID
			generation.HashVersion = hashVersion
		}
		generations[legacyGeneration] = generation
	}
	if legacyGeneration != 0 {
		if err := migrateLegacyAPIKeyRecords(hours, requests, legacyGeneration); err != nil {
			return err
		}
		if raw := meta.Get(apiKeyLabelsKey); len(raw) > 0 {
			var labels map[string]string
			if err := json.Unmarshal(raw, &labels); err != nil {
				return fmt.Errorf("decode legacy API key labels: %w", err)
			}
			migrated := make(map[string]string, len(labels))
			for hash, label := range labels {
				ref := apiKeyRef(legacyGeneration, hash)
				if ref == "" {
					return fmt.Errorf("invalid legacy API key label reference %q", hash)
				}
				migrated[ref] = label
			}
			encoded, err := json.Marshal(migrated)
			if err != nil {
				return err
			}
			if err := meta.Put(apiKeyLabelsKey, encoded); err != nil {
				return err
			}
		}
	}
	if err := saveAPIKeyGenerations(meta, generations); err != nil {
		return err
	}
	next := legacyGeneration + 1
	if next == 1 {
		next = 1
	}
	if err := meta.Put(apiKeyNextGenerationKey, encodeUint64(next)); err != nil {
		return err
	}
	if err := meta.Delete(cryptoKeyIDKey); err != nil {
		return err
	}
	if err := meta.Delete(apiKeyHashVersionKey); err != nil {
		return err
	}
	return validateAPIKeyGenerationReferences(hours, requests, generations)
}

func migrateLegacyAPIKeyRecords(hours, requests *bolt.Bucket, generation uint64) error {
	if err := hours.ForEach(func(hourKey, value []byte) error {
		if value != nil {
			return nil
		}
		hour := hours.Bucket(hourKey)
		if hour == nil {
			return nil
		}
		type entry struct {
			dimensions Dimensions
			counters   Counters
		}
		entries := make([]entry, 0)
		if err := hour.ForEach(func(key, value []byte) error {
			if value == nil {
				return errors.New("hour bucket contains nested bucket")
			}
			var dimensions Dimensions
			var counters Counters
			if err := json.Unmarshal(key, &dimensions); err != nil {
				return fmt.Errorf("decode legacy dimensions: %w", err)
			}
			if err := json.Unmarshal(value, &counters); err != nil {
				return fmt.Errorf("decode legacy counters: %w", err)
			}
			if dimensions.APIKeyHash != "" && dimensions.APIKeyGeneration == 0 {
				dimensions.APIKeyGeneration = generation
			}
			entries = append(entries, entry{dimensions: dimensions, counters: counters})
			return nil
		}); err != nil {
			return err
		}
		cursor := hour.Cursor()
		for key, _ := cursor.First(); key != nil; key, _ = cursor.Next() {
			if err := cursor.Delete(); err != nil {
				return err
			}
		}
		merged := make(map[Dimensions]Counters)
		for _, item := range entries {
			combined := merged[item.dimensions]
			combined.add(item.counters)
			merged[item.dimensions] = combined
		}
		for dimensions, counters := range merged {
			key, err := json.Marshal(dimensions)
			if err != nil {
				return err
			}
			value, err := json.Marshal(counters)
			if err != nil {
				return err
			}
			if err := hour.Put(key, value); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return requests.ForEach(func(key, value []byte) error {
		if value == nil {
			return errors.New("request bucket contains nested bucket")
		}
		var request RequestDetail
		if err := json.Unmarshal(value, &request); err != nil {
			return fmt.Errorf("decode legacy request: %w", err)
		}
		if request.APIKeyHash == "" || request.APIKeyGeneration != 0 {
			return nil
		}
		request.APIKeyGeneration = generation
		encoded, err := json.Marshal(request)
		if err != nil {
			return err
		}
		return requests.Put(key, encoded)
	})
}

func validateAPIKeyGenerationReferences(hours, requests *bolt.Bucket, generations map[uint64]APIKeyCryptoGeneration) error {
	validate := func(dimensions Dimensions) error {
		if dimensions.APIKeyHash == "" {
			if dimensions.APIKeyGeneration != 0 || dimensions.APIKey != "" {
				return errors.New("API key generation or ciphertext exists without fingerprint")
			}
			return nil
		}
		if !validAPIKeyHash(dimensions.APIKeyHash) || dimensions.APIKeyGeneration == 0 {
			return errors.New("API key fingerprint has no valid crypto generation")
		}
		if _, ok := generations[dimensions.APIKeyGeneration]; !ok {
			return fmt.Errorf("API key references unknown crypto generation %d", dimensions.APIKeyGeneration)
		}
		return nil
	}
	if err := hours.ForEach(func(hourKey, value []byte) error {
		if value != nil {
			return nil
		}
		hour := hours.Bucket(hourKey)
		return hour.ForEach(func(key, value []byte) error {
			var dimensions Dimensions
			if err := json.Unmarshal(key, &dimensions); err != nil {
				return fmt.Errorf("decode dimensions while validating API key generation: %w", err)
			}
			return validate(dimensions)
		})
	}); err != nil {
		return err
	}
	return requests.ForEach(func(_, value []byte) error {
		if value == nil {
			return errors.New("request bucket contains nested bucket")
		}
		var request RequestDetail
		if err := json.Unmarshal(value, &request); err != nil {
			return fmt.Errorf("decode request while validating API key generation: %w", err)
		}
		return validate(request.Dimensions)
	})
}

func cloneStringMap(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func validAPIKeyHash(hash string) bool {
	if len(hash) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(hash)
	return err == nil && len(decoded) == 16 && strings.ToLower(hash) == hash
}

func validateAPIKeyLabel(ref, label string) error {
	if _, _, ok := parseAPIKeyRef(ref); !ok {
		return errors.New("api key ref must identify a valid crypto generation and fingerprint")
	}
	if !utf8.ValidString(label) {
		return errors.New("api key label must be valid UTF-8")
	}
	if utf8.RuneCountInString(label) > maxAPIKeyLabelRunes {
		return fmt.Errorf("api key label must not exceed %d characters", maxAPIKeyLabelRunes)
	}
	return nil
}

func validateAPIKeyLabels(labels map[string]string) error {
	if len(labels) > maxAPIKeyLabels {
		return fmt.Errorf("api key labels exceed limit %d", maxAPIKeyLabels)
	}
	for ref, label := range labels {
		if label == "" {
			return errors.New("stored api key labels must not be empty")
		}
		if err := validateAPIKeyLabel(ref, label); err != nil {
			return err
		}
	}
	return nil
}

func (a *storeActor) retainedAPIKeyRefs() map[string]struct{} {
	refs := make(map[string]struct{})
	for key := range a.data {
		if ref := apiKeyRef(key.Dimensions.APIKeyGeneration, key.Dimensions.APIKeyHash); ref != "" {
			refs[ref] = struct{}{}
		}
	}
	for _, request := range a.pendingRequests {
		if ref := apiKeyRef(request.APIKeyGeneration, request.APIKeyHash); ref != "" {
			refs[ref] = struct{}{}
		}
	}
	return refs
}

func (a *storeActor) hasRetainedAPIKeyRef(ref string) bool {
	_, exists := a.retainedAPIKeyRefs()[ref]
	return exists
}

func (a *storeActor) resolveAPIKeyHash(hash string) (string, error) {
	if !validAPIKeyHash(hash) {
		return "", withStatus(400, "api_key_hash must be 32 lowercase hexadecimal characters")
	}
	var match string
	for ref := range a.retainedAPIKeyRefs() {
		_, candidate, _ := parseAPIKeyRef(ref)
		if candidate != hash {
			continue
		}
		if match != "" && match != ref {
			return "", withStatus(400, "api_key_hash matches multiple crypto generations; use api_key_ref")
		}
		match = ref
	}
	if match == "" {
		return "", withStatus(404, "api_key_hash is not present in retained data")
	}
	return match, nil
}

func (a *storeActor) saveAPIKeyLabels(labels map[string]string) error {
	if err := validateAPIKeyLabels(labels); err != nil {
		return err
	}
	encoded, err := json.Marshal(labels)
	if err != nil {
		return fmt.Errorf("encode API key labels: %w", err)
	}
	if err := a.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		if meta == nil {
			return errors.New("metadata bucket is missing")
		}
		if len(labels) == 0 {
			return meta.Delete(apiKeyLabelsKey)
		}
		return meta.Put(apiKeyLabelsKey, encoded)
	}); err != nil {
		return fmt.Errorf("save API key labels: %w", err)
	}
	return nil
}

func (a *storeActor) setAPIKeyLabel(ref, label string) error {
	if err := validateAPIKeyLabel(ref, label); err != nil {
		return withStatus(400, "%v", err)
	}
	candidate := cloneStringMap(a.apiKeyLabels)
	if label == "" {
		delete(candidate, ref)
	} else {
		if !a.hasRetainedAPIKeyRef(ref) {
			return withStatus(404, "api key ref is not present in retained data")
		}
		if len(candidate) >= maxAPIKeyLabels {
			if _, replacing := candidate[ref]; !replacing {
				return withStatus(409, "api key label limit reached")
			}
		}
		candidate[ref] = label
	}
	if err := a.saveAPIKeyLabels(candidate); err != nil {
		return err
	}
	a.apiKeyLabels = candidate
	return nil
}

func recoverInterruptedRestore(dataPath string) error {
	rollbackPath := dataPath + ".rollback"
	rollbackInfo, err := os.Stat(rollbackPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat restore rollback database: %w", err)
	}
	if rollbackInfo.IsDir() {
		return fmt.Errorf("restore rollback path is a directory: %s", rollbackPath)
	}

	liveInfo, err := os.Stat(dataPath)
	switch {
	case errors.Is(err, os.ErrNotExist), err == nil && !liveInfo.IsDir() && liveInfo.Size() == 0:
		if err == nil {
			if removeErr := os.Remove(dataPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return fmt.Errorf("remove empty live database before rollback recovery: %w", removeErr)
			}
		}
		if renameErr := os.Rename(rollbackPath, dataPath); renameErr != nil {
			return fmt.Errorf("recover restore rollback database: %w", renameErr)
		}
		fmt.Fprintln(os.Stderr, "cap-token-usage-tracker: recovered database from interrupted restore rollback")
		return nil
	case err != nil:
		return fmt.Errorf("stat live database during restore recovery: %w", err)
	default:
		// A usable live database already exists; drop the leftover rollback file.
		if removeErr := os.Remove(rollbackPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("remove stale restore rollback database: %w", removeErr)
		}
		return nil
	}
}

func validateRestoreDatabase(path string) error {
	return validateRestoreDatabaseWithCrypto(path, cryptoContext{})
}

func validateRestoreDatabaseWithCrypto(path string, crypto cryptoContext) error {
	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: storeOpenProbeTimeout})
	if err != nil {
		return fmt.Errorf("open staged restore database: %w", err)
	}
	defer db.Close()

	return db.View(func(tx *bolt.Tx) error {
		var firstErr error
		for checkErr := range tx.Check() {
			if checkErr != nil && firstErr == nil {
				firstErr = checkErr
			}
		}
		if firstErr != nil {
			return fmt.Errorf("database integrity check failed: %w", firstErr)
		}
		meta := tx.Bucket(metaBucket)
		hours := tx.Bucket(hoursBucket)
		requests := tx.Bucket(requestsBucket)
		if meta == nil || hours == nil || requests == nil {
			return errors.New("database buckets are missing")
		}
		version := decodeUint64(meta.Get(schemaKey))
		if version != persistenceSchemaVersion {
			return fmt.Errorf("unsupported restore schema version %d", version)
		}
		generations, err := loadAPIKeyGenerations(meta)
		if err != nil {
			return err
		}
		if err := validateAPIKeyGenerationReferences(hours, requests, generations); err != nil {
			return err
		}
		if raw := meta.Get(sinceKey); len(raw) != 0 && len(raw) != 8 {
			return errors.New("invalid since metadata")
		}
		if raw := meta.Get(lastUsedKey); len(raw) != 0 && len(raw) != 8 {
			return errors.New("invalid last_used metadata")
		}
		if raw := meta.Get(requestSequenceKey); len(raw) != 0 && len(raw) != 8 {
			return errors.New("invalid request sequence metadata")
		}
		if raw := meta.Get(modelPriceRevisionKey); len(raw) != 0 && len(raw) != 8 {
			return errors.New("invalid model price revision metadata")
		}
		if raw := meta.Get(modelPricesKey); len(raw) > 0 {
			var stored map[string]ModelPrice
			if err := json.Unmarshal(raw, &stored); err != nil {
				return fmt.Errorf("decode model prices: %w", err)
			}
			if _, err := normalizeModelPrices(stored); err != nil {
				return fmt.Errorf("validate model prices: %w", err)
			}
		}
		if raw := meta.Get(modelPriceSettingsKey); len(raw) > 0 {
			var stored PriceSyncSettings
			if err := json.Unmarshal(raw, &stored); err != nil {
				return fmt.Errorf("decode model price sync settings: %w", err)
			}
			if _, err := normalizePriceSyncSettings(stored); err != nil {
				return fmt.Errorf("validate model price sync settings: %w", err)
			}
		}
		if raw := meta.Get(modelPriceLastSyncKey); len(raw) > 0 {
			var stored PriceSyncMetadata
			if err := json.Unmarshal(raw, &stored); err != nil {
				return fmt.Errorf("decode model price last sync: %w", err)
			}
		}
		if raw := meta.Get(dashboardPreferencesKey); len(raw) > 0 {
			var stored DashboardPreferences
			if err := json.Unmarshal(raw, &stored); err != nil {
				return fmt.Errorf("decode dashboard preferences: %w", err)
			}
			if _, err := normalizeDashboardPreferences(stored); err != nil {
				return fmt.Errorf("validate dashboard preferences: %w", err)
			}
		}
		if raw := meta.Get(apiKeyLabelsKey); len(raw) > 0 {
			var labels map[string]string
			if err := json.Unmarshal(raw, &labels); err != nil {
				return fmt.Errorf("decode API key labels: %w", err)
			}
			if err := validateAPIKeyLabels(labels); err != nil {
				return fmt.Errorf("validate API key labels: %w", err)
			}
			retained, _, err := retainedAPIKeyState(hours, requests)
			if err != nil {
				return err
			}
			for hash := range labels {
				if _, ok := retained[hash]; !ok {
					return fmt.Errorf("API key label references data outside retention: %s", hash)
				}
			}
		}
		return requests.ForEach(func(key, value []byte) error {
			if len(key) != 16 {
				return fmt.Errorf("invalid request key length %d", len(key))
			}
			if value == nil {
				return errors.New("request bucket contains nested bucket")
			}
			var detail RequestDetail
			if err := json.Unmarshal(value, &detail); err != nil {
				return fmt.Errorf("decode request detail: %w", err)
			}
			return nil
		})
	})
}

func migrateRestoreDatabase(path string, config Config, crypto cryptoContext) error {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: storeOpenProbeTimeout})
	if err != nil {
		return fmt.Errorf("open staged restore database for migration: %w", err)
	}
	actor := &storeActor{
		db:                   db,
		config:               config,
		crypto:               crypto,
		data:                 make(map[aggregateKey]Counters),
		dirty:                make(map[aggregateKey]struct{}),
		dashboardPreferences: defaultDashboardPreferences(),
		apiKeyCiphertexts:    make(map[string]string),
	}
	initializeErr := actor.initialize()
	syncErr := db.Sync()
	closeErr := db.Close()
	return errors.Join(initializeErr, syncErr, closeErr)
}

// syncDir fsyncs a directory so directory entries (creates/renames) become durable.
func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (a *storeActor) restoreBackup(backup []byte) (*bolt.DB, error) {
	if len(backup) == 0 {
		return a.db, withStatus(400, "backup body must not be empty")
	}
	if len(backup) > maxDatabaseBackupBytes {
		return a.db, withStatus(413, "backup body exceeds %d bytes", maxDatabaseBackupBytes)
	}
	if err := a.flush(time.Now().UTC(), true); err != nil {
		a.lastFlushErr = err
		return a.db, err
	}

	dir := filepath.Dir(a.config.DataPath)
	staged, err := os.CreateTemp(dir, "usage-restore-*.db")
	if err != nil {
		return a.db, fmt.Errorf("create staged restore database: %w", err)
	}
	stagedPath := staged.Name()
	cleanupStaged := true
	defer func() {
		if cleanupStaged {
			_ = os.Remove(stagedPath)
		}
	}()
	if _, err := staged.Write(backup); err != nil {
		_ = staged.Close()
		return a.db, fmt.Errorf("write staged restore database: %w", err)
	}
	// Persist the staged payload before validation/rename so a crash cannot leave a
	// half-written restore candidate that later renames would promote.
	if err := staged.Sync(); err != nil {
		_ = staged.Close()
		return a.db, fmt.Errorf("sync staged restore database: %w", err)
	}
	if err := staged.Close(); err != nil {
		return a.db, fmt.Errorf("close staged restore database: %w", err)
	}
	if err := migrateRestoreDatabase(stagedPath, a.config, a.crypto); err != nil {
		return a.db, withStatus(400, "invalid backup database: %v", err)
	}
	if err := validateRestoreDatabaseWithCrypto(stagedPath, a.crypto); err != nil {
		return a.db, withStatus(400, "invalid backup database: %v", err)
	}

	livePath := a.config.DataPath
	rollbackPath := livePath + ".rollback"
	_ = os.Remove(rollbackPath)

	if err := a.db.Close(); err != nil {
		return nil, fmt.Errorf("close database for restore: %w", err)
	}
	a.db = nil

	reopenLive := func(path string) (*bolt.DB, error) {
		db, openErr := bolt.Open(path, 0o600, &bolt.Options{Timeout: storeOpenProbeTimeout})
		if openErr != nil {
			return nil, openErr
		}
		a.db = db
		if reloadErr := a.reload(); reloadErr != nil {
			_ = db.Close()
			a.db = nil
			return nil, reloadErr
		}
		return db, nil
	}

	if err := os.Rename(livePath, rollbackPath); err != nil {
		db, openErr := reopenLive(livePath)
		if openErr != nil {
			return nil, errors.Join(fmt.Errorf("rename live database aside: %w", err), openErr)
		}
		return db, fmt.Errorf("rename live database aside: %w", err)
	}

	if err := os.Rename(stagedPath, livePath); err != nil {
		cleanupStaged = false
		_ = os.Remove(stagedPath)
		if renameErr := os.Rename(rollbackPath, livePath); renameErr != nil {
			return nil, errors.Join(fmt.Errorf("promote staged restore database: %w", err), renameErr)
		}
		db, openErr := reopenLive(livePath)
		if openErr != nil {
			return nil, errors.Join(fmt.Errorf("promote staged restore database: %w", err), openErr)
		}
		return db, fmt.Errorf("promote staged restore database: %w", err)
	}
	cleanupStaged = false
	// Directory fsync makes the staged→live rename durable across power loss.
	// The rename already succeeded, so treat a directory sync failure as best-effort:
	// continue opening the restored database rather than leaving the actor without a handle.
	_ = syncDir(dir)

	db, err := bolt.Open(livePath, 0o600, &bolt.Options{Timeout: storeOpenProbeTimeout})
	if err != nil {
		_ = os.Remove(livePath)
		if renameErr := os.Rename(rollbackPath, livePath); renameErr != nil {
			return nil, errors.Join(fmt.Errorf("open restored database: %w", err), renameErr)
		}
		db, openErr := reopenLive(livePath)
		if openErr != nil {
			return nil, errors.Join(fmt.Errorf("open restored database: %w", err), openErr)
		}
		return db, fmt.Errorf("open restored database: %w", err)
	}

	a.db = db
	if err := a.reload(); err != nil {
		_ = db.Close()
		a.db = nil
		_ = os.Remove(livePath)
		if renameErr := os.Rename(rollbackPath, livePath); renameErr != nil {
			return nil, errors.Join(fmt.Errorf("reload restored database: %w", err), renameErr)
		}
		db, openErr := reopenLive(livePath)
		if openErr != nil {
			return nil, errors.Join(fmt.Errorf("reload restored database: %w", err), openErr)
		}
		return db, fmt.Errorf("reload restored database: %w", err)
	}

	a.costGeneration++
	_ = os.Remove(rollbackPath)
	return db, nil
}

func migrateUsageSources(hours, requests *bolt.Bucket, version uint64) error {
	type legacyDimensions struct {
		Dimensions
		AuthProvider string `json:"auth_provider"`
		AuthAccount  string `json:"auth_account"`
	}

	type legacyRequestDetail struct {
		Sequence uint64    `json:"sequence"`
		Time     time.Time `json:"time"`
		legacyDimensions
		Counters
		Result        string         `json:"result"`
		LatencyNS     uint64         `json:"latency_ns"`
		TTFTNS        uint64         `json:"ttft_ns"`
		GenerationNS  uint64         `json:"generation_ns"`
		TPS           float64        `json:"tps"`
		CacheHit      bool           `json:"cache_hit"`
		EstimatedCost *EstimatedCost `json:"estimated_cost,omitempty"`
	}

	toCurrentRequest := func(request legacyRequestDetail) RequestDetail {
		return RequestDetail{Sequence: request.Sequence, Time: request.Time, Dimensions: request.Dimensions, Counters: request.Counters, Result: request.Result, LatencyNS: request.LatencyNS, TTFTNS: request.TTFTNS, GenerationNS: request.GenerationNS, TPS: request.TPS, CacheHit: request.CacheHit, EstimatedCost: request.EstimatedCost}
	}

	if version >= persistenceSchemaVersion {
		return nil
	}
	if hours != nil {
		var hourKeys [][]byte
		if err := hours.ForEach(func(key, value []byte) error {
			if value == nil {
				hourKeys = append(hourKeys, append([]byte(nil), key...))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, hourKey := range hourKeys {
			hour := hours.Bucket(hourKey)
			if hour == nil {
				continue
			}
			merged := make(map[Dimensions]Counters)
			var oldKeys [][]byte
			changed := false
			if err := hour.ForEach(func(key, value []byte) error {
				if value == nil {
					return nil
				}
				var dimensions legacyDimensions
				if err := json.Unmarshal(key, &dimensions); err != nil {
					return fmt.Errorf("decode dimensions for source migration: %w", err)
				}
				var counters Counters
				if err := json.Unmarshal(value, &counters); err != nil {
					return fmt.Errorf("decode counters for source migration: %w", err)
				}
				sanitized := dimensions.Dimensions
				sanitized.Source = canonicalUsageSourceWithIdentity(sanitized, dimensions.AuthProvider, dimensions.AuthAccount)
				changed = true
				combined := merged[sanitized]
				combined.add(counters)
				merged[sanitized] = combined
				oldKeys = append(oldKeys, append([]byte(nil), key...))
				return nil
			}); err != nil {
				return err
			}
			if !changed {
				continue
			}
			for _, key := range oldKeys {
				if err := hour.Delete(key); err != nil {
					return err
				}
			}
			for dimensions, counters := range merged {
				key, err := json.Marshal(dimensions)
				if err != nil {
					return err
				}
				value, err := json.Marshal(counters)
				if err != nil {
					return err
				}
				if err := hour.Put(key, value); err != nil {
					return err
				}
			}
		}
	}
	if requests != nil {
		type requestUpdate struct {
			key   []byte
			value []byte
		}
		var updates []requestUpdate
		if err := requests.ForEach(func(key, value []byte) error {
			if value == nil {
				return nil
			}
			var request legacyRequestDetail
			if err := json.Unmarshal(value, &request); err != nil {
				return fmt.Errorf("decode request for source migration: %w", err)
			}
			current := toCurrentRequest(request)
			current.Source = canonicalUsageSourceWithIdentity(current.Dimensions, request.AuthProvider, request.AuthAccount)
			encoded, err := json.Marshal(current)
			if err != nil {
				return err
			}
			updates = append(updates, requestUpdate{key: append([]byte(nil), key...), value: encoded})
			return nil
		}); err != nil {
			return err
		}
		for _, update := range updates {
			if err := requests.Put(update.key, update.value); err != nil {
				return err
			}
		}
	}
	return nil
}

func migratePriceMetadata(meta *bolt.Bucket, version uint64) error {
	prices := make(map[string]ModelPrice)
	if raw := meta.Get(modelPricesKey); len(raw) > 0 {
		var stored map[string]ModelPrice
		if err := json.Unmarshal(raw, &stored); err != nil {
			return fmt.Errorf("decode model prices: %w", err)
		}
		normalized, err := normalizeModelPrices(stored)
		if err != nil {
			return fmt.Errorf("validate model prices: %w", err)
		}
		prices = normalized
	}
	settings := defaultPriceSyncSettings()
	if raw := meta.Get(modelPriceSettingsKey); len(raw) > 0 {
		var stored PriceSyncSettings
		if err := json.Unmarshal(raw, &stored); err != nil {
			return fmt.Errorf("decode model price sync settings: %w", err)
		}
		normalized, err := normalizePriceSyncSettings(stored)
		if err != nil {
			return fmt.Errorf("validate model price sync settings: %w", err)
		}
		settings = normalized
	}
	if raw := meta.Get(modelPriceLastSyncKey); len(raw) > 0 {
		var stored PriceSyncMetadata
		if err := json.Unmarshal(raw, &stored); err != nil {
			return fmt.Errorf("decode model price last sync: %w", err)
		}
	}
	if version >= persistenceSchemaVersion {
		return nil
	}
	encodedPrices, err := json.Marshal(prices)
	if err != nil {
		return fmt.Errorf("encode migrated model prices: %w", err)
	}
	encodedSettings, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("encode migrated model price sync settings: %w", err)
	}
	if len(prices) == 0 {
		if err := meta.Delete(modelPricesKey); err != nil {
			return err
		}
	} else if err := meta.Put(modelPricesKey, encodedPrices); err != nil {
		return err
	}
	if err := meta.Put(modelPriceSettingsKey, encodedSettings); err != nil {
		return err
	}
	revision := decodeUint64(meta.Get(modelPriceRevisionKey))
	if revision == 0 && len(prices) > 0 {
		revision = 1
	}
	if err := meta.Put(modelPriceRevisionKey, encodeUint64(revision)); err != nil {
		return err
	}
	return nil
}

func (a *storeActor) record(usage normalizedUsage) error {
	usage.Dimensions = sanitizeDimensionsSource(usage.Dimensions)
	if usage.Dimensions.APIKey != "" || usage.Dimensions.APIKeyHash != "" || usage.Dimensions.APIKeyGeneration != 0 {
		if usage.Dimensions.APIKey == "" || !validAPIKeyHash(usage.Dimensions.APIKeyHash) || usage.Dimensions.APIKeyGeneration == 0 {
			return errors.New("API key ciphertext, fingerprint, and generation must be recorded together")
		}
		if _, ok := a.generations[usage.Dimensions.APIKeyGeneration]; !ok {
			return fmt.Errorf("API key references unregistered crypto generation %d", usage.Dimensions.APIKeyGeneration)
		}
	}
	aggregateDimensions := usage.Dimensions
	ciphertext := aggregateDimensions.APIKey
	aggregateDimensions.APIKey = ""
	key := aggregateKey{
		Hour:       usage.RequestedAt.UTC().Truncate(time.Minute).Unix(),
		Dimensions: aggregateDimensions,
	}
	counters := a.data[key]
	counters.add(countersForUsage(usage))
	a.data[key] = counters
	a.dirty[key] = struct{}{}
	a.nextRequestSeq++
	if a.nextRequestSeq == 0 {
		a.nextRequestSeq = 1
	}
	a.pendingRequests = append(a.pendingRequests, requestDetailForUsage(usage, a.nextRequestSeq))
	if ref := apiKeyRef(aggregateDimensions.APIKeyGeneration, aggregateDimensions.APIKeyHash); ciphertext != "" && ref != "" {
		a.apiKeyCiphertexts[ref] = ciphertext
	}
	a.pending++
	if a.lastUsed.IsZero() || usage.RequestedAt.After(a.lastUsed) {
		a.lastUsed = usage.RequestedAt
	}

	if a.lastFlushErr != nil || a.config.SyncOnRecord || a.pending >= a.config.FlushMaxRecords {
		a.lastFlushErr = a.flush(time.Now().UTC(), false)
		return a.lastFlushErr
	}
	return nil
}

func (a *storeActor) retryFailedFlush(now time.Time) error {
	if a.lastFlushErr == nil {
		return nil
	}
	a.lastFlushErr = a.flush(now, true)
	return a.lastFlushErr
}

func (a *storeActor) flush(now time.Time, force bool) error {
	shouldPrune := a.lastPruneAt.IsZero() || now.Sub(a.lastPruneAt) >= time.Hour
	if len(a.dirty) == 0 && len(a.pendingRequests) == 0 && !shouldPrune && !force {
		return nil
	}
	cutoff := retentionCutoff(a.config, now)
	nextSince := a.since
	if shouldPrune {
		cutoffTime := time.Unix(cutoff, 0).UTC()
		if cutoffTime.After(nextSince) {
			nextSince = cutoffTime
		}
	}
	var nextCiphertexts map[string]string
	var nextLabels map[string]string
	err := a.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		hours := tx.Bucket(hoursBucket)
		requests := tx.Bucket(requestsBucket)
		if meta == nil || hours == nil || requests == nil {
			return errors.New("database buckets are missing")
		}
		for key := range a.dirty {
			hourBucket, err := hours.CreateBucketIfNotExists(encodeInt64(key.Hour))
			if err != nil {
				return err
			}
			dimensions, err := json.Marshal(key.Dimensions)
			if err != nil {
				return err
			}
			counters, err := json.Marshal(a.data[key])
			if err != nil {
				return err
			}
			if err := hourBucket.Put(dimensions, counters); err != nil {
				return err
			}
		}
		for _, request := range a.pendingRequests {
			encoded, err := json.Marshal(request)
			if err != nil {
				return err
			}
			if err := requests.Put(encodeRequestKey(request.Time.UnixNano(), request.Sequence), encoded); err != nil {
				return err
			}
		}
		if err := meta.Put(sinceKey, encodeInt64(nextSince.UnixNano())); err != nil {
			return err
		}
		if !a.lastUsed.IsZero() {
			if err := meta.Put(lastUsedKey, encodeInt64(a.lastUsed.UnixNano())); err != nil {
				return err
			}
		}
		if err := meta.Put(requestSequenceKey, encodeUint64(a.nextRequestSeq)); err != nil {
			return err
		}
		if shouldPrune {
			if err := pruneHoursBucket(hours, cutoff); err != nil {
				return err
			}
			if err := pruneRequestsBucket(requests, time.Unix(cutoff, 0).UTC().UnixNano()); err != nil {
				return err
			}
			retained, ciphertexts, err := retainedAPIKeyState(hours, requests)
			if err != nil {
				return err
			}
			labels := cloneStringMap(a.apiKeyLabels)
			for hash := range labels {
				if _, ok := retained[hash]; !ok {
					delete(labels, hash)
				}
			}
			encoded, err := json.Marshal(labels)
			if err != nil {
				return err
			}
			if len(labels) == 0 {
				if err := meta.Delete(apiKeyLabelsKey); err != nil {
					return err
				}
			} else if err := meta.Put(apiKeyLabelsKey, encoded); err != nil {
				return err
			}
			nextCiphertexts = ciphertexts
			nextLabels = labels
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("flush database: %w", err)
	}

	clear(a.dirty)
	a.pendingRequests = a.pendingRequests[:0]
	a.pending = 0
	a.lastFlushErr = nil
	if shouldPrune {
		a.since = nextSince
		for key := range a.data {
			if key.Hour < cutoff {
				delete(a.data, key)
			}
		}
		a.lastPruneAt = now
		a.apiKeyCiphertexts = nextCiphertexts
		a.apiKeyLabels = nextLabels
	}
	return nil
}

func (a *storeActor) reconfigure(config Config, crypto cryptoContext) error {
	if config.DataPath != a.config.DataPath {
		return errors.New("data_path changes require opening a new store")
	}
	if err := a.flush(time.Now().UTC(), true); err != nil {
		a.lastFlushErr = err
		return err
	}
	previous := a.config
	previousCrypto := a.crypto
	previousPrune := a.lastPruneAt
	a.config = config
	a.crypto = crypto
	a.lastPruneAt = time.Time{}
	var generations map[uint64]APIKeyCryptoGeneration
	var generation uint64
	if err := a.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		if meta == nil {
			return errors.New("metadata bucket is missing")
		}
		var err error
		generation, generations, err = activateAPIKeyGeneration(meta, a.generations, crypto, time.Now().UTC())
		return err
	}); err != nil {
		a.config = previous
		a.crypto = previousCrypto
		a.lastPruneAt = previousPrune
		a.lastFlushErr = err
		return err
	}
	a.generations = generations
	a.activeGeneration = generation
	a.lastFlushErr = nil
	a.costGeneration++
	return nil
}

func (a *storeActor) saveDashboardPreferences(preferences DashboardPreferences) (DashboardPreferences, error) {
	encoded, err := json.Marshal(preferences)
	if err != nil {
		return DashboardPreferences{}, fmt.Errorf("encode dashboard preferences: %w", err)
	}
	if err := a.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		if meta == nil {
			return errors.New("metadata bucket is missing")
		}
		return meta.Put(dashboardPreferencesKey, encoded)
	}); err != nil {
		return DashboardPreferences{}, fmt.Errorf("persist dashboard preferences: %w", err)
	}
	a.dashboardPreferences = cloneDashboardPreferences(preferences)
	return cloneDashboardPreferences(a.dashboardPreferences), nil
}

func (a *storeActor) priceBookResponse() ModelPricesResponse {
	return ModelPricesResponse{
		SchemaVersion: 2,
		Revision:      a.priceRevision,
		Prices:        cloneModelPrices(a.modelPrices),
		SyncSettings:  clonePriceSyncSettings(a.priceSyncSettings),
		LastSync:      clonePriceSyncMetadata(a.lastPriceSync),
	}
}

func (a *storeActor) saveModelPrices(prices map[string]ModelPrice, settings *PriceSyncSettings) (ModelPricesResponse, error) {
	next := make(map[string]ModelPrice, len(prices))
	now := time.Now().UTC()
	for model, price := range prices {
		current, exists := a.modelPrices[model]
		if exists && current.Source == priceSourceModelsDev && sameEditableModelPrice(current, price) {
			next[model] = current
			continue
		}
		price.Source = priceSourceManual
		price.CatalogProvider = ""
		price.CatalogModel = ""
		price.UpdatedAt = now
		next[model] = price
	}
	nextSettings := a.priceSyncSettings
	if settings != nil {
		nextSettings = clonePriceSyncSettings(*settings)
	}
	nextRevision := a.priceRevision + 1
	if nextRevision == 0 {
		nextRevision = 1
	}
	if err := a.persistPriceBook(next, nextSettings, a.lastPriceSync, nextRevision); err != nil {
		return ModelPricesResponse{}, err
	}
	a.modelPrices = cloneModelPrices(next)
	a.priceSyncSettings = clonePriceSyncSettings(nextSettings)
	a.priceRevision = nextRevision
	return a.priceBookResponse(), nil
}

func (a *storeActor) applyModelPriceSync(prices map[string]ModelPrice, settings PriceSyncSettings, metadata PriceSyncMetadata, expectedRevision uint64) (ModelPricesResponse, error) {
	if a.priceRevision != expectedRevision {
		return ModelPricesResponse{}, withStatus(409, "model prices changed while synchronization was running")
	}
	next := cloneModelPrices(a.modelPrices)
	metadata.Created = 0
	metadata.Updated = 0
	metadata.SkippedManual = 0
	for model, price := range prices {
		current, exists := next[model]
		if exists && current.Source != priceSourceModelsDev {
			metadata.SkippedManual++
			continue
		}
		if exists {
			metadata.Updated++
		} else {
			metadata.Created++
		}
		next[model] = price
	}
	metadata.Source = priceSourceModelsDev
	metadata.CompletedAt = metadata.CompletedAt.UTC()
	if metadata.CompletedAt.IsZero() {
		metadata.CompletedAt = time.Now().UTC()
	}
	nextRevision := a.priceRevision + 1
	if nextRevision == 0 {
		nextRevision = 1
	}
	if err := a.persistPriceBook(next, settings, &metadata, nextRevision); err != nil {
		return ModelPricesResponse{}, err
	}
	a.modelPrices = cloneModelPrices(next)
	a.priceSyncSettings = clonePriceSyncSettings(settings)
	a.lastPriceSync = clonePriceSyncMetadata(&metadata)
	a.priceRevision = nextRevision
	return a.priceBookResponse(), nil
}

func (a *storeActor) persistPriceBook(prices map[string]ModelPrice, settings PriceSyncSettings, lastSync *PriceSyncMetadata, revision uint64) error {
	encodedPrices, err := json.Marshal(prices)
	if err != nil {
		return fmt.Errorf("encode model prices: %w", err)
	}
	encodedSettings, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("encode model price sync settings: %w", err)
	}
	var encodedLastSync []byte
	if lastSync != nil {
		encodedLastSync, err = json.Marshal(lastSync)
		if err != nil {
			return fmt.Errorf("encode model price last sync: %w", err)
		}
	}
	if err := a.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(metaBucket)
		if meta == nil {
			return errors.New("metadata bucket is missing")
		}
		if len(prices) == 0 {
			if err := meta.Delete(modelPricesKey); err != nil {
				return err
			}
		} else if err := meta.Put(modelPricesKey, encodedPrices); err != nil {
			return err
		}
		if err := meta.Put(modelPriceSettingsKey, encodedSettings); err != nil {
			return err
		}
		if lastSync == nil {
			if err := meta.Delete(modelPriceLastSyncKey); err != nil {
				return err
			}
		} else if err := meta.Put(modelPriceLastSyncKey, encodedLastSync); err != nil {
			return err
		}
		return meta.Put(modelPriceRevisionKey, encodeUint64(revision))
	}); err != nil {
		return fmt.Errorf("save model prices: %w", err)
	}
	return nil
}

func (a *storeActor) observedModels(now time.Time) []string {
	seen := make(map[string]struct{})
	cutoff := retentionCutoff(a.config, now)
	for key := range a.data {
		if key.Hour < cutoff {
			continue
		}
		model := strings.TrimSpace(key.Dimensions.Model)
		if model != "" {
			seen[model] = struct{}{}
		}
	}
	models := make([]string, 0, len(seen))
	for model := range seen {
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}

func (a *storeActor) reset() error {
	now := time.Now().UTC()
	if err := a.db.Update(func(tx *bolt.Tx) error {
		if err := tx.DeleteBucket(hoursBucket); err != nil && !errors.Is(err, bolt.ErrBucketNotFound) {
			return err
		}
		if _, err := tx.CreateBucket(hoursBucket); err != nil {
			return err
		}
		if err := tx.DeleteBucket(requestsBucket); err != nil && !errors.Is(err, bolt.ErrBucketNotFound) {
			return err
		}
		if _, err := tx.CreateBucket(requestsBucket); err != nil {
			return err
		}
		meta := tx.Bucket(metaBucket)
		if meta == nil {
			return errors.New("metadata bucket is missing")
		}
		if err := meta.Put(sinceKey, encodeInt64(now.UnixNano())); err != nil {
			return err
		}
		if err := meta.Put(requestSequenceKey, encodeUint64(0)); err != nil {
			return err
		}
		if err := meta.Delete(cryptoKeyIDKey); err != nil {
			return err
		}
		if err := meta.Delete(apiKeyHashVersionKey); err != nil {
			return err
		}
		if err := meta.Delete(apiKeyGenerationsKey); err != nil {
			return err
		}
		if err := meta.Delete(apiKeyNextGenerationKey); err != nil {
			return err
		}
		if err := meta.Delete(apiKeyLabelsKey); err != nil {
			return err
		}
		return meta.Delete(lastUsedKey)
	}); err != nil {
		return fmt.Errorf("reset database: %w", err)
	}
	a.data = make(map[aggregateKey]Counters)
	a.dirty = make(map[aggregateKey]struct{})
	a.pending = 0
	a.pendingRequests = nil
	a.nextRequestSeq = 0
	a.lastFlushErr = nil
	a.since = now
	a.lastUsed = time.Time{}
	a.apiKeyCiphertexts = make(map[string]string)
	a.apiKeyLabels = make(map[string]string)
	a.generations = make(map[uint64]APIKeyCryptoGeneration)
	a.activeGeneration = 0
	if a.crypto.enabled {
		if err := a.db.Update(func(tx *bolt.Tx) error {
			var err error
			a.activeGeneration, a.generations, err = activateAPIKeyGeneration(tx.Bucket(metaBucket), a.generations, a.crypto, now)
			return err
		}); err != nil {
			return fmt.Errorf("reset API key generation: %w", err)
		}
	}
	a.costGeneration++
	return nil
}

func (a *storeActor) queryRequests(queryRange usageRange, offset, limit int, model string, filter usageFilter, resultFilter string, now time.Time) (RequestPage, error) {
	if err := queryRange.validate(); err != nil {
		return RequestPage{}, withStatus(400, "%v", err)
	}
	if offset < 0 {
		return RequestPage{}, withStatus(400, "offset must not be negative")
	}
	if limit == 0 {
		limit = defaultRequestPageSize
	}
	if limit < 1 || limit > maxRequestPageSize {
		return RequestPage{}, withStatus(400, "limit must be between 1 and %d", maxRequestPageSize)
	}
	if resultFilter != "" && resultFilter != "success" && resultFilter != "failed" {
		return RequestPage{}, withStatus(400, "result must be success or failed")
	}

	resolver := newModelPriceResolver(a.modelPrices, a.priceSyncSettings)
	page := RequestPage{
		GeneratedAt:       now.UTC(),
		Range:             queryRange.Name,
		PriceBookRevision: a.priceRevision,
		Offset:            offset,
		Limit:             limit,
		Items:             make([]RequestDetail, 0, limit),
	}
	err := a.db.View(func(tx *bolt.Tx) error {
		requests := tx.Bucket(requestsBucket)
		if requests == nil {
			return errors.New("requests bucket is missing")
		}
		cursor := requests.Cursor()
		for key, value := cursor.Last(); key != nil; key, value = cursor.Prev() {
			if len(key) != 16 || value == nil {
				continue
			}
			requestedAt := time.Unix(0, decodeInt64(key[:8])).UTC()
			if !queryRange.End.IsZero() && !requestedAt.Before(queryRange.End) {
				continue
			}
			if !queryRange.Start.IsZero() && requestedAt.Before(queryRange.Start) {
				break
			}
			var item RequestDetail
			if err := json.Unmarshal(value, &item); err != nil {
				return fmt.Errorf("decode request detail: %w", err)
			}
			item.Dimensions = sanitizeDimensionsSource(item.Dimensions)
			itemModel := item.Model
			if itemModel == "" {
				itemModel = "未标记模型"
			}
			if model != "" && itemModel != model {
				continue
			}
			if !filter.matches(item.Dimensions) {
				continue
			}
			if (resultFilter == "success" && item.Failed) || (resultFilter == "failed" && !item.Failed) {
				continue
			}
			page.Total++
			if page.Total <= offset || len(page.Items) >= limit {
				continue
			}
			cost := estimateRequestCostWithResolver(item, resolver)
			item.EstimatedCost = &cost
			page.Items = append(page.Items, item)
		}
		return nil
	})
	if err != nil {
		return RequestPage{}, fmt.Errorf("query request details: %w", err)
	}
	return page, nil
}

func requiresExactStats(queryRange usageRange) bool {
	return queryRange.Name == "custom" &&
		(queryRange.Start.Second() != 0 || queryRange.Start.Nanosecond() != 0 ||
			queryRange.End.Second() != 0 || queryRange.End.Nanosecond() != 0)
}

func (a *storeActor) queryExactStats(queryRange usageRange, filter usageFilter, now time.Time) (StatsResponse, error) {
	if err := queryRange.validate(); err != nil {
		return StatsResponse{}, withStatus(400, "%v", err)
	}
	data := make(map[aggregateKey]Counters)
	err := a.db.View(func(tx *bolt.Tx) error {
		requests := tx.Bucket(requestsBucket)
		if requests == nil {
			return errors.New("requests bucket is missing")
		}
		cursor := requests.Cursor()
		for key, value := cursor.Last(); key != nil; key, value = cursor.Prev() {
			if len(key) != 16 || value == nil {
				continue
			}
			requestedAt := time.Unix(0, decodeInt64(key[:8])).UTC()
			if !requestedAt.Before(queryRange.End) {
				continue
			}
			if requestedAt.Before(queryRange.Start) {
				break
			}
			var item RequestDetail
			if err := json.Unmarshal(value, &item); err != nil {
				return fmt.Errorf("decode request detail: %w", err)
			}
			dimensions := sanitizeDimensionsSource(item.Dimensions)
			dimensions.APIKey = ""
			counters := item.Counters
			if item.LatencyNS > 0 {
				counters.TotalLatencyNS = item.LatencyNS
				counters.LatencySamples = 1
			}
			if item.TTFTNS > 0 {
				counters.TotalTTFTNS = item.TTFTNS
				counters.TTFTSamples = 1
			}
			bucket := aggregateKey{Hour: requestedAt.Truncate(time.Minute).Unix(), Dimensions: dimensions}
			combined := data[bucket]
			combined.add(counters)
			data[bucket] = combined
		}
		return nil
	})
	if err != nil {
		return StatsResponse{}, fmt.Errorf("query exact stats: %w", err)
	}
	return buildStatsForRangeWithFilter(data, a.since, a.lastUsed, usageRange{Name: queryRange.Name}, filter, now, a.apiKeyCiphertexts), nil
}

func retentionCutoff(config Config, now time.Time) int64 {
	return now.UTC().Add(-time.Duration(config.RetentionDays) * 24 * time.Hour).Truncate(time.Minute).Unix()
}

func pruneHoursBucket(hours *bolt.Bucket, cutoff int64) error {
	var expired [][]byte
	if err := hours.ForEach(func(key, value []byte) error {
		if value == nil && decodeInt64(key) < cutoff {
			expired = append(expired, append([]byte(nil), key...))
		}
		return nil
	}); err != nil {
		return err
	}
	for _, key := range expired {
		if err := hours.DeleteBucket(key); err != nil {
			return err
		}
	}
	return nil
}

func encodeRequestKey(unixNano int64, sequence uint64) []byte {
	result := make([]byte, 16)
	copy(result[:8], encodeInt64(unixNano))
	binary.BigEndian.PutUint64(result[8:], sequence)
	return result
}

func pruneRequestsBucket(requests *bolt.Bucket, cutoffUnixNano int64) error {
	cursor := requests.Cursor()
	for key, _ := cursor.First(); key != nil; key, _ = cursor.Next() {
		if len(key) != 16 {
			continue
		}
		if decodeInt64(key[:8]) >= cutoffUnixNano {
			break
		}
		if err := cursor.Delete(); err != nil {
			return err
		}
	}
	return nil
}

func retainedAPIKeyState(hours, requests *bolt.Bucket) (map[string]struct{}, map[string]string, error) {
	hashes := make(map[string]struct{})
	ciphertexts := make(map[string]string)
	if err := hours.ForEach(func(hourKey, value []byte) error {
		if value != nil {
			return nil
		}
		hour := hours.Bucket(hourKey)
		if hour == nil {
			return nil
		}
		return hour.ForEach(func(key, value []byte) error {
			if value == nil {
				return nil
			}
			var dimensions Dimensions
			if err := json.Unmarshal(key, &dimensions); err != nil {
				return fmt.Errorf("decode dimensions while pruning API key state: %w", err)
			}
			if ref := apiKeyRef(dimensions.APIKeyGeneration, dimensions.APIKeyHash); ref != "" {
				hashes[ref] = struct{}{}
			}
			return nil
		})
	}); err != nil {
		return nil, nil, err
	}
	if err := requests.ForEach(func(_, value []byte) error {
		if value == nil {
			return errors.New("request bucket contains nested bucket")
		}
		var request RequestDetail
		if err := json.Unmarshal(value, &request); err != nil {
			return fmt.Errorf("decode request while pruning API key state: %w", err)
		}
		if ref := apiKeyRef(request.APIKeyGeneration, request.APIKeyHash); ref != "" {
			hashes[ref] = struct{}{}
			if request.APIKey != "" {
				ciphertexts[ref] = request.APIKey
			}
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	return hashes, ciphertexts, nil
}

func encodeUint64(value uint64) []byte {
	result := make([]byte, 8)
	binary.BigEndian.PutUint64(result, value)
	return result
}

func decodeUint64(value []byte) uint64 {
	if len(value) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(value)
}

func encodeInt64(value int64) []byte {
	return encodeUint64(uint64(value) ^ (uint64(1) << 63))
}

func decodeInt64(value []byte) int64 {
	return int64(decodeUint64(value) ^ (uint64(1) << 63))
}
