package storage

// Storage defines the interface for disk usage statistics storage backend
// This interface abstracts the storage layer to allow different implementations
// (SQLite, in-memory, etc.) while providing consistent functionality
type Storage interface {
	// Initialize sets up the storage backend (creates tables, indices, etc.)
	// Returns error if initialization fails
	Initialize() error

	// StoreBatch efficiently stores multiple DiskStat records in a single transaction
	// This is the primary method for bulk data insertion during directory scans
	// Parameters:
	//   - stats: slice of DiskStat records to store
	// Returns error if storage operation fails
	StoreBatch(stats []DiskStat) error

	// GetMetricsData retrieves disk statistics for specified paths and their subdirectories
	// Used by the metrics collection system to generate Prometheus metrics
	// Parameters:
	//   - paths: map of root path -> max level depth for each monitored path
	// Returns:
	//   - []DiskStat: matching statistics records
	//   - error: if query fails
	GetMetricsData(paths map[string]int) ([]DiskStat, error)

	// Close properly shuts down the storage backend and releases resources
	// Should be called during application shutdown
	// Returns error if cleanup fails
	Close() error

	// Cleanup performs maintenance operations (vacuum, optimize indices, etc.)
	// Can be called periodically to maintain storage performance
	// Returns error if cleanup operations fail
	Cleanup() error
}

// StorageConfig holds configuration parameters for storage implementations
type StorageConfig struct {
	// Database connection settings
	DBPath           string // Path to SQLite database file
	MaxConnections   int    // Maximum number of concurrent connections
	ConnectionTimeout int   // Connection timeout in seconds

	// Performance settings
	BatchSize        int  // Number of records per batch operation
	EnableWAL        bool // Enable Write-Ahead Logging mode
	CacheSize        int  // Cache size in KB
	MemoryMapSize    int64 // Memory map size in bytes

	// Maintenance settings
	AutoVacuum       bool // Enable automatic database vacuuming
	CheckpointWAL    bool // Enable periodic WAL checkpointing
	SyncMode         string // Synchronous mode: OFF, NORMAL, FULL
}

// DefaultStorageConfig returns a production-ready configuration with optimized settings
func DefaultStorageConfig() *StorageConfig {
	return &StorageConfig{
		DBPath:           "/tmp/disk-usage.db",
		MaxConnections:   1, // SQLite works best with single writer
		ConnectionTimeout: 30,
		
		BatchSize:        1000,
		EnableWAL:        true,
		CacheSize:        10000, // 10MB cache
		MemoryMapSize:    268435456, // 256MB memory map
		
		AutoVacuum:       true,
		CheckpointWAL:    true,
		SyncMode:         "NORMAL", // Balance between performance and safety
	}
}