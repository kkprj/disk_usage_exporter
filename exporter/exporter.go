package exporter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"runtime/debug"
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
		maxProcs:        4, // default value
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

	log.Infof("Using %d CPU cores for parallel scanning (gdu internal parallelization)", e.maxProcs)

	// Use stored analyzer if storage is configured, otherwise regular analyzer
	if e.useStorage && e.storagePath != "" {
		// Handle potential database lock errors during storage analyzer creation
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Warnf("Storage analyzer creation failed, falling back to regular analyzer: %v", r)
					analyzer := analyze.CreateAnalyzer()
					analyzer.SetFollowSymlinks(e.followSymlinks)
					e.performAnalysisWithRegular(analyzer)
					return
				}
			}()
			
			stored := analyze.CreateStoredAnalyzer(e.storagePath)
			stored.SetFollowSymlinks(e.followSymlinks)
			e.performAnalysisWithStored(stored)
		}()
	} else {
		analyzer := analyze.CreateAnalyzer()
		analyzer.SetFollowSymlinks(e.followSymlinks)
		e.performAnalysisWithRegular(analyzer)
	}

	// Initialize cached stats map
	e.mu.Lock()
	if e.cachedStats == nil {
		e.cachedStats = make(map[string]map[string]*aggregatedStats)
	}
	e.mu.Unlock()

	log.Info("All background scans completed")
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
			log.Infof("Starting background scan for path: %s, initial goroutines: %d", path, runtime.NumGoroutine())
			
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
		e.convertToAggregatedStats(result.path, result.result)
		e.lastScanTime = time.Now()
		e.mu.Unlock()
		
		// Queue storage write if disk caching is enabled
		// JSON storage avoids the circular reference issues that occurred with gob
		if e.diskCaching && e.jsonStorage != nil {
			e.queueStorageWrite(result.path, result.result)
		}
		
		// Log scan completion time with goroutine stats
		log.Infof("Background scan completed for path: %s, elapsed time: %v, goroutines: %d", result.path, result.elapsed, runtime.NumGoroutine())
	}
}

func (e *Exporter) performAnalysisWithRegular(analyzer *analyze.ParallelAnalyzer) {
	for path := range e.paths {
		// Track scan time for each path
		startTime := time.Now()
		log.Infof("Starting background scan for path: %s, initial goroutines: %d", path, runtime.NumGoroutine())
		
		// Use constGC=true for better memory management during intensive analysis
		dir := analyzer.AnalyzeDir(path, e.shouldDirBeIgnored, true)
		dir.UpdateStats(fs.HardLinkedItems{})
		
		// Convert and store results in lightweight stats cache
		e.mu.Lock()
		e.convertToAggregatedStats(path, dir)
		e.lastScanTime = time.Now()
		e.mu.Unlock()
		
		// Queue storage write if disk caching is enabled
		if e.diskCaching && e.jsonStorage != nil {
			e.queueStorageWrite(path, dir)
		}
		
		// Log scan completion time with goroutine stats
		elapsedTime := time.Since(startTime)
		log.Infof("Background scan completed for path: %s, elapsed time: %v, goroutines: %d", path, elapsedTime, runtime.NumGoroutine())
		
		// Reset progress for next analysis
		analyzer.ResetProgress()
	}
}

func (e *Exporter) runAnalysis() {
	defer debug.FreeOSMemory()

	log.Infof("Using %d CPU cores for live analysis (gdu internal parallelization)", e.maxProcs)

	// Set scanning state
	e.setScanningState(true)
	defer e.setScanningState(false)

	// Create analyzer once and reuse for better performance
	if e.useStorage && e.storagePath != "" {
		stored := analyze.CreateStoredAnalyzer(e.storagePath)
		stored.SetFollowSymlinks(e.followSymlinks)
		e.performLiveAnalysisWithStored(stored)
	} else {
		analyzer := analyze.CreateAnalyzer()
		analyzer.SetFollowSymlinks(e.followSymlinks)
		e.performLiveAnalysisWithRegular(analyzer)
	}

	// Mark that we have valid results
	e.setValidResults(true)

	log.Info("All live analysis completed")
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
	
	// Log directory scanning progress by level
	if item.IsDir() {
		log.Debugf("[Level %d] Scanning directory: %s (files: %d)", level, path, len(item.GetFiles()))
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
	// Always collect total size for node_disk_usage metric
	stats.totalSize += item.GetUsage()
	
	if item.IsDir() {
		// Only increment directory count if flag is enabled
		if e.collectDirCount {
			stats.directoryCount++
		}
		log.Tracef("[Level %d] Directory processed: %s (size: %d bytes)", level, path, item.GetUsage())
	} else {
		// Process file metrics based on collection flags
		if e.collectFileCount {
			stats.fileCount++
		}
		
		// Only process size buckets if enabled and sizeBuckets map exists
		if e.collectSizeBucket && stats.sizeBuckets != nil {
			sizeRange := getSizeRange(item.GetUsage())
			stats.sizeBuckets[sizeRange]++
			log.Tracef("[Level %d] File processed: %s (size: %d bytes, bucket: %s)", level, path, item.GetUsage(), sizeRange)
		}
	}
	stats.Unlock()

	// Recursively process subdirectories
	if item.IsDir() && level+1 <= maxLevel {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Warnf("Storage access error during collectAggregatedStats for %s: %v", path, r)
					return
				}
			}()
			
			files := item.GetFiles()
			if files == nil {
				log.Debugf("[Level %d] No files found for directory: %s", level, path)
				return
			}
			
			log.Debugf("[Level %d] Processing %d entries in directory: %s", level, len(files), path)
			
			// OPTIMIZATION: Only process entries if we need the data
			processedCount := 0
			for _, entry := range files {
				if entry == nil {
					continue
				}
				
				// Skip processing if no relevant metrics needed for this entry type
				if entry.IsDir() && !e.collectDirCount && level+1 > maxLevel {
					continue
				}
				if !entry.IsDir() && !e.collectFileCount && !e.collectSizeBucket {
					continue
				}
				
				e.collectAggregatedStats(entry, level+1, maxLevel, rootPath)
				processedCount++
			}
			
			log.Debugf("[Level %d] Completed directory: %s (processed: %d/%d entries)", level, path, processedCount, len(files))
		}()
	}
}

// convertToAggregatedStats converts fs.Item to lightweight aggregated stats
func (e *Exporter) convertToAggregatedStats(rootPath string, item fs.Item) {
	if item == nil {
		return
	}
	
	level := e.paths[rootPath]
	if level < 0 {
		level = 0
	}
	
	// Initialize path stats if needed
	if e.cachedStats[rootPath] == nil {
		e.cachedStats[rootPath] = make(map[string]*aggregatedStats)
	}
	
	// Clear existing stats and regenerate from fs.Item
	e.tempStatsMutex.Lock()
	e.tempStats = make(map[string]*aggregatedStats)
	e.tempStatsMutex.Unlock()
	
	// Collect stats from the fs.Item structure
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
