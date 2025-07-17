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
	diskUsage = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_disk_usage_bytes",
			Help: "Disk usage of the directory/file",
		},
		[]string{"path", "level"},
	)
	diskUsageLevel1 = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_disk_usage_level_1_bytes",
			Help: "Disk usage of the directory/file level 1",
		},
		[]string{"path"},
	)
	collectors = []prometheus.Collector{diskUsage, diskUsageLevel1}
)

// writeRequest represents a request to write analysis results to storage
type writeRequest struct {
	path   string
	result fs.Item
	done   chan error
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
	
	// Sequential DB write components
	sharedStorage  *analyze.Storage
	storageMutex   sync.Mutex
	writeQueue     chan writeRequest
	writerDone     chan struct{}
}

// NewExporter creates new Exporter
func NewExporter(paths map[string]int, followSymlinks bool) *Exporter {
	return &Exporter{
		followSymlinks: followSymlinks,
		paths:          paths,
		stopChan:       make(chan struct{}),
		maxProcs:       4, // default value
		writeQueue:     make(chan writeRequest, 100), // Buffer for write requests
		writerDone:     make(chan struct{}),
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

	log.Info("All live analysis completed")
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
					diskUsage.WithLabelValues(path, "0").Set(0)
					if level >= 1 {
						diskUsageLevel1.WithLabelValues(path).Set(0)
					}
				}
			}()
			
			dir := stored.AnalyzeDir(path, e.shouldDirBeIgnored, true)
			dir.UpdateStats(fs.HardLinkedItems{})
			e.reportItem(dir, 0, level)
			
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
		e.reportItem(dir, 0, level)
		
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
			diskUsage.WithLabelValues(path, "0").Set(0)
			if level >= 1 {
				diskUsageLevel1.WithLabelValues(path).Set(0)
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
			diskUsage.WithLabelValues(path, "0").Set(0)
			if level >= 1 {
				diskUsageLevel1.WithLabelValues(path).Set(0)
			}
			continue
		}
		e.reportItem(dir, 0, level)
	}
}

func (e *Exporter) reportItem(item fs.Item, level, maxLevel int) {
	// Always report the root path (level 0) and paths at maxLevel
	if level == 0 || level == maxLevel {
		diskUsage.WithLabelValues(item.GetPath(), fmt.Sprintf("%d", level)).Set(float64(item.GetUsage()))
	} else if level == 1 {
		diskUsageLevel1.WithLabelValues(item.GetPath()).Set(float64(item.GetUsage()))
	}

	if item.IsDir() && level+1 <= maxLevel {
		// Safe handling of GetFiles() to prevent nil pointer panics
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Warnf("Storage access error during reportItem for %s: %v", item.GetPath(), r)
					return
				}
			}()
			
			// GetFiles() may panic if BadgerDB connection is closed
			files := item.GetFiles()
			if files == nil {
				log.Debugf("No files found for directory: %s", item.GetPath())
				return
			}
			
			for _, entry := range files {
				if entry == nil {
					continue
				}
				e.reportItem(entry, level+1, maxLevel)
			}
		}()
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

	// Use cached storage data if available, otherwise run analysis
	if e.storagePath != "" && e.scanInterval > 0 {
		e.loadFromStorage()
	} else {
		e.runAnalysis()
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
