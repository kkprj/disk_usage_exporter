package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3" // SQLite driver
	log "github.com/sirupsen/logrus"
)

// SQLiteStorage implements the Storage interface using SQLite as the backend
// Optimized for high-performance batch operations and concurrent read access
type SQLiteStorage struct {
	db       *sql.DB
	dbPath   string
	config   *StorageConfig
	
	// Prepared statements for performance
	insertStmt    *sql.Stmt
	selectStmt    *sql.Stmt
	
	// Batch transaction management
	batchTx       *sql.Tx
	batchStmt     *sql.Stmt
	batchCount    int
	batchMutex    sync.Mutex
	
	// Connection management
	closed        bool
	closeMutex    sync.RWMutex
}

// Database schema constants
const (
	// Main table for storing disk statistics
	createTableSQL = `
	CREATE TABLE IF NOT EXISTS disk_stats (
		path TEXT NOT NULL,
		level INTEGER NOT NULL,
		size INTEGER NOT NULL,
		file_count INTEGER NOT NULL,
		dir_count INTEGER NOT NULL,
		size_buckets TEXT, -- JSON encoded map of size_range -> count
		top_files TEXT,    -- JSON encoded array of TopFileInfo
		others_total INTEGER DEFAULT 0,
		others_count INTEGER DEFAULT 0,
		last_updated DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (path, level)
	);`

	// Performance indices
	createIndicesSQL = `
	CREATE INDEX IF NOT EXISTS idx_level ON disk_stats(level);
	CREATE INDEX IF NOT EXISTS idx_path_prefix ON disk_stats(path);
	CREATE INDEX IF NOT EXISTS idx_size_desc ON disk_stats(size DESC);
	CREATE INDEX IF NOT EXISTS idx_last_updated ON disk_stats(last_updated);`

	// Prepared statement queries
	insertSQL = `
	INSERT OR REPLACE INTO disk_stats 
	(path, level, size, file_count, dir_count, size_buckets, top_files, others_total, others_count, last_updated) 
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))`

	selectSQL = `
	SELECT path, level, size, file_count, dir_count, size_buckets, top_files, others_total, others_count, last_updated
	FROM disk_stats 
	WHERE level <= ? AND (`
)

// NewSQLiteStorage creates a new SQLite storage implementation
// Parameters:
//   - dbPath: path to SQLite database file
//   - config: storage configuration (nil for defaults)
// Returns configured SQLiteStorage instance and error
func NewSQLiteStorage(dbPath string, config *StorageConfig) (*SQLiteStorage, error) {
	if config == nil {
		config = DefaultStorageConfig()
		config.DBPath = dbPath
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %v", err)
	}

	// Create storage instance
	storage := &SQLiteStorage{
		dbPath: dbPath,
		config: config,
	}

	// Open database connection
	if err := storage.openConnection(); err != nil {
		return nil, fmt.Errorf("failed to open database: %v", err)
	}

	log.Infof("SQLite storage initialized at: %s", dbPath)
	return storage, nil
}

// openConnection establishes the database connection with optimized settings
func (s *SQLiteStorage) openConnection() error {
	// Build connection string with SQLite-specific parameters
	connStr := fmt.Sprintf("%s?cache=shared&mode=rwc&_journal_mode=WAL&_synchronous=NORMAL&_cache_size=%d&_temp_store=memory&_mmap_size=%d",
		s.dbPath, s.config.CacheSize, s.config.MemoryMapSize)

	db, err := sql.Open("sqlite3", connStr)
	if err != nil {
		return fmt.Errorf("failed to open SQLite database: %v", err)
	}

	// Configure connection pool
	db.SetMaxOpenConns(s.config.MaxConnections)
	db.SetMaxIdleConns(s.config.MaxConnections)
	db.SetConnMaxLifetime(time.Hour)

	// Test connection
	if err := db.Ping(); err != nil {
		db.Close()
		return fmt.Errorf("failed to ping database: %v", err)
	}

	s.db = db
	return nil
}

// Initialize sets up the database schema and optimizations
func (s *SQLiteStorage) Initialize() error {
	s.closeMutex.RLock()
	defer s.closeMutex.RUnlock()

	if s.closed {
		return fmt.Errorf("storage is closed")
	}

	// Create tables
	if _, err := s.db.Exec(createTableSQL); err != nil {
		return fmt.Errorf("failed to create tables: %v", err)
	}

	// Create indices
	if _, err := s.db.Exec(createIndicesSQL); err != nil {
		return fmt.Errorf("failed to create indices: %v", err)
	}

	// Apply performance optimizations
	if err := s.optimizeDatabase(); err != nil {
		return fmt.Errorf("failed to optimize database: %v", err)
	}

	// Prepare statements
	if err := s.prepareStatements(); err != nil {
		return fmt.Errorf("failed to prepare statements: %v", err)
	}

	log.Info("SQLite storage initialized successfully")
	return nil
}

// optimizeDatabase applies SQLite performance optimizations
func (s *SQLiteStorage) optimizeDatabase() error {
	optimizations := []string{
		"PRAGMA journal_mode=WAL",           // Enable Write-Ahead Logging
		"PRAGMA synchronous=NORMAL",         // Balance performance vs safety
		fmt.Sprintf("PRAGMA cache_size=%d", s.config.CacheSize), // Set cache size
		"PRAGMA temp_store=memory",          // Store temp tables in memory
		fmt.Sprintf("PRAGMA mmap_size=%d", s.config.MemoryMapSize), // Memory mapping
		"PRAGMA optimize",                   // Optimize query planner
	}

	// Apply auto vacuum if enabled
	if s.config.AutoVacuum {
		optimizations = append(optimizations, "PRAGMA auto_vacuum=INCREMENTAL")
	}

	for _, pragma := range optimizations {
		if _, err := s.db.Exec(pragma); err != nil {
			log.Warnf("Failed to apply optimization '%s': %v", pragma, err)
		}
	}

	return nil
}

// prepareStatements creates prepared statements for performance
func (s *SQLiteStorage) prepareStatements() error {
	var err error

	// Prepare insert statement
	s.insertStmt, err = s.db.Prepare(insertSQL)
	if err != nil {
		return fmt.Errorf("failed to prepare insert statement: %v", err)
	}

	return nil
}

// StoreBatch efficiently stores multiple DiskStat records using transactions
func (s *SQLiteStorage) StoreBatch(stats []DiskStat) error {
	s.closeMutex.RLock()
	defer s.closeMutex.RUnlock()

	if s.closed {
		return fmt.Errorf("storage is closed")
	}

	if len(stats) == 0 {
		return nil
	}

	// Use transaction for batch insert
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %v", err)
	}
	defer tx.Rollback()

	// Prepare statement within transaction
	stmt, err := tx.Prepare(insertSQL)
	if err != nil {
		return fmt.Errorf("failed to prepare statement: %v", err)
	}
	defer stmt.Close()

	// Insert each record
	for _, stat := range stats {
		// Serialize complex fields to JSON
		sizeBucketsJSON, err := json.Marshal(stat.SizeBuckets)
		if err != nil {
			log.Warnf("Failed to marshal size buckets for path %s: %v", stat.Path, err)
			sizeBucketsJSON = []byte("{}")
		}

		topFilesJSON, err := json.Marshal(stat.TopFiles)
		if err != nil {
			log.Warnf("Failed to marshal top files for path %s: %v", stat.Path, err)
			topFilesJSON = []byte("[]")
		}

		// Execute insert
		_, err = stmt.Exec(
			stat.Path,
			stat.Level,
			stat.Size,
			stat.FileCount,
			stat.DirCount,
			string(sizeBucketsJSON),
			string(topFilesJSON),
			stat.OthersTotal,
			stat.OthersCount,
		)
		if err != nil {
			return fmt.Errorf("failed to insert record for path %s: %v", stat.Path, err)
		}
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit batch: %v", err)
	}

	log.Debugf("Successfully stored batch of %d records", len(stats))
	return nil
}

// GetMetricsData retrieves statistics for specified paths and levels
func (s *SQLiteStorage) GetMetricsData(paths map[string]int) ([]DiskStat, error) {
	s.closeMutex.RLock()
	defer s.closeMutex.RUnlock()

	if s.closed {
		return nil, fmt.Errorf("storage is closed")
	}

	if len(paths) == 0 {
		return []DiskStat{}, nil
	}

	// Build dynamic query for multiple paths
	var conditions []string
	var args []interface{}

	// Find maximum level across all paths
	maxLevel := 0
	for _, level := range paths {
		if level > maxLevel {
			maxLevel = level
		}
	}
	args = append(args, maxLevel)

	// Add path conditions
	for rootPath, maxPathLevel := range paths {
		conditions = append(conditions, "(path = ? OR path LIKE ?) AND level <= ?")
		args = append(args, rootPath, rootPath+"/%", maxPathLevel)
	}

	// Complete query
	query := selectSQL + strings.Join(conditions, " OR ") + ") ORDER BY path, level"

	// Execute query
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to execute query: %v", err)
	}
	defer rows.Close()

	// Parse results
	var results []DiskStat
	for rows.Next() {
		var stat DiskStat
		var sizeBucketsJSON, topFilesJSON string
		var lastUpdated string

		err := rows.Scan(
			&stat.Path,
			&stat.Level,
			&stat.Size,
			&stat.FileCount,
			&stat.DirCount,
			&sizeBucketsJSON,
			&topFilesJSON,
			&stat.OthersTotal,
			&stat.OthersCount,
			&lastUpdated,
		)
		if err != nil {
			log.Warnf("Failed to scan row: %v", err)
			continue
		}

		// Parse JSON fields
		if err := json.Unmarshal([]byte(sizeBucketsJSON), &stat.SizeBuckets); err != nil {
			log.Warnf("Failed to unmarshal size buckets for path %s: %v", stat.Path, err)
			stat.SizeBuckets = make(map[string]int64)
		}

		if err := json.Unmarshal([]byte(topFilesJSON), &stat.TopFiles); err != nil {
			log.Warnf("Failed to unmarshal top files for path %s: %v", stat.Path, err)
			stat.TopFiles = make([]TopFileInfo, 0)
		}

		// Parse timestamp
		if parsedTime, err := time.Parse("2006-01-02 15:04:05", lastUpdated); err == nil {
			stat.LastUpdated = parsedTime
		} else {
			stat.LastUpdated = time.Now()
		}

		results = append(results, stat)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating rows: %v", err)
	}

	log.Debugf("Retrieved %d records for %d paths", len(results), len(paths))
	return results, nil
}

// Close properly shuts down the storage and releases resources
func (s *SQLiteStorage) Close() error {
	s.closeMutex.Lock()
	defer s.closeMutex.Unlock()

	if s.closed {
		return nil
	}

	// Close prepared statements
	if s.insertStmt != nil {
		s.insertStmt.Close()
	}
	if s.selectStmt != nil {
		s.selectStmt.Close()
	}

	// Close any open batch transaction
	s.batchMutex.Lock()
	if s.batchTx != nil {
		s.batchTx.Rollback()
		s.batchTx = nil
	}
	if s.batchStmt != nil {
		s.batchStmt.Close()
		s.batchStmt = nil
	}
	s.batchMutex.Unlock()

	// Checkpoint WAL if enabled
	if s.config.CheckpointWAL {
		if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			log.Warnf("Failed to checkpoint WAL: %v", err)
		}
	}

	// Close database connection
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("failed to close database: %v", err)
	}

	s.closed = true
	log.Info("SQLite storage closed successfully")
	return nil
}

// Cleanup performs database maintenance operations
func (s *SQLiteStorage) Cleanup() error {
	s.closeMutex.RLock()
	defer s.closeMutex.RUnlock()

	if s.closed {
		return fmt.Errorf("storage is closed")
	}

	// Analyze database for optimization
	if _, err := s.db.Exec("ANALYZE"); err != nil {
		log.Warnf("Failed to analyze database: %v", err)
	}

	// Incremental vacuum if auto vacuum is enabled
	if s.config.AutoVacuum {
		if _, err := s.db.Exec("PRAGMA incremental_vacuum"); err != nil {
			log.Warnf("Failed to perform incremental vacuum: %v", err)
		}
	}

	// Optimize database
	if _, err := s.db.Exec("PRAGMA optimize"); err != nil {
		log.Warnf("Failed to optimize database: %v", err)
	}

	// Checkpoint WAL
	if s.config.CheckpointWAL {
		if _, err := s.db.Exec("PRAGMA wal_checkpoint(PASSIVE)"); err != nil {
			log.Warnf("Failed to checkpoint WAL: %v", err)
		}
	}

	log.Debug("Database cleanup completed")
	return nil
}

// GetDatabaseInfo returns diagnostic information about the database
func (s *SQLiteStorage) GetDatabaseInfo() (map[string]interface{}, error) {
	s.closeMutex.RLock()
	defer s.closeMutex.RUnlock()

	if s.closed {
		return nil, fmt.Errorf("storage is closed")
	}

	info := make(map[string]interface{})

	// Get database size
	if stat, err := os.Stat(s.dbPath); err == nil {
		info["file_size_bytes"] = stat.Size()
	}

	// Get record count
	var count int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM disk_stats").Scan(&count); err == nil {
		info["record_count"] = count
	}

	// Get WAL info
	var walSize int64
	if err := s.db.QueryRow("PRAGMA wal_checkpoint").Scan(&walSize); err == nil {
		info["wal_size"] = walSize
	}

	// Get cache hit ratio
	var cacheHits, cacheMisses int64
	s.db.QueryRow("PRAGMA cache_hit_ratio").Scan(&cacheHits)
	s.db.QueryRow("PRAGMA cache_miss_ratio").Scan(&cacheMisses)
	if (cacheHits + cacheMisses) > 0 {
		info["cache_hit_ratio"] = float64(cacheHits) / float64(cacheHits+cacheMisses)
	}

	return info, nil
}