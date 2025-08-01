package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSQLiteStorage_BasicOperations(t *testing.T) {
	// Create temporary database file
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	// Create storage instance
	storage, err := NewSQLiteStorage(dbPath, nil)
	require.NoError(t, err)
	defer storage.Close()

	// Initialize database
	err = storage.Initialize()
	require.NoError(t, err)

	// Test data
	testStats := []DiskStat{
		{
			Path:        "/test",
			Level:       0,
			Size:        1000,
			FileCount:   10,
			DirCount:    2,
			SizeBuckets: map[string]int64{"1KB-1MB": 5, "1MB-100MB": 3},
			TopFiles: []TopFileInfo{
				{Path: "/test/large1.txt", Size: 500, Rank: 1},
				{Path: "/test/large2.txt", Size: 300, Rank: 2},
			},
			OthersTotal: 200,
			OthersCount: 8,
			LastUpdated: time.Now(),
		},
		{
			Path:        "/test/sub",
			Level:       1,
			Size:        500,
			FileCount:   5,
			DirCount:    1,
			SizeBuckets: map[string]int64{"0-1KB": 2, "1KB-1MB": 3},
			TopFiles:    []TopFileInfo{{Path: "/test/sub/file.txt", Size: 200, Rank: 1}},
			OthersTotal: 300,
			OthersCount: 4,
			LastUpdated: time.Now(),
		},
	}

	// Test StoreBatch
	err = storage.StoreBatch(testStats)
	assert.NoError(t, err)

	// Test GetMetricsData
	paths := map[string]int{"/test": 2}
	results, err := storage.GetMetricsData(paths)
	assert.NoError(t, err)
	assert.Len(t, results, 2)

	// Verify data integrity
	for _, result := range results {
		if result.Path == "/test" {
			assert.Equal(t, int64(1000), result.Size)
			assert.Equal(t, int64(10), result.FileCount)
			assert.Equal(t, int64(2), result.DirCount)
			assert.Len(t, result.SizeBuckets, 2)
			assert.Len(t, result.TopFiles, 2)
			assert.Equal(t, int64(200), result.OthersTotal)
			assert.Equal(t, int64(8), result.OthersCount)
		} else if result.Path == "/test/sub" {
			assert.Equal(t, int64(500), result.Size)
			assert.Equal(t, int64(5), result.FileCount)
			assert.Equal(t, int64(1), result.DirCount)
			assert.Len(t, result.SizeBuckets, 2)
			assert.Len(t, result.TopFiles, 1)
		}
	}
}

func TestSQLiteStorage_EmptyBatch(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	storage, err := NewSQLiteStorage(dbPath, nil)
	require.NoError(t, err)
	defer storage.Close()

	err = storage.Initialize()
	require.NoError(t, err)

	// Test empty batch
	err = storage.StoreBatch([]DiskStat{})
	assert.NoError(t, err)

	// Test empty query
	results, err := storage.GetMetricsData(map[string]int{})
	assert.NoError(t, err)
	assert.Empty(t, results)
}

func TestSQLiteStorage_LevelFiltering(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	storage, err := NewSQLiteStorage(dbPath, nil)
	require.NoError(t, err)
	defer storage.Close()

	err = storage.Initialize()
	require.NoError(t, err)

	// Create test data with different levels
	testStats := []DiskStat{
		{Path: "/test", Level: 0, Size: 1000, FileCount: 10, SizeBuckets: make(map[string]int64), TopFiles: make([]TopFileInfo, 0)},
		{Path: "/test/level1", Level: 1, Size: 500, FileCount: 5, SizeBuckets: make(map[string]int64), TopFiles: make([]TopFileInfo, 0)},
		{Path: "/test/level1/level2", Level: 2, Size: 200, FileCount: 2, SizeBuckets: make(map[string]int64), TopFiles: make([]TopFileInfo, 0)},
		{Path: "/test/level1/level2/level3", Level: 3, Size: 100, FileCount: 1, SizeBuckets: make(map[string]int64), TopFiles: make([]TopFileInfo, 0)},
	}

	err = storage.StoreBatch(testStats)
	require.NoError(t, err)

	// Test level filtering
	paths := map[string]int{"/test": 2} // Only get levels 0, 1, 2
	results, err := storage.GetMetricsData(paths)
	assert.NoError(t, err)
	assert.Len(t, results, 3) // Should exclude level 3

	// Verify no level 3 results
	for _, result := range results {
		assert.LessOrEqual(t, result.Level, 2)
	}
}

func TestSQLiteStorage_Cleanup(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	storage, err := NewSQLiteStorage(dbPath, nil)
	require.NoError(t, err)
	defer storage.Close()

	err = storage.Initialize()
	require.NoError(t, err)

	// Test cleanup operations
	err = storage.Cleanup()
	assert.NoError(t, err)
}

func TestSQLiteStorage_DatabaseInfo(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	storage, err := NewSQLiteStorage(dbPath, nil)
	require.NoError(t, err)
	defer storage.Close()

	err = storage.Initialize()
	require.NoError(t, err)

	// Get database info
	info, err := storage.GetDatabaseInfo()
	assert.NoError(t, err)
	assert.Contains(t, info, "file_size_bytes")
	assert.Contains(t, info, "record_count")
}

func TestSQLiteStorage_ConcurrentAccess(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	storage, err := NewSQLiteStorage(dbPath, nil)
	require.NoError(t, err)
	defer storage.Close()

	err = storage.Initialize()
	require.NoError(t, err)

	// Test concurrent writes (should be serialized by SQLite)
	done := make(chan bool, 2)

	go func() {
		stats := []DiskStat{{Path: "/test1", Level: 0, Size: 100, SizeBuckets: make(map[string]int64), TopFiles: make([]TopFileInfo, 0)}}
		storage.StoreBatch(stats)
		done <- true
	}()

	go func() {
		stats := []DiskStat{{Path: "/test2", Level: 0, Size: 200, SizeBuckets: make(map[string]int64), TopFiles: make([]TopFileInfo, 0)}}
		storage.StoreBatch(stats)
		done <- true
	}()

	// Wait for both goroutines
	<-done
	<-done

	// Verify both records exist
	results, err := storage.GetMetricsData(map[string]int{"/test1": 1, "/test2": 1})
	assert.NoError(t, err)
	assert.Len(t, results, 2)
}

func TestGetSizeRange(t *testing.T) {
	tests := []struct {
		size     int64
		expected string
	}{
		{0, SizeRange0B},
		{512, SizeRange0To1KB},
		{1024, SizeRange0To1KB},
		{2048, SizeRange1KBTo1MB},
		{1024 * 1024, SizeRange1KBTo1MB},
		{2 * 1024 * 1024, SizeRange1MBTo100MB},
		{100 * 1024 * 1024, SizeRange1MBTo100MB},
		{200 * 1024 * 1024, SizeRange100MBPlus},
	}

	for _, test := range tests {
		result := GetSizeRange(test.size)
		assert.Equal(t, test.expected, result, "Size %d should be in range %s", test.size, test.expected)
	}
}

func TestNewDiskStat(t *testing.T) {
	stat := NewDiskStat("/test/path", 2)
	
	assert.Equal(t, "/test/path", stat.Path)
	assert.Equal(t, 2, stat.Level)
	assert.NotNil(t, stat.SizeBuckets)
	assert.NotNil(t, stat.TopFiles)
	assert.WithinDuration(t, time.Now(), stat.LastUpdated, time.Second)
}

func TestDefaultStorageConfig(t *testing.T) {
	config := DefaultStorageConfig()
	
	assert.Equal(t, "/tmp/disk-usage.db", config.DBPath)
	assert.Equal(t, 1, config.MaxConnections)
	assert.Equal(t, 1000, config.BatchSize)
	assert.True(t, config.EnableWAL)
	assert.Equal(t, 10000, config.CacheSize)
	assert.Equal(t, int64(268435456), config.MemoryMapSize)
	assert.True(t, config.AutoVacuum)
	assert.True(t, config.CheckpointWAL)
	assert.Equal(t, "NORMAL", config.SyncMode)
}