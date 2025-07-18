package exporter

import (
	"encoding/json"
	"fmt"
	"net/http"
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
	diskUsageDirectory = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_disk_usage_directory_bytes",
			Help: "Total disk usage by directory",
		},
		[]string{"path", "level"},
	)
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
	
	// 타입별 집계 메트릭
	diskUsageByType = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_disk_usage_by_type_bytes",
			Help: "Disk usage aggregated by file type",
		},
		[]string{"path", "type", "level"},
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
		diskUsageDirectory, diskUsageFileCount, diskUsageDirectoryCount,
		diskUsageByType, diskUsageSizeBucket, diskUsageTopFiles,
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
	totalSize      int64
	fileCount      int64
	directoryCount int64
	fileSize       int64
	directorySize  int64
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
	cachedResults  map[string]fs.Item
	lastScanTime   time.Time
	mu             sync.RWMutex
	stopChan       chan struct{}
	useStorage     bool
	maxProcs       int
	dirOnly        bool
	
	// Sequential DB write components
	sharedStorage  *analyze.Storage
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
		topNLimit:       1000, // Track top 1000 largest files
	}
}

// SetStorageConfig sets storage path and scan interval in minutes
func (e *Exporter) SetStorageConfig(storagePath string, scanIntervalMinutes int) {
	e.storagePath = storagePath
	e.scanInterval = time.Duration(scanIntervalMinutes) * time.Minute
	e.useStorage = storagePath != "" && scanIntervalMinutes > 0
	
	// Skip storage initialization to prevent circular reference issues
	// Only use memory cache for background scans
	log.Infof("Storage writes disabled to prevent circular reference issues, using memory-only cache")
}

// SetMaxProcs sets the maximum number of CPU cores to use
func (e *Exporter) SetMaxProcs(maxProcs int) {
	if maxProcs <= 0 {
		maxProcs = 4 // default value
	}
	e.maxProcs = maxProcs
}

// SetDirOnly sets whether to analyze only directories or include files
func (e *Exporter) SetDirOnly(dirOnly bool) {
	e.dirOnly = dirOnly
}

// initializeSharedStorage initializes shared storage and starts writer goroutine
func (e *Exporter) initializeSharedStorage() {
	e.storageMutex.Lock()
	defer e.storageMutex.Unlock()
	
	if e.sharedStorage == nil {
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
	
	if e.sharedStorage == nil {
		return fmt.Errorf("shared storage not initialized")
	}
	
	// Store the result in BadgerDB
	return e.sharedStorage.StoreDir(result)
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

	// Initialize cached results map
	e.mu.Lock()
	if e.cachedResults == nil {
		e.cachedResults = make(map[string]fs.Item)
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
		// Store results in memory cache immediately
		e.mu.Lock()
		if e.cachedResults == nil {
			e.cachedResults = make(map[string]fs.Item)
		}
		e.cachedResults[result.path] = result.result
		e.lastScanTime = time.Now()
		e.mu.Unlock()
		
		// Skip storage writes to prevent circular reference issues
		// Storage writes cause stack overflow due to gob serialization of fs.Item
		// Memory cache is sufficient for HTTP requests
		
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
		
		// Store results in memory cache 
		e.mu.Lock()
		e.cachedResults[path] = dir
		e.lastScanTime = time.Now()
		e.mu.Unlock()
		
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
			diskUsageDirectory.WithLabelValues(path, levelStr).Set(0)
			diskUsageDirectoryCount.WithLabelValues(path, levelStr).Set(0)
			diskUsageByType.WithLabelValues(path, "directory", levelStr).Set(0)
			
			// Only provide file-related metrics if dir-only is disabled
			if !e.dirOnly {
				diskUsageFileCount.WithLabelValues(path, levelStr).Set(0)
				diskUsageByType.WithLabelValues(path, "file", levelStr).Set(0)
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
						diskUsageDirectory.WithLabelValues(path, levelStr).Set(0)
						diskUsageDirectoryCount.WithLabelValues(path, levelStr).Set(0)
						if !e.dirOnly {
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

	// Check if we have cached results in memory
	if e.cachedResults == nil {
		log.Debugf("No cached data available, returning empty metrics - storagePath: %s, scanInterval: %v, scanPaths: %v, followSymlinks: %v",
			e.storagePath, e.scanInterval, e.paths, e.followSymlinks)
		// Return empty metrics for all paths
		for path, level := range e.paths {
			for i := 0; i <= level; i++ {
				levelStr := fmt.Sprintf("%d", i)
				diskUsageDirectory.WithLabelValues(path, levelStr).Set(0)
				diskUsageDirectoryCount.WithLabelValues(path, levelStr).Set(0)
				if !e.dirOnly {
					diskUsageFileCount.WithLabelValues(path, levelStr).Set(0)
				}
			}
		}
		return
	}

	for path, level := range e.paths {
		dir, exists := e.cachedResults[path]
		if !exists || dir == nil {
			log.Debugf("No cached data found for path: %s, returning empty metrics - storagePath: %s, scanInterval: %v, followSymlinks: %v",
				path, e.storagePath, e.scanInterval, e.followSymlinks)
			// Return empty metrics (0 bytes) for missing cache data
			for i := 0; i <= level; i++ {
				levelStr := fmt.Sprintf("%d", i)
				diskUsageDirectory.WithLabelValues(path, levelStr).Set(0)
				diskUsageDirectoryCount.WithLabelValues(path, levelStr).Set(0)
				if !e.dirOnly {
					diskUsageFileCount.WithLabelValues(path, levelStr).Set(0)
				}
			}
			continue
		}
		e.analyzeAndAggregate(dir, 0, level, path)
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
	
	// Skip files entirely if dir-only mode is enabled
	if e.dirOnly && !item.IsDir() {
		return
	}
	
	// Initialize aggregated stats for this path/level if it doesn't exist
	key := fmt.Sprintf("%s:%d", path, level)
	
	e.tempStatsMutex.Lock()
	if e.tempStats[key] == nil {
		e.tempStats[key] = &aggregatedStats{
			path:        path,
			level:       level,
			sizeBuckets: make(map[string]int64),
			topFiles:    make([]topFileInfo, 0),
		}
	}
	stats := e.tempStats[key]
	e.tempStatsMutex.Unlock()
	
	// Update directory-level statistics
	if level == 0 || path != rootPath {
		stats.Lock()
		stats.totalSize += item.GetUsage()
		
		if item.IsDir() {
			stats.directoryCount++
			stats.directorySize += item.GetUsage()
		} else if !e.dirOnly {
			// Only process files if dir-only mode is disabled
			stats.fileCount++
			stats.fileSize += item.GetUsage()
			
			// Update size buckets
			sizeRange := getSizeRange(item.GetUsage())
			stats.sizeBuckets[sizeRange]++
			
			// Track for top-N files (already locked)
			e.updateTopFilesLocked(stats, path, item.GetUsage())
		}
		stats.Unlock()
	}

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
				log.Debugf("No files found for directory: %s", path)
				return
			}
			
			for _, entry := range files {
				if entry == nil {
					continue
				}
				e.collectAggregatedStats(entry, level+1, maxLevel, rootPath)
			}
		}()
	}
}

// updateTopFilesLocked maintains the top-N largest files list (assumes stats is already locked)
func (e *Exporter) updateTopFilesLocked(stats *aggregatedStats, path string, size int64) {
	if len(stats.topFiles) < e.topNLimit {
		stats.topFiles = append(stats.topFiles, topFileInfo{
			path: path,
			size: size,
			rank: len(stats.topFiles) + 1,
		})
	} else {
		// Find smallest in top-N and replace if current is larger
		minIdx := 0
		minSize := stats.topFiles[0].size
		for i, file := range stats.topFiles {
			if file.size < minSize {
				minIdx = i
				minSize = file.size
			}
		}
		
		if size > minSize {
			stats.topFiles[minIdx] = topFileInfo{
				path: path,
				size: size,
				rank: minIdx + 1,
			}
		} else {
			// Add to others total
			stats.othersTotal += size
			stats.othersCount++
		}
	}
}

// publishAggregatedMetrics publishes all aggregated metrics to Prometheus
func (e *Exporter) publishAggregatedMetrics() {
	e.statsMutex.RLock()
	defer e.statsMutex.RUnlock()
	
	for _, stats := range e.aggregatedStats {
		stats.Lock()
		levelStr := fmt.Sprintf("%d", stats.level)
		
		// Directory-level metrics
		diskUsageDirectory.WithLabelValues(stats.path, levelStr).Set(float64(stats.totalSize))
		diskUsageDirectoryCount.WithLabelValues(stats.path, levelStr).Set(float64(stats.directoryCount))
		
		// File-related metrics - only publish if dir-only is disabled
		if !e.dirOnly {
			diskUsageFileCount.WithLabelValues(stats.path, levelStr).Set(float64(stats.fileCount))
			
			// Type-based metrics for files
			diskUsageByType.WithLabelValues(stats.path, "file", levelStr).Set(float64(stats.fileSize))
			
			// Size bucket metrics
			for sizeRange, count := range stats.sizeBuckets {
				diskUsageSizeBucket.WithLabelValues(stats.path, sizeRange, levelStr).Set(float64(count))
			}
			
			// Top-N file metrics
			for _, topFile := range stats.topFiles {
				diskUsageTopFiles.WithLabelValues(topFile.path, fmt.Sprintf("%d", topFile.rank)).Set(float64(topFile.size))
			}
			
			// Others metrics
			if stats.othersTotal > 0 {
				diskUsageOthersTotal.WithLabelValues(stats.path).Set(float64(stats.othersTotal))
				diskUsageOthersCount.WithLabelValues(stats.path).Set(float64(stats.othersCount))
			}
		}
		
		// Directory type-based metrics (always published)
		diskUsageByType.WithLabelValues(stats.path, "directory", levelStr).Set(float64(stats.directorySize))
		
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
		go e.runBackgroundScan()
		log.Printf("Background scan started with interval: %v", e.scanInterval)
	}
}

// Stop stops the background scanning
func (e *Exporter) Stop() {
	close(e.stopChan)
	
	// Wait for writer goroutine to finish if it was started
	if e.useStorage && e.writerDone != nil {
		<-e.writerDone
	}
	
	// Close the write queue to prevent new writes
	if e.writeQueue != nil {
		close(e.writeQueue)
	}
	
	if e.storageCloseFn != nil {
		e.storageCloseFn()
	}
	
	// Clean up cached results
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cachedResults = nil
	e.storage = nil
	e.sharedStorage = nil
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
