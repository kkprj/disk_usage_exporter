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
		[]string{"path"},
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
}

// NewExporter creates new Exporter
func NewExporter(paths map[string]int, followSymlinks bool) *Exporter {
	return &Exporter{
		followSymlinks: followSymlinks,
		paths:          paths,
		stopChan:       make(chan struct{}),
		maxProcs:       4, // default value
	}
}

// SetStorageConfig sets storage path and scan interval in minutes
func (e *Exporter) SetStorageConfig(storagePath string, scanIntervalMinutes int) {
	e.storagePath = storagePath
	e.scanInterval = time.Duration(scanIntervalMinutes) * time.Minute
	e.useStorage = storagePath != "" && scanIntervalMinutes > 0
}

// SetMaxProcs sets the maximum number of CPU cores to use
func (e *Exporter) SetMaxProcs(maxProcs int) {
	if maxProcs <= 0 {
		maxProcs = 4 // default value
	}
	e.maxProcs = maxProcs
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

	// Set max cores for parallel scanning from config
	runtime.GOMAXPROCS(e.maxProcs)
	log.Infof("Using %d CPU cores for parallel scanning (GOMAXPROCS=%d)", e.maxProcs, runtime.GOMAXPROCS(0))

	// Use stored analyzer if storage is configured, otherwise regular analyzer
	if e.useStorage && e.storagePath != "" {
		stored := analyze.CreateStoredAnalyzer(e.storagePath)
		stored.SetFollowSymlinks(e.followSymlinks)
		e.performAnalysisWithStored(stored)
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
	for path := range e.paths {
		// Track scan time for each path
		startTime := time.Now()
		log.Infof("Starting background scan for path: %s, initial goroutines: %d", path, runtime.NumGoroutine())
		
		// Use constGC=true for better memory management during intensive analysis
		dir := stored.AnalyzeDir(path, e.shouldDirBeIgnored, true)
		dir.UpdateStats(fs.HardLinkedItems{})
		
		// Store results in memory cache (gdu's StoredAnalyzer handles disk storage automatically)
		e.mu.Lock()
		e.cachedResults[path] = dir
		e.lastScanTime = time.Now()
		e.mu.Unlock()
		
		// Log scan completion time with goroutine stats
		elapsedTime := time.Since(startTime)
		log.Infof("Background scan completed for path: %s, elapsed time: %v, goroutines: %d", path, elapsedTime, runtime.NumGoroutine())
		
		// Reset progress for next analysis
		stored.ResetProgress()
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

	// Set max cores for live analysis from config
	runtime.GOMAXPROCS(e.maxProcs)
	log.Infof("Using %d CPU cores for live analysis (GOMAXPROCS=%d)", e.maxProcs, runtime.GOMAXPROCS(0))

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
		dir := stored.AnalyzeDir(path, e.shouldDirBeIgnored, true)
		dir.UpdateStats(fs.HardLinkedItems{})
		e.reportItem(dir, 0, level)
		
		// Log scan completion time with goroutine stats
		elapsedTime := time.Since(startTime)
		log.Infof("Live analysis completed for path: %s, elapsed time: %v, goroutines: %d", path, elapsedTime, runtime.NumGoroutine())

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

// loadFromGduStorage loads cached results from gdu's built-in storage
func (e *Exporter) loadFromGduStorage() {
	if !e.useStorage || e.storagePath == "" {
		return
	}
	
	e.mu.Lock()
	defer e.mu.Unlock()
	
	if e.cachedResults == nil {
		e.cachedResults = make(map[string]fs.Item)
	}
	
	for path := range e.paths {
		// Create storage instance for this path
		storage := analyze.NewStorage(e.storagePath, path)
		closeFn := storage.Open()
		
		// Try to load from storage
		dir, err := storage.GetDirForPath(path)
		if err != nil {
			log.Debugf("Failed to load from gdu storage for path %s: %v", path, err)
			closeFn()
			continue
		}
		
		e.cachedResults[path] = dir
		log.Debugf("Loaded cache data from gdu storage for path: %s", path)
		closeFn()
	}
}

func (e *Exporter) loadFromStorage() {
	e.mu.RLock()
	defer e.mu.RUnlock()

	// Check if we have cached results in memory
	if e.cachedResults == nil {
		// Try to load from gdu storage if configured
		if e.useStorage {
			e.mu.RUnlock()
			e.loadFromGduStorage()
			e.mu.RLock()
		}
		
		// If still no cached data after trying to load from disk
		if e.cachedResults == nil {
			log.Debugf("No cached data available, returning empty metrics - storagePath: %s, scanInterval: %v, scanPaths: %v, followSymlinks: %v",
				e.storagePath, e.scanInterval, e.paths, e.followSymlinks)
			// Return empty metrics for all paths
			for path, level := range e.paths {
				diskUsage.WithLabelValues(path).Set(0)
				if level >= 1 {
					diskUsageLevel1.WithLabelValues(path).Set(0)
				}
			}
			return
		}
	}

	for path, level := range e.paths {
		dir, exists := e.cachedResults[path]
		if !exists || dir == nil {
			log.Debugf("No cached data found for path: %s, returning empty metrics - storagePath: %s, scanInterval: %v, followSymlinks: %v",
				path, e.storagePath, e.scanInterval, e.followSymlinks)
			// Return empty metrics (0 bytes) for missing cache data
			diskUsage.WithLabelValues(path).Set(0)
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
		diskUsage.WithLabelValues(item.GetPath()).Set(float64(item.GetUsage()))
	} else if level == 1 {
		diskUsageLevel1.WithLabelValues(item.GetPath()).Set(float64(item.GetUsage()))
	}

	if item.IsDir() && level+1 <= maxLevel {
		for _, entry := range item.GetFiles() {
			e.reportItem(entry, level+1, maxLevel)
		}
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
		// Load existing cache data from gdu storage on startup
		e.loadFromGduStorage()
		go e.runBackgroundScan()
		log.Printf("Background scan started with interval: %v", e.scanInterval)
	}
}

// Stop stops the background scanning
func (e *Exporter) Stop() {
	close(e.stopChan)
	if e.storageCloseFn != nil {
		e.storageCloseFn()
	}
	// Clean up cached results
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cachedResults = nil
	e.storage = nil
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
