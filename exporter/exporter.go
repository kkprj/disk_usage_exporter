package exporter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/dundee/disk_usage_exporter/build"
	"github.com/dundee/gdu/v5/pkg/analyze"
	"github.com/dundee/gdu/v5/pkg/fs"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
)

var (
	// 계층적 집계 메트릭
	diskUsageFileCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_disk_usage_file_count",
			Help: "Number of files in directory",
		},
		[]string{"path", "level"},
	)
	diskUsageDirectoryCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_disk_usage_directory_count",
			Help: "Number of subdirectories in directory",
		},
		[]string{"path", "level"},
	)
	
	// 디스크 사용량 메트릭
	diskUsage = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_disk_usage",
			Help: "Total disk usage by directory",
		},
		[]string{"path", "level"},
	)
	
	// 크기별 버킷 메트릭
	diskUsageSizeBucket = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_disk_usage_size_bucket",
			Help: "File count by size range",
		},
		[]string{"path", "size_range", "level"},
	)
	
	// Top-N 파일 메트릭
	diskUsageTopFiles = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_disk_usage_top_files",
			Help: "Top N largest files",
		},
		[]string{"path", "rank"},
	)
	diskUsageOthersTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_disk_usage_others_total",
			Help: "Total size of files not in top N",
		},
		[]string{"path"},
	)
	diskUsageOthersCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_disk_usage_others_count",
			Help: "Count of files not in top N",
		},
		[]string{"path"},
	)
	
	collectors = []prometheus.Collector{
		diskUsageFileCount, diskUsageDirectoryCount,
		diskUsage, diskUsageSizeBucket, diskUsageTopFiles,
		diskUsageOthersTotal, diskUsageOthersCount,
	}
)

// writeRequest represents a request to write analysis results to storage
type writeRequest struct {
	path   string
	result fs.Item
	done   chan error
}

// aggregatedStats holds aggregated statistics for a directory
type aggregatedStats struct {
	sync.Mutex
	path           string
	level          int
	totalSize      int64             // total size for by_type metric
	fileCount      int64
	directoryCount int64
	sizeBuckets    map[string]int64  // size_range -> count
	topFiles       []topFileInfo     // largest files
	othersTotal    int64
	othersCount    int64
}

// topFileInfo holds information about large files
type topFileInfo struct {
	path string
	size int64
	rank int
}

// getSizeRange returns the size range bucket for a given file size
func getSizeRange(size int64) string {
	if size == 0 {
		return "0B"
	} else if size <= 1024 {
		return "0-1KB"
	} else if size <= 1024*1024 {
		return "1KB-1MB"
	} else if size <= 100*1024*1024 {
		return "1MB-100MB"
	} else {
		return "100MB+"
	}
}

// streamingItem represents a lightweight file system item for streaming processing
type streamingItem struct {
	path     string
	size     int64
	isDir    bool
	level    int
	modTime  time.Time
}

// workUnit represents a unit of work for the worker pool
type workUnit struct {
	rootPath string
	dirPath  string
	level    int
	maxLevel int
}

// workResult represents the result of processing a work unit
type workResult struct {
	rootPath string
	stats    map[string]*aggregatedStats
	err      error
}

// streamingProcessor handles memory-efficient directory traversal
type streamingProcessor struct {
	exporter     *Exporter
	rootPath     string
	maxLevel     int
	stats        map[string]*aggregatedStats
	statsMutex   sync.RWMutex
	batchSize    int
	currentBatch []streamingItem
}

// newStreamingProcessor creates a new streaming processor
func newStreamingProcessor(exporter *Exporter, rootPath string, maxLevel int) *streamingProcessor {
	batchSize := exporter.chunkSize
	return &streamingProcessor{
		exporter:     exporter,
		rootPath:     rootPath,
		maxLevel:     maxLevel,
		stats:        make(map[string]*aggregatedStats),
		batchSize:    batchSize,
		currentBatch: make([]streamingItem, 0, batchSize),
	}
}

// processDirectoryStreaming performs memory-efficient directory analysis
func (sp *streamingProcessor) processDirectoryStreaming() error {
	log.Debugf("Starting streaming analysis for path: %s (maxLevel: %d)", sp.rootPath, sp.maxLevel)
	defer func() {
		// Force cleanup of current batch only
		// Stats will be transferred via getStreamingStats
		sp.currentBatch = nil
		runtime.GC()
		log.Debugf("Completed streaming analysis for path: %s", sp.rootPath)
	}()

	// Start recursive streaming traversal
	return sp.traverseDirectoryStreaming(sp.rootPath, 0)
}

// traverseDirectoryStreaming recursively processes directories with immediate memory cleanup
func (sp *streamingProcessor) traverseDirectoryStreaming(dirPath string, level int) error {
	// Calculate relative level for this directory
	relativeLevel := sp.calculateRelativeLevel(dirPath)
	
	// Continue scanning beyond maxLevel for files, but don't create directory metrics
	scanForFilesOnly := relativeLevel > sp.maxLevel
	if scanForFilesOnly {
		log.Tracef("Scanning beyond maxLevel for files only: %s (relative level: %d > %d)", dirPath, relativeLevel, sp.maxLevel)
	}

	log.Debugf("[Level %d] Starting streaming scan: %s", level, dirPath)

	// Check if directory should be ignored
	dirName := filepath.Base(dirPath)
	if sp.exporter.shouldDirBeIgnored(dirName, dirPath) {
		log.Tracef("Ignoring directory: %s", dirPath)
		return nil
	}

	// Read directory entries
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		log.Warnf("Failed to read directory %s: %v", dirPath, err)
		return err
	}

	log.Debugf("[Level %d] Processing %d entries in: %s", level, len(entries), dirPath)

	// Process directory entries in batches
	fileCount := 0
	dirCount := 0
	processedCount := 0

	for _, entry := range entries {
		// Get entry info
		entryPath := filepath.Join(dirPath, entry.Name())
		info, err := entry.Info()
		if err != nil {
			log.Warnf("Failed to get info for %s: %v", entryPath, err)
			continue
		}

		// Create streaming item
		item := streamingItem{
			path:    entryPath,
			size:    info.Size(),
			isDir:   entry.IsDir(),
			level:   level,
			modTime: info.ModTime(),
		}

		// Add to current batch
		sp.currentBatch = append(sp.currentBatch, item)

		// Count items by type
		if entry.IsDir() {
			dirCount++
		} else {
			fileCount++
		}
		processedCount++

		// Process batch when it reaches batchSize
		if len(sp.currentBatch) >= sp.batchSize {
			sp.processBatch()
		}
	}

	// Process remaining items in batch
	if len(sp.currentBatch) > 0 {
		sp.processBatch()
	}

	log.Debugf("[Level %d] Completed scan: %s (dirs: %d, files: %d, processed: %d)", 
		level, dirPath, dirCount, fileCount, processedCount)

	// Recursively process subdirectories only if within level limit
	// Check relative level for subdirectories to respect maxLevel constraints
	for _, entry := range entries {
		if entry.IsDir() {
			subDirPath := filepath.Join(dirPath, entry.Name())
			
			// Always traverse subdirectories to find files, even beyond maxLevel
			if !sp.exporter.shouldDirBeIgnored(entry.Name(), subDirPath) {
				err := sp.traverseDirectoryStreaming(subDirPath, level+1)
				if err != nil {
					log.Warnf("Failed to process subdirectory %s: %v", subDirPath, err)
					// Continue with other directories instead of failing completely
				}
			}
		}
	}

	return nil
}

// processBatch processes the current batch of items and immediately frees memory
func (sp *streamingProcessor) processBatch() {
	if len(sp.currentBatch) == 0 {
		return
	}

	log.Debugf("Processing batch of %d items", len(sp.currentBatch))

	// Process each item in the batch
	for _, item := range sp.currentBatch {
		sp.aggregateStreamingItem(item)
	}

	// Immediately clear the batch to free memory
	sp.currentBatch = sp.currentBatch[:0] // Reset slice but keep capacity
	runtime.GC() // Suggest garbage collection

	log.Tracef("Batch processed and memory freed")
}

// aggregateStreamingItem aggregates statistics for a single streaming item
func (sp *streamingProcessor) aggregateStreamingItem(item streamingItem) {
	// Determine aggregation path (use directory path for files)
	aggregationPath := item.path
	if !item.isDir {
		aggregationPath = filepath.Dir(item.path)
	}

	// Calculate level relative to root path
	relativeLevel := sp.calculateRelativeLevel(aggregationPath)
	
	// For items beyond maxLevel: still process files for propagation, skip directory metrics
	beyondMaxLevel := relativeLevel > sp.maxLevel
	if beyondMaxLevel {
		if item.isDir {
			// Skip directory metrics beyond maxLevel, but still propagate any files found
			log.Tracef("Skipping directory metrics beyond maxLevel: %s (relative level: %d > %d)", item.path, relativeLevel, sp.maxLevel)
			return
		} else {
			// For files beyond maxLevel: only propagate to parent levels, don't create level metrics
			log.Tracef("Processing file beyond maxLevel for propagation only: %s (relative level: %d > %d)", item.path, relativeLevel, sp.maxLevel)
			sp.propagateFileSizeToParents(aggregationPath, relativeLevel, item.size)
			return
		}
	}
	
	// Process all items - let the level-based aggregation handle duplicates
	// Only skip if this would create infinite recursion (which shouldn't happen in streaming)

	// Create stats key for the immediate level
	key := fmt.Sprintf("%s:%d", aggregationPath, relativeLevel)
	log.Debugf("Creating stats key: %s for item: %s (level: %d)", key, item.path, relativeLevel)

	// Get or create stats for this path/level
	sp.statsMutex.Lock()
	if sp.stats[key] == nil {
		log.Debugf("Creating new stats for key: %s", key)
		sp.stats[key] = &aggregatedStats{
			path:  aggregationPath,
			level: relativeLevel,
			// Only initialize sizeBuckets if needed
			sizeBuckets: func() map[string]int64 {
				if sp.exporter.collectSizeBucket {
					return make(map[string]int64)
				}
				return nil
			}(),
		}
	}
	stats := sp.stats[key]
	sp.statsMutex.Unlock()

	// Update statistics atomically
	stats.Lock()
	defer stats.Unlock()

	// Process the item at its immediate level
	if item.isDir {
		// For directories: only add size to immediate level, count the directory
		stats.totalSize += item.size
		log.Debugf("Directory size %d added only to level %d: %s", item.size, relativeLevel, item.path)
		
		// Only increment directory count if flag is enabled
		if sp.exporter.collectDirCount {
			stats.directoryCount++
		}
		log.Tracef("[Level %d] Directory processed: %s (size: %d bytes)", relativeLevel, item.path, item.size)
	} else {
		// For files: add size to immediate level and count the file
		stats.totalSize += item.size
		
		// Count file at its immediate level only
		if sp.exporter.collectFileCount {
			stats.fileCount++
		}

		// Only process size buckets if enabled
		if sp.exporter.collectSizeBucket && stats.sizeBuckets != nil {
			sizeRange := getSizeRange(item.size)
			stats.sizeBuckets[sizeRange]++
			log.Tracef("[Level %d] File processed: %s (size: %d bytes, bucket: %s)", 
				relativeLevel, item.path, item.size, sizeRange)
		}
		
		// FIX: Propagate file size to all parent directory levels
		// This ensures level 1 directories include all files from subdirectories
		sp.propagateFileSizeToParents(aggregationPath, relativeLevel, item.size)
	}
}

// propagateFileSizeToParents propagates file size to all parent directory levels
func (sp *streamingProcessor) propagateFileSizeToParents(startPath string, startLevel int, fileSize int64) {
	currentPath := startPath
	
	// Move up the directory hierarchy and add file size to each parent level
	for level := startLevel - 1; level >= 0; level-- {
		currentPath = filepath.Dir(currentPath)
		
		// Safety check to prevent infinite loops
		if currentPath == "/" || currentPath == "." {
			break
		}
		
		// Stop if we go beyond the root path
		rootLevel := sp.calculateRelativeLevel(currentPath)
		if rootLevel < 0 {
			break
		}
		
		parentKey := fmt.Sprintf("%s:%d", currentPath, rootLevel)
		
		// Get or create stats for parent level
		sp.statsMutex.Lock()
		if sp.stats[parentKey] == nil {
			sp.stats[parentKey] = &aggregatedStats{
				path:  currentPath,
				level: rootLevel,
				sizeBuckets: func() map[string]int64 {
					if sp.exporter.collectSizeBucket {
						return make(map[string]int64)
					}
					return nil
				}(),
			}
		}
		parentStats := sp.stats[parentKey]
		sp.statsMutex.Unlock()
		
		// Add file size to parent level
		parentStats.Lock()
		parentStats.totalSize += fileSize
		log.Debugf("Propagated file size %d to parent level %d: %s", fileSize, rootLevel, currentPath)
		parentStats.Unlock()
	}
}

// calculateRelativeLevel calculates the level relative to the root path
func (sp *streamingProcessor) calculateRelativeLevel(path string) int {
	// Clean both paths to ensure consistent comparison
	cleanRootPath := filepath.Clean(sp.rootPath)
	cleanPath := filepath.Clean(path)
	
	// If the path is the same as root path, level is 0
	if cleanPath == cleanRootPath {
		return 0
	}
	
	// Check if path is within root path
	if !strings.HasPrefix(cleanPath, cleanRootPath+string(filepath.Separator)) {
		// Path is not within root path, return 0 for safety
		return 0
	}
	
	// Get relative path from root
	relativePath := strings.TrimPrefix(cleanPath, cleanRootPath+string(filepath.Separator))
	
	// Count directory separators to determine level
	if relativePath == "" {
		return 0
	}
	
	level := strings.Count(relativePath, string(filepath.Separator)) + 1
	return level
}

// getStreamingStats returns the collected statistics and transfers ownership
func (sp *streamingProcessor) getStreamingStats() map[string]*aggregatedStats {
	sp.statsMutex.Lock()
	defer sp.statsMutex.Unlock()

	// Transfer ownership of stats map
	result := sp.stats
	log.Debugf("Before transfer: stats map has %d entries", len(result))
	for key, stats := range result {
		log.Debugf("Stats entry: %s -> size=%d, level=%d", key, stats.totalSize, stats.level)
	}
	sp.stats = make(map[string]*aggregatedStats) // Create new empty map

	log.Debugf("Transferred %d aggregated stats from streaming processor", len(result))
	return result
}

// setScanningState sets the scanning state
func (e *Exporter) setScanningState(scanning bool) {
	e.scanMutex.Lock()
	defer e.scanMutex.Unlock()
	e.isScanning = scanning
}

// isCurrentlyScanning returns true if scan is in progress
func (e *Exporter) isCurrentlyScanning() bool {
	e.scanMutex.RLock()
	defer e.scanMutex.RUnlock()
	return e.isScanning
}

// setValidResults marks that we have valid scan results
func (e *Exporter) setValidResults(valid bool) {
	e.scanMutex.Lock()
	defer e.scanMutex.Unlock()
	e.hasValidResults = valid
}

// hasValidScanResults returns true if we have valid cached results
func (e *Exporter) hasValidScanResults() bool {
	e.scanMutex.RLock()
	defer e.scanMutex.RUnlock()
	return e.hasValidResults
}

func init() {
	prometheus.MustRegister(collectors...)
}

// Exporter is the type to be used to start HTTP server and run the analysis
type Exporter struct {
	paths          map[string]int
	ignoreDirPaths map[string]struct{}
	followSymlinks bool
	basicAuth      map[string]string
	storagePath    string
	scanInterval   time.Duration
	storage        *analyze.Storage
	storageCloseFn func()
	cachedStats    map[string]map[string]*aggregatedStats // path -> level:stats
	lastScanTime   time.Time
	mu             sync.RWMutex
	stopChan       chan struct{}
	useStorage     bool
	diskCaching    bool
	maxProcs       int
	chunkSize      int
	collectDirCount    bool
	collectFileCount   bool
	collectSizeBucket  bool
	
	// Sequential DB write components
	sharedStorage  *analyze.Storage
	jsonStorage    JSONStorageInterface
	storageMutex   sync.Mutex
	writeQueue     chan writeRequest
	writerDone     chan struct{}
	
	// Aggregated statistics storage
	aggregatedStats map[string]*aggregatedStats
	statsMutex      sync.RWMutex
	topNLimit       int
	
	// Scan state tracking
	isScanning      bool
	scanMutex       sync.RWMutex
	hasValidResults bool
	
	// Temporary stats during scanning
	tempStats       map[string]*aggregatedStats
	tempStatsMutex  sync.RWMutex
}

// NewExporter creates new Exporter
func NewExporter(paths map[string]int, followSymlinks bool) *Exporter {
	return &Exporter{
		followSymlinks:  followSymlinks,
		paths:           paths,
		stopChan:        make(chan struct{}),
		maxProcs:        4,    // default value
		chunkSize:       1000, // default chunk size
		writeQueue:      make(chan writeRequest, 100), // Buffer for write requests
		writerDone:      make(chan struct{}),
		aggregatedStats: make(map[string]*aggregatedStats),
		tempStats:       make(map[string]*aggregatedStats),
		cachedStats:     make(map[string]map[string]*aggregatedStats),
		topNLimit:       1000, // Track top 1000 largest files (only if needed)
	}
}

// SetStorageConfig sets storage path, scan interval in minutes, and disk caching flag
func (e *Exporter) SetStorageConfig(storagePath string, scanIntervalMinutes int, diskCaching bool) {
	e.storagePath = storagePath
	e.scanInterval = time.Duration(scanIntervalMinutes) * time.Minute
	e.useStorage = storagePath != "" && scanIntervalMinutes > 0
	e.diskCaching = diskCaching
	
	if diskCaching {
		log.Infof("Disk caching enabled with BadgerDB + JSON storage at: %s", storagePath)
	} else {
		log.Infof("Disk caching disabled, using memory-only cache")
	}
}

// SetMaxProcs sets the maximum number of CPU cores to use
func (e *Exporter) SetMaxProcs(maxProcs int) {
	if maxProcs <= 0 {
		maxProcs = 4 // default value
	}
	e.maxProcs = maxProcs
}

// SetChunkSize sets the chunk size for batch processing
func (e *Exporter) SetChunkSize(chunkSize int) {
	if chunkSize <= 0 {
		chunkSize = 1000 // default value
	}
	e.chunkSize = chunkSize
}

// SetCollectionFlags sets which metrics to collect
func (e *Exporter) SetCollectionFlags(dirCount, fileCount, sizeBucket bool) {
	e.collectDirCount = dirCount
	e.collectFileCount = fileCount
	e.collectSizeBucket = sizeBucket
}

// initializeSharedStorage initializes shared storage and starts writer goroutine
func (e *Exporter) initializeSharedStorage() {
	e.storageMutex.Lock()
	defer e.storageMutex.Unlock()
	
	// Use JSON storage when disk caching is enabled
	if e.diskCaching && e.storagePath != "" {
		if e.jsonStorage == nil {
			e.jsonStorage = NewJSONStorage(e.storagePath)
			closeFn, err := e.jsonStorage.Open()
			if err != nil {
				log.Warnf("Failed to open JSON storage at %s: %v, falling back to memory-only mode", e.storagePath, err)
				e.jsonStorage = nil
				e.diskCaching = false
				return
			}
			e.storageCloseFn = closeFn
			log.Infof("JSON storage initialized successfully at: %s", e.storagePath)
			
			// Load existing data from storage
			e.loadExistingDataFromStorage()
			
			// Start writer goroutine
			go e.runStorageWriter()
		}
	} else {
		// Fallback to original gob-based storage for backward compatibility
		if e.sharedStorage == nil && e.useStorage {
			e.sharedStorage = analyze.NewStorage(e.storagePath, "")
			if e.sharedStorage != nil {
				closeFn := e.sharedStorage.Open()
				if closeFn != nil {
					e.storageCloseFn = closeFn
					// Start writer goroutine
					go e.runStorageWriter()
				}
			}
		}
	}
}

// runStorageWriter runs the sequential DB write goroutine
func (e *Exporter) runStorageWriter() {
	defer close(e.writerDone)
	
	for {
		select {
		case req := <-e.writeQueue:
			err := e.writeToStorage(req.path, req.result)
			if req.done != nil {
				req.done <- err
			}
		case <-e.stopChan:
			return
		}
	}
}

// writeToStorage writes analysis result to storage sequentially
func (e *Exporter) writeToStorage(path string, result fs.Item) error {
	e.storageMutex.Lock()
	defer e.storageMutex.Unlock()
	
	// Use JSON storage if disk caching is enabled
	if e.diskCaching && e.jsonStorage != nil {
		// Get the dir-level for this path to limit caching depth
		maxLevel := e.paths[path]
		if maxLevel < 0 {
			maxLevel = 0 // Default to level 0 if not found
		}
		
		// Use level-based caching to optimize storage size
		if jsonStorageWithLevel, ok := e.jsonStorage.(interface {
			StoreDirWithLevel(string, fs.Item, int) error
		}); ok {
			log.Debugf("Storing with level limit %d for path: %s", maxLevel, path)
			return jsonStorageWithLevel.StoreDirWithLevel(path, result, maxLevel)
		}
		
		// Fallback to regular storage if StoreDirWithLevel is not available
		return e.jsonStorage.StoreDir(path, result)
	}
	
	// Fallback to gob storage for backward compatibility
	if e.sharedStorage != nil {
		return e.sharedStorage.StoreDir(result)
	}
	
	return fmt.Errorf("no storage backend initialized")
}

// loadExistingDataFromStorage loads cached data from JSON storage on startup
func (e *Exporter) loadExistingDataFromStorage() {
	if e.jsonStorage == nil || !e.jsonStorage.IsOpen() {
		return
	}
	
	// Initialize cached stats map if needed
	if e.cachedStats == nil {
		e.cachedStats = make(map[string]map[string]*aggregatedStats)
	}
	
	// Attempt to load data for each configured path
	loadedCount := 0
	for path := range e.paths {
		// Get the dir-level for this path to load only necessary data
		maxLevel := e.paths[path]
		if maxLevel < 0 {
			maxLevel = 0 // Default to level 0 if not found
		}
		
		// Try to load with level filtering if available
		var item fs.Item
		var err error
		if jsonStorageWithLevel, ok := e.jsonStorage.(interface {
			LoadDirWithLevel(string, int) (fs.Item, error)
		}); ok {
			log.Debugf("Loading with level limit %d for path: %s", maxLevel, path)
			item, err = jsonStorageWithLevel.LoadDirWithLevel(path, maxLevel)
		} else {
			// Fallback to regular loading
			item, err = e.jsonStorage.LoadDir(path)
		}
		
		if err == nil && item != nil {
			// Convert loaded fs.Item to lightweight aggregated stats
			e.convertToAggregatedStats(path, item)
			loadedCount++
			log.Debugf("Loaded and converted cached data from storage for path: %s", path)
		} else {
			log.Debugf("No cached data found in storage for path: %s (this is normal for first run)", path)
		}
	}
	
	if loadedCount > 0 {
		log.Infof("Successfully loaded cached data from JSON storage for %d out of %d paths", loadedCount, len(e.paths))
		// Update last scan time to indicate we have valid data
		e.lastScanTime = time.Now()
		e.setValidResults(true)
	} else {
		log.Info("No cached data found in JSON storage - will perform fresh scan")
	}
}

// queueStorageWrite queues a write request for sequential processing
func (e *Exporter) queueStorageWrite(path string, result fs.Item) {
	select {
	case e.writeQueue <- writeRequest{path: path, result: result, done: nil}:
		// Queued successfully
	default:
		// Queue is full, log warning
		log.Warnf("Storage write queue is full, dropping write request for path: %s", path)
	}
}

func (e *Exporter) runBackgroundScan() {
	if e.storagePath == "" || e.scanInterval == 0 {
		return
	}

	ticker := time.NewTicker(e.scanInterval)
	defer ticker.Stop()

	// Run initial scan immediately
	e.performScan()

	for {
		select {
		case <-ticker.C:
			e.performScan()
		case <-e.stopChan:
			return
		}
	}
}

func (e *Exporter) performScan() {
	defer debug.FreeOSMemory()

	log.Infof("Using %d CPU cores for streaming scanning (memory-optimized)", e.maxProcs)

	// Initialize cached stats map
	e.mu.Lock()
	if e.cachedStats == nil {
		e.cachedStats = make(map[string]map[string]*aggregatedStats)
	}
	e.mu.Unlock()

	// Use streaming analysis for all paths
	e.performStreamingAnalysis()

	log.Info("All streaming background scans completed")
}

// performStreamingAnalysis performs memory-efficient streaming analysis for all paths using worker pool
func (e *Exporter) performStreamingAnalysis() {
	// Create work units for all paths
	workUnits := make([]workUnit, 0, len(e.paths))
	for path, maxLevel := range e.paths {
		if maxLevel < 0 {
			maxLevel = 0
		}
		workUnits = append(workUnits, workUnit{
			rootPath: path,
			dirPath:  path,
			level:    0,
			maxLevel: maxLevel,
		})
	}
	
	// Create channels for work distribution
	workChan := make(chan workUnit, len(workUnits))
	resultChan := make(chan workResult, len(workUnits))
	
	// Fill work channel
	for _, unit := range workUnits {
		workChan <- unit
	}
	close(workChan)
	
	// Start worker pool with max-procs workers
	var wg sync.WaitGroup
	for i := 0; i < e.maxProcs; i++ {
		wg.Add(1)
		go e.streamingWorker(i, workChan, resultChan, &wg)
	}
	
	// Wait for all workers to complete
	go func() {
		wg.Wait()
		close(resultChan)
	}()
	
	// Process results and store aggregated stats
	pathResults := make(map[string]struct {
		stats   map[string]*aggregatedStats
		elapsed time.Duration
		err     error
	})
	
	for result := range resultChan {
		if result.err != nil {
			log.Warnf("Streaming scan failed for path %s: %v", result.rootPath, result.err)
			continue
		}
		
		// Merge stats for the same root path
		if existing, ok := pathResults[result.rootPath]; ok {
			// Merge stats maps
			for key, stats := range result.stats {
				existing.stats[key] = stats
			}
		} else {
			pathResults[result.rootPath] = struct {
				stats   map[string]*aggregatedStats
				elapsed time.Duration
				err     error
			}{
				stats:   result.stats,
				elapsed: time.Duration(0), // Will be updated with actual timing
				err:     result.err,
			}
		}
	}
	
	// Store final results
	for path, result := range pathResults {
		// Store aggregated stats in cache
		e.mu.Lock()
		e.cachedStats[path] = result.stats
		e.lastScanTime = time.Now()
		e.mu.Unlock()
		
		// Queue storage write if disk caching is enabled
		if e.diskCaching && e.jsonStorage != nil && len(result.stats) > 0 {
			// Create a dummy fs.Item for storage compatibility
			// Note: This should be replaced with native aggregatedStats storage
			log.Debugf("Skipping fs.Item-based storage for streaming results (path: %s)", path)
		}
		
		// Log scan completion time with goroutine stats
		log.Infof("Streaming scan completed for path: %s, stats: %d, workers: %d", 
			path, len(result.stats), e.maxProcs)
	}
	
	// Transfer cached stats to aggregated stats for metric publishing
	e.statsMutex.Lock()
	for _, pathStats := range pathResults {
		for statsKey, stats := range pathStats.stats {
			e.aggregatedStats[statsKey] = stats
		}
	}
	e.statsMutex.Unlock()
	
	// Publish aggregated metrics
	e.publishAggregatedMetrics()
	
	// Update results validity
	e.setValidResults(true)
}

// streamingWorker processes work units from the work channel using streaming analysis
func (e *Exporter) streamingWorker(workerID int, workChan <-chan workUnit, resultChan chan<- workResult, wg *sync.WaitGroup) {
	defer wg.Done()
	
	log.Debugf("Worker %d started", workerID)
	
	for unit := range workChan {
		log.Infof("Worker %d: Starting streaming scan for path: %s (dir-level: %d)", 
			workerID, unit.rootPath, unit.maxLevel)
		
		startTime := time.Now()
		
		// Create streaming processor for this work unit
		processor := newStreamingProcessor(e, unit.rootPath, unit.maxLevel)
		
		// Perform streaming analysis
		err := processor.processDirectoryStreaming()
		var stats map[string]*aggregatedStats
		if err == nil {
			stats = processor.getStreamingStats()
		}
		
		// Send result
		resultChan <- workResult{
			rootPath: unit.rootPath,
			stats:    stats,
			err:      err,
		}
		
		elapsed := time.Since(startTime)
		log.Infof("Worker %d: Completed streaming scan for path: %s, elapsed: %v, stats: %d", 
			workerID, unit.rootPath, elapsed, len(stats))
	}
	
	log.Debugf("Worker %d finished", workerID)
}

func (e *Exporter) performAnalysisWithStored(stored *analyze.StoredAnalyzer) {
	// Use WaitGroup for parallel analysis
	var wg sync.WaitGroup
	
	// Channel for results to prevent race conditions
	resultChan := make(chan struct {
		path   string
		result fs.Item
		elapsed time.Duration
	}, len(e.paths))
	
	// Start parallel analysis for each path
	for path := range e.paths {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			
			// Track scan time for each path
			startTime := time.Now()
			maxLevel := e.paths[path]
			if maxLevel < 0 {
				maxLevel = 0
			}
			log.Infof("Starting background scan for path: %s (dir-level: %d, goroutines: %d)", 
				path, maxLevel, runtime.NumGoroutine())
			
			// Use constGC=true for better memory management during intensive analysis
			// Handle potential database lock errors during analysis
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Warnf("Background scan panic recovered for path %s: %v", path, r)
						return
					}
				}()
				
				// Create separate analyzer for each path to avoid conflicts
				pathAnalyzer := analyze.CreateAnalyzer()
				pathAnalyzer.SetFollowSymlinks(e.followSymlinks)
				
				dir := pathAnalyzer.AnalyzeDir(path, e.shouldDirBeIgnored, true)
				dir.UpdateStats(fs.HardLinkedItems{})
				
				// Send result to channel
				resultChan <- struct {
					path   string
					result fs.Item
					elapsed time.Duration
				}{
					path:    path,
					result:  dir,
					elapsed: time.Since(startTime),
				}
			}()
		}(path)
	}
	
	// Wait for all analysis to complete
	go func() {
		wg.Wait()
		close(resultChan)
	}()
	
	// Process results and queue for sequential DB writes
	for result := range resultChan {
		// Convert and store results in lightweight stats cache
		e.mu.Lock()
		if e.cachedStats == nil {
			e.cachedStats = make(map[string]map[string]*aggregatedStats)
		}
		// Convert fs.Item to aggregated stats to save memory
		// MEMORY OPTIMIZATION: Process level-limited aggregated stats immediately
		e.convertToAggregatedStats(result.path, result.result)
		e.lastScanTime = time.Now()
		e.mu.Unlock()
		
		// Queue storage write if disk caching is enabled
		// JSON storage avoids the circular reference issues that occurred with gob
		if e.diskCaching && e.jsonStorage != nil {
			e.queueStorageWrite(result.path, result.result)
		}
		
		// MEMORY OPTIMIZATION: Immediately release the full fs.Item result after processing
		// This prevents holding large directory structures in memory longer than necessary
		result.result = nil
		runtime.GC() // Suggest garbage collection to free the large fs.Item structure
		
		// Log scan completion time with goroutine stats
		log.Infof("Background scan completed for path: %s, elapsed time: %v, goroutines: %d", result.path, result.elapsed, runtime.NumGoroutine())
	}
}

func (e *Exporter) performAnalysisWithRegular(analyzer *analyze.ParallelAnalyzer) {
	for path := range e.paths {
		// Track scan time for each path
		startTime := time.Now()
		maxLevel := e.paths[path]
		if maxLevel < 0 {
			maxLevel = 0
		}
		log.Infof("Starting background scan for path: %s (dir-level: %d, goroutines: %d)", 
			path, maxLevel, runtime.NumGoroutine())
		
		// Use constGC=true for better memory management during intensive analysis
		dir := analyzer.AnalyzeDir(path, e.shouldDirBeIgnored, true)
		dir.UpdateStats(fs.HardLinkedItems{})
		
		// Convert and store results in lightweight stats cache
		// MEMORY OPTIMIZATION: Process level-limited aggregated stats immediately
		e.mu.Lock()
		e.convertToAggregatedStats(path, dir)
		e.lastScanTime = time.Now()
		e.mu.Unlock()
		
		// Queue storage write if disk caching is enabled
		if e.diskCaching && e.jsonStorage != nil {
			e.queueStorageWrite(path, dir)
		}
		
		// MEMORY OPTIMIZATION: Release the full fs.Item after processing to free memory
		// This is especially important when scanning large directory structures
		dir = nil
		runtime.GC() // Suggest garbage collection to free the large fs.Item structure
		
		// Log scan completion time with goroutine stats
		elapsedTime := time.Since(startTime)
		log.Infof("Background scan completed for path: %s, elapsed time: %v, goroutines: %d", path, elapsedTime, runtime.NumGoroutine())
		
		// Reset progress for next analysis
		analyzer.ResetProgress()
	}
}

func (e *Exporter) runAnalysis() {
	defer debug.FreeOSMemory()

	log.Infof("Using %d CPU cores for live streaming analysis (memory-optimized)", e.maxProcs)

	// Set scanning state
	e.setScanningState(true)
	defer e.setScanningState(false)

	// Use streaming analysis for live results
	e.performLiveStreamingAnalysis()

	// Mark that we have valid results
	e.setValidResults(true)

	log.Info("All live streaming analysis completed")
}

// performLiveStreamingAnalysis performs live streaming analysis for immediate results
func (e *Exporter) performLiveStreamingAnalysis() {
	// Temporary stats for live analysis
	tempStats := make(map[string]map[string]*aggregatedStats)
	var tempMutex sync.RWMutex

	// Use WaitGroup for parallel analysis
	var wg sync.WaitGroup
	
	// Start parallel streaming analysis for each path
	for path := range e.paths {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			
			// Track scan time for each path
			startTime := time.Now()
			maxLevel := e.paths[path]
			if maxLevel < 0 {
				maxLevel = 0
			}
			log.Debugf("Starting live streaming scan for path: %s (dir-level: %d)", path, maxLevel)
			
			// Create streaming processor for this path
			processor := newStreamingProcessor(e, path, maxLevel)
			
			// Perform streaming analysis
			err := processor.processDirectoryStreaming()
			if err != nil {
				log.Warnf("Live streaming scan failed for path %s: %v", path, err)
				return
			}
			
			// Get results and store in temp stats
			stats := processor.getStreamingStats()
			tempMutex.Lock()
			tempStats[path] = stats
			tempMutex.Unlock()
			
			elapsedTime := time.Since(startTime)
			log.Debugf("Live streaming scan completed for path: %s, elapsed time: %v, stats: %d", 
				path, elapsedTime, len(stats))
		}(path)
	}
	
	// Wait for all analysis to complete
	wg.Wait()
	
	// Atomically update aggregated stats for live results
	e.statsMutex.Lock()
	e.aggregatedStats = make(map[string]*aggregatedStats)
	for _, pathStats := range tempStats {
		for key, stats := range pathStats {
			e.aggregatedStats[key] = stats
		}
	}
	e.statsMutex.Unlock()
	
	// Publish the live results
	e.publishAggregatedMetrics()
	
	log.Debugf("Live streaming analysis results published with %d total stats", len(e.aggregatedStats))
}

// runAnalysisAsync runs analysis in background without blocking
func (e *Exporter) runAnalysisAsync() {
	go func() {
		e.runAnalysis()
	}()
}

// provideEmptyMetrics provides empty metrics when no scan results are available
func (e *Exporter) provideEmptyMetrics() {
	for path, level := range e.paths {
		for i := 0; i <= level; i++ {
			levelStr := fmt.Sprintf("%d", i)
			
			// Provide metrics based on collection flags
			if e.collectDirCount {
				diskUsageDirectoryCount.WithLabelValues(path, levelStr).Set(0)
			}
			if e.collectFileCount {
				diskUsageFileCount.WithLabelValues(path, levelStr).Set(0)
			}
		}
	}
}

func (e *Exporter) performLiveAnalysisWithStored(stored *analyze.StoredAnalyzer) {
	for path, level := range e.paths {
		// Track scan time for each path
		startTime := time.Now()
		log.Infof("Starting live analysis for path: %s, initial goroutines: %d", path, runtime.NumGoroutine())
		
		// Use constGC=true for better memory management during intensive analysis
		// Handle potential database lock errors during analysis
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Warnf("Live analysis panic recovered for path %s: %v", path, r)
					// Set zero values for metrics when analysis fails
					for i := 0; i <= level; i++ {
						levelStr := fmt.Sprintf("%d", i)
						if e.collectDirCount {
							diskUsageDirectoryCount.WithLabelValues(path, levelStr).Set(0)
						}
						if e.collectFileCount {
							diskUsageFileCount.WithLabelValues(path, levelStr).Set(0)
						}
					}
				}
			}()
			
			dir := stored.AnalyzeDir(path, e.shouldDirBeIgnored, true)
			dir.UpdateStats(fs.HardLinkedItems{})
			e.analyzeAndAggregate(dir, 0, level, path)
			
			// Log scan completion time with goroutine stats
			elapsedTime := time.Since(startTime)
			log.Infof("Live analysis completed for path: %s, elapsed time: %v, goroutines: %d", path, elapsedTime, runtime.NumGoroutine())
		}()

		// Reset progress for next analysis
		stored.ResetProgress()
	}
}

func (e *Exporter) performLiveAnalysisWithRegular(analyzer *analyze.ParallelAnalyzer) {
	for path, level := range e.paths {
		// Track scan time for each path
		startTime := time.Now()
		log.Infof("Starting live analysis for path: %s, initial goroutines: %d", path, runtime.NumGoroutine())
		
		// Use constGC=true for better memory management during intensive analysis
		dir := analyzer.AnalyzeDir(path, e.shouldDirBeIgnored, true)
		dir.UpdateStats(fs.HardLinkedItems{})
		e.analyzeAndAggregate(dir, 0, level, path)
		
		// Log scan completion time with goroutine stats
		elapsedTime := time.Since(startTime)
		log.Infof("Live analysis completed for path: %s, elapsed time: %v, goroutines: %d", path, elapsedTime, runtime.NumGoroutine())

		// Reset progress for next analysis
		analyzer.ResetProgress()
	}
}

// SetIgnoreDirPaths sets paths to ignore
func (e *Exporter) SetIgnoreDirPaths(paths []string) {
	e.ignoreDirPaths = make(map[string]struct{}, len(paths))
	for _, path := range paths {
		e.ignoreDirPaths[path] = struct{}{}
	}
}

// SetBasicAuth sets Basic Auth credentials
func (e *Exporter) SetBasicAuth(users map[string]string) {
	e.basicAuth = users
}

func (e *Exporter) shouldDirBeIgnored(_, path string) bool {
	_, ok := e.ignoreDirPaths[path]
	return ok
}


func (e *Exporter) loadFromStorage() {
	e.mu.RLock()
	defer e.mu.RUnlock()

	// Check if we have cached stats in memory
	if e.cachedStats == nil {
		log.Debugf("No cached data available, returning empty metrics - storagePath: %s, scanInterval: %v, scanPaths: %v, followSymlinks: %v",
			e.storagePath, e.scanInterval, e.paths, e.followSymlinks)
		// Return empty metrics for all paths
		for path, level := range e.paths {
			for i := 0; i <= level; i++ {
				levelStr := fmt.Sprintf("%d", i)
				if e.collectDirCount {
					diskUsageDirectoryCount.WithLabelValues(path, levelStr).Set(0)
				}
				if e.collectFileCount {
					diskUsageFileCount.WithLabelValues(path, levelStr).Set(0)
				}
			}
		}
		return
	}

	for path, level := range e.paths {
		pathStats, exists := e.cachedStats[path]
		if !exists || pathStats == nil {
			log.Debugf("No cached data found for path: %s, returning empty metrics - storagePath: %s, scanInterval: %v, followSymlinks: %v",
				path, e.storagePath, e.scanInterval, e.followSymlinks)
			// Return empty metrics (0 bytes) for missing cache data
			for i := 0; i <= level; i++ {
				levelStr := fmt.Sprintf("%d", i)
				if e.collectDirCount {
					diskUsageDirectoryCount.WithLabelValues(path, levelStr).Set(0)
				}
				if e.collectFileCount {
					diskUsageFileCount.WithLabelValues(path, levelStr).Set(0)
				}
			}
			continue
		}
		// Use cached aggregated stats directly
		e.publishCachedStats(pathStats)
	}
}

// analyzeAndAggregate performs analysis and creates aggregated statistics
func (e *Exporter) analyzeAndAggregate(item fs.Item, level, maxLevel int, rootPath string) {
	// Use temporary stats during scanning to not interfere with current results
	e.tempStatsMutex.Lock()
	e.tempStats = make(map[string]*aggregatedStats)
	e.tempStatsMutex.Unlock()
	
	e.collectAggregatedStats(item, level, maxLevel, rootPath)
	
	// After collection is complete, atomically replace the main stats
	e.statsMutex.Lock()
	e.tempStatsMutex.Lock()
	e.aggregatedStats = e.tempStats
	e.tempStats = make(map[string]*aggregatedStats)
	e.tempStatsMutex.Unlock()
	e.statsMutex.Unlock()
	
	e.publishAggregatedMetrics()
}

// collectAggregatedStats recursively collects statistics for aggregation
func (e *Exporter) collectAggregatedStats(item fs.Item, level, maxLevel int, rootPath string) {
	if level > maxLevel {
		return
	}

	path := item.GetPath()
	
	// Log directory scanning start and progress by level
	if item.IsDir() {
		files := item.GetFiles()
		fileCount := 0
		dirCount := 0
		if files != nil {
			for _, file := range files {
				if file != nil {
					if file.IsDir() {
						dirCount++
					} else {
						fileCount++
					}
				}
			}
		}
		log.Debugf("[Level %d] Starting scan: %s (subdirs: %d, files: %d, total: %d)", 
			level, path, dirCount, fileCount, len(files))
	}
	
	// Skip files if no file-related metrics are needed
	if !item.IsDir() && !e.collectFileCount && !e.collectSizeBucket {
		return
	}
	
	// Skip directories if no directory-related metrics are needed - NEW OPTIMIZATION
	if item.IsDir() && !e.collectDirCount && level > 0 {
		// Still need to recurse for size-bucket collection from files
		if !e.collectSizeBucket {
			return
		}
	}
	
	// For files, use parent directory path for aggregation
	aggregationPath := path
	if !item.IsDir() {
		// Get parent directory path for file aggregation
		aggregationPath = filepath.Dir(path)
	}
	
	// Check if we should collect statistics for this path/level combination
	// For root path: only collect at level 0
	// For non-root paths: collect at all levels
	shouldCollect := (aggregationPath == rootPath && level == 0) || (aggregationPath != rootPath)
	
	if !shouldCollect {
		return // Skip creating stats entirely for root path at level > 0
	}
	
	// Initialize aggregated stats for this path/level if it doesn't exist
	key := fmt.Sprintf("%s:%d", aggregationPath, level)
	
	e.tempStatsMutex.Lock()
	if e.tempStats[key] == nil {
		e.tempStats[key] = &aggregatedStats{
			path:        aggregationPath,
			level:       level,
			// Only initialize sizeBuckets if needed
			sizeBuckets: func() map[string]int64 {
				if e.collectSizeBucket {
					return make(map[string]int64)
				}
				return nil
			}(),
			// Don't initialize topFiles if not needed
			topFiles:    nil, // Optimize: only create when actually needed
		}
	}
	stats := e.tempStats[key]
	e.tempStatsMutex.Unlock()

	// Update directory-level statistics
	stats.Lock()
	itemSize := item.GetUsage()
	
	// Process the item at its immediate level
	if item.IsDir() {
		// For directories: only add size to immediate level, count the directory
		stats.totalSize += itemSize
		log.Debugf("Directory size %d added only to level %d: %s", itemSize, level, path)
		
		// Only increment directory count if flag is enabled
		if e.collectDirCount {
			stats.directoryCount++
		}
		log.Tracef("[Level %d] Directory processed: %s (size: %d bytes)", level, path, itemSize)
	} else {
		// For files: add size to immediate level and count the file
		stats.totalSize += itemSize
		
		// Process file metrics at immediate level based on collection flags
		if e.collectFileCount {
			stats.fileCount++
		}
		
		// Only process size buckets if enabled and sizeBuckets map exists
		if e.collectSizeBucket && stats.sizeBuckets != nil {
			sizeRange := getSizeRange(itemSize)
			stats.sizeBuckets[sizeRange]++
			log.Tracef("[Level %d] File processed: %s (size: %d bytes, bucket: %s)", level, path, itemSize, sizeRange)
		}
	}
	
	stats.Unlock()
	
	// FIX: For files, propagate size to all parent directory levels
	// This ensures level 1 directories include all files from subdirectories
	if !item.IsDir() {
		e.propagateFileSizeToParents(aggregationPath, level, itemSize, rootPath)
	}

	// EARLY TERMINATION: Skip subdirectory processing if we've reached maxLevel
	// This prevents memory spike from loading large directory structures unnecessarily
	if !item.IsDir() || level >= maxLevel {
		return // Don't process subdirectories if we're at or beyond maxLevel
	}
	
	// Recursively process subdirectories only if within level limit
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Warnf("Storage access error during collectAggregatedStats for %s: %v", path, r)
				return
			}
		}()
		
		// MEMORY OPTIMIZATION: Only call GetFiles() if we actually need to process subdirectories
		// This prevents loading 100k+ directory entries into memory when level limit is reached
		files := item.GetFiles()
		if files == nil {
			log.Debugf("[Level %d] No files found for directory: %s", level, path)
			return
		}
		
		log.Debugf("[Level %d] Processing %d entries in directory: %s (maxLevel: %d)", level, len(files), path, maxLevel)
		
		// OPTIMIZATION: Only process entries if we need the data
		processedCount := 0
		for _, entry := range files {
			if entry == nil {
				continue
			}
			
			// EARLY TERMINATION: Skip directories that would exceed maxLevel
			if entry.IsDir() && level+1 > maxLevel {
				log.Tracef("[Level %d] Skipping directory beyond maxLevel: %s", level+1, entry.GetPath())
				continue
			}
			
			// Skip processing if no relevant metrics needed for this entry type
			if !entry.IsDir() && !e.collectFileCount && !e.collectSizeBucket {
				continue
			}
			
			e.collectAggregatedStats(entry, level+1, maxLevel, rootPath)
			processedCount++
		}
		
		skippedCount := len(files) - processedCount
		log.Debugf("[Level %d] Completed scan: %s (processed: %d/%d entries, skipped: %d)", 
			level, path, processedCount, len(files), skippedCount)
	}()
}

// propagateFileSizeToParents propagates file size to all parent directory levels for collectAggregatedStats
func (e *Exporter) propagateFileSizeToParents(startPath string, startLevel int, fileSize int64, rootPath string) {
	currentPath := startPath
	
	// Move up the directory hierarchy and add file size to each parent level
	for level := startLevel - 1; level >= 0; level-- {
		currentPath = filepath.Dir(currentPath)
		
		// Safety check to prevent infinite loops
		if currentPath == "/" || currentPath == "." {
			break
		}
		
		// Calculate actual level for this path
		actualLevel := level
		if currentPath == rootPath {
			actualLevel = 0
		}
		
		// Skip if we've gone beyond the root path
		if len(currentPath) < len(rootPath) {
			break
		}
		
		parentKey := fmt.Sprintf("%s:%d", currentPath, actualLevel)
		
		// Get or create stats for parent level
		e.tempStatsMutex.Lock()
		if e.tempStats[parentKey] == nil {
			e.tempStats[parentKey] = &aggregatedStats{
				path:        currentPath,
				level:       actualLevel,
				sizeBuckets: func() map[string]int64 {
					if e.collectSizeBucket {
						return make(map[string]int64)
					}
					return nil
				}(),
				topFiles: nil,
			}
		}
		parentStats := e.tempStats[parentKey]
		e.tempStatsMutex.Unlock()
		
		// Add file size to parent level
		parentStats.Lock()
		parentStats.totalSize += fileSize
		log.Debugf("Propagated file size %d to parent level %d: %s", fileSize, actualLevel, currentPath)
		parentStats.Unlock()
		
		// Stop when we reach the root path
		if currentPath == rootPath {
			break
		}
	}
}

// convertToAggregatedStats converts fs.Item to lightweight aggregated stats
// MEMORY OPTIMIZATION: Only processes directories up to the configured level limit
func (e *Exporter) convertToAggregatedStats(rootPath string, item fs.Item) {
	if item == nil {
		return
	}
	
	level := e.paths[rootPath]
	if level < 0 {
		level = 0
	}
	
	// MEMORY OPTIMIZATION: Log level limits for debugging large directory issues
	log.Debugf("Converting fs.Item to aggregated stats for path: %s (maxLevel: %d)", rootPath, level)
	
	// Initialize path stats if needed
	if e.cachedStats[rootPath] == nil {
		e.cachedStats[rootPath] = make(map[string]*aggregatedStats)
	}
	
	// Clear existing stats and regenerate from fs.Item
	e.tempStatsMutex.Lock()
	e.tempStats = make(map[string]*aggregatedStats)
	e.tempStatsMutex.Unlock()
	
	// EARLY TERMINATION: Collect stats from the fs.Item structure with strict level limits
	// This prevents processing beyond the configured dir-level, saving memory
	e.collectAggregatedStats(item, 0, level, rootPath)
	
	// Copy temp stats to cached stats
	e.tempStatsMutex.Lock()
	for key, stats := range e.tempStats {
		e.cachedStats[rootPath][key] = stats
	}
	e.tempStats = make(map[string]*aggregatedStats)
	e.tempStatsMutex.Unlock()
	
	log.Debugf("Converted fs.Item to lightweight stats for path: %s (stats count: %d)", rootPath, len(e.cachedStats[rootPath]))
}

// publishCachedStats publishes cached aggregated stats to Prometheus
func (e *Exporter) publishCachedStats(pathStats map[string]*aggregatedStats) {
	for _, stats := range pathStats {
		stats.Lock()
		levelStr := fmt.Sprintf("%d", stats.level)
		
		// Always publish total disk usage metric
		diskUsage.WithLabelValues(stats.path, levelStr).Set(float64(stats.totalSize))
		
		// Publish metrics based on collection flags
		if e.collectDirCount {
			diskUsageDirectoryCount.WithLabelValues(stats.path, levelStr).Set(float64(stats.directoryCount))
		}
		
		if e.collectFileCount {
			diskUsageFileCount.WithLabelValues(stats.path, levelStr).Set(float64(stats.fileCount))
		}
		
		if e.collectSizeBucket && stats.sizeBuckets != nil {
			for sizeRange, count := range stats.sizeBuckets {
				diskUsageSizeBucket.WithLabelValues(stats.path, sizeRange, levelStr).Set(float64(count))
			}
		}
		
		stats.Unlock()
	}
}

// updateTopFilesLocked maintains the top-N largest files list (assumes stats is already locked) - UNUSED
// This method is currently unused due to memory optimization
func (e *Exporter) updateTopFilesLocked(stats *aggregatedStats, path string, size int64) {
	// This method is disabled for memory optimization
	// Top-N file tracking is memory intensive and disabled when only size-bucket is needed
	return
}

// publishAggregatedMetrics publishes all aggregated metrics to Prometheus
func (e *Exporter) publishAggregatedMetrics() {
	e.statsMutex.RLock()
	defer e.statsMutex.RUnlock()
	
	for _, stats := range e.aggregatedStats {
		stats.Lock()
		levelStr := fmt.Sprintf("%d", stats.level)
		
		// Always publish total disk usage metric
		diskUsage.WithLabelValues(stats.path, levelStr).Set(float64(stats.totalSize))
		
		// Publish metrics based on collection flags
		if e.collectDirCount {
			diskUsageDirectoryCount.WithLabelValues(stats.path, levelStr).Set(float64(stats.directoryCount))
		}
		
		if e.collectFileCount {
			diskUsageFileCount.WithLabelValues(stats.path, levelStr).Set(float64(stats.fileCount))
		}
		
		if e.collectSizeBucket {
			for sizeRange, count := range stats.sizeBuckets {
				diskUsageSizeBucket.WithLabelValues(stats.path, sizeRange, levelStr).Set(float64(count))
			}
		}
		
		// Total disk usage metric is published above
		
		stats.Unlock()
	}
}

// WriteToTextfile writes the prometheus report to file
func (e *Exporter) WriteToTextfile(name string) {
	e.runAnalysis()

	// Use a custom registry to drop go stats
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors...)

	err := prometheus.WriteToTextfile(name, registry)
	if err != nil {
		log.Fatalf("WriteToTextfile error: %v", err)
	}
	log.Printf("Stored stats in file %s", name)
}

func (e *Exporter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if len(e.basicAuth) > 0 {
		if ok := e.authorizeReq(w, req); !ok {
			return
		}
	}

	// Handle metrics request with immediate response
	if e.storagePath != "" && e.scanInterval > 0 {
		// Background caching mode - use cached results or empty metrics
		e.loadFromStorage()
	} else {
		// Live analysis mode - check if scan is needed
		if e.hasValidScanResults() {
			// We have previous results, use them
			log.Debugf("Using cached analysis results")
		} else if e.isCurrentlyScanning() {
			// Scan is in progress, provide empty metrics
			log.Debugf("Scan in progress, providing empty metrics")
			e.provideEmptyMetrics()
		} else {
			// No scan in progress and no results, start async scan and provide empty metrics
			log.Debugf("Starting background scan, providing empty metrics")
			e.runAnalysisAsync()
			e.provideEmptyMetrics()
		}
	}
	
	promhttp.Handler().ServeHTTP(w, req)
}

func (e *Exporter) authorizeReq(w http.ResponseWriter, req *http.Request) bool {
	user, pass, ok := req.BasicAuth()
	if ok {
		if hashed, found := e.basicAuth[user]; found {
			err := bcrypt.CompareHashAndPassword([]byte(hashed), []byte(pass))
			if err == nil {
				return true
			}
		}
	}

	w.Header().Add("WWW-Authenticate", "Basic realm=\"Access to disk usage exporter\"")
	w.WriteHeader(401)
	return false
}

// StartBackgroundScan starts the background scanning goroutine
func (e *Exporter) StartBackgroundScan() {
	if e.storagePath != "" && e.scanInterval > 0 {
		// Initialize storage before starting background scan
		e.initializeSharedStorage()
		go e.runBackgroundScan()
		log.Printf("Background scan started with interval: %v", e.scanInterval)
	}
}

// Stop stops the background scanning
func (e *Exporter) Stop() {
	close(e.stopChan)
	
	// Wait for writer goroutine to finish if it was started
	if (e.useStorage || e.diskCaching) && e.writerDone != nil {
		<-e.writerDone
	}
	
	// Close the write queue to prevent new writes
	if e.writeQueue != nil {
		close(e.writeQueue)
	}
	
	if e.storageCloseFn != nil {
		e.storageCloseFn()
	}
	
	// Clean up cached stats
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cachedStats = nil
	e.storage = nil
	e.sharedStorage = nil
	
	// Clean up JSON storage
	if e.jsonStorage != nil {
		e.jsonStorage.Close()
		e.jsonStorage = nil
	}
}

// RunServer starts HTTP server loop
func (e *Exporter) RunServer(addr string) {
	http.Handle("/", http.HandlerFunc(ServeIndex))
	http.Handle("/metrics", e)
	http.Handle("/version", http.HandlerFunc(ServeVersion))
	http.Handle("/health", http.HandlerFunc(ServeHealth))

	// Start background scanning if configured
	e.StartBackgroundScan()

	log.Printf("Providing metrics at http://%s/metrics", addr)
	err := http.ListenAndServe(addr, nil)
	if err != nil {
		log.Fatal("ListenAndServe:", err)
	}
}

// ServeHealth serves health check endpoint
func ServeHealth(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	healthInfo := map[string]string{
		"status":  "ok",
		"version": build.BuildVersion,
	}
	json.NewEncoder(w).Encode(healthInfo)
}

// ServeVersion serves version information
func ServeVersion(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	versionInfo := map[string]string{
		"version":   build.BuildVersion,
		"buildDate": build.BuildDate,
		"commitSha": build.BuildCommitSha,
		"goVersion": runtime.Version(),
		"goos":      runtime.GOOS,
		"goarch":    runtime.GOARCH,
	}
	json.NewEncoder(w).Encode(versionInfo)
}

// ServeIndex serves index page
func ServeIndex(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-type", "text/html")
	res := `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="utf-8">
	<meta name="viewport" content="width=device-width">
	<title>Disk Usage Prometheus Exporter</title>
</head>
<body>
<h1>Disk Usage Prometheus Exporter</h1>
<p>
	<a href="/metrics">Metrics</a>
</p>
<p>
	<a href="/version">Version</a>
</p>
<p>
	<a href="/health">Health</a>
</p>
<p>
	<a href="https://github.com/dundee/disk_usage_exporter">Homepage</a>
</p>
</body>
</html>
`
	fmt.Fprint(w, res)
}
