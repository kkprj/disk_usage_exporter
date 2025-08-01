package exporter

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dundee/disk_usage_exporter/storage"
)

func TestSQLiteMigrationIntegration(t *testing.T) {
	// Setup temporary environment
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test-disk-usage.db")

	// Create test directory structure
	testRoot := filepath.Join(tmpDir, "test-root")
	err := os.MkdirAll(filepath.Join(testRoot, "subdir1", "subdir2"), 0755)
	require.NoError(t, err)

	// Create test files
	testFiles := []struct {
		path string
		size int64
	}{
		{filepath.Join(testRoot, "large.txt"), 1024 * 1024},     // 1MB
		{filepath.Join(testRoot, "subdir1", "medium.txt"), 1024}, // 1KB
		{filepath.Join(testRoot, "subdir1", "subdir2", "small.txt"), 100}, // 100B
	}

	for _, tf := range testFiles {
		file, err := os.Create(tf.path)
		require.NoError(t, err)
		
		// Write dummy data
		data := make([]byte, tf.size)
		for i := range data {
			data[i] = byte(i % 256)
		}
		_, err = file.Write(data)
		require.NoError(t, err)
		file.Close()
	}

	// Create SQLite-based exporter
	pathMap := map[string]int{testRoot: 3}
	exporter := NewExporter(pathMap, false)

	// Configure SQLite storage
	err = exporter.SetSQLiteStorage(dbPath, 100)
	require.NoError(t, err)

	// Perform scan
	exporter.performScan()

	// Verify database contains data
	dbStorage, ok := exporter.sqliteStorage.(*storage.SQLiteStorage)
	require.True(t, ok, "Expected SQLite storage")

	results, err := dbStorage.GetMetricsData(pathMap)
	require.NoError(t, err)
	assert.NotEmpty(t, results, "Database should contain scan results")

	// Verify metrics can be published
	registry := prometheus.NewRegistry()
	diskUsage := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "disk_usage_bytes",
			Help: "Disk usage in bytes",
		},
		[]string{"path", "level"},
	)
	diskUsageSizeBucket := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "disk_usage_size_bucket",
			Help: "File count by size bucket",
		},
		[]string{"path", "level", "size_range"},
	)

	registry.MustRegister(diskUsage, diskUsageSizeBucket)

	// Simulate metrics publishing by testing if we can gather data
	// The actual publishing method would be private or not exist yet

	// For now, just verify we can collect data from storage
	// In the future, actual metrics publishing would be tested here

	// Test database persistence - reopen and verify data persists
	newExporter := NewExporter(pathMap, false)

	err = newExporter.SetSQLiteStorage(dbPath, 100)
	require.NoError(t, err)

	newDbStorage, ok := newExporter.sqliteStorage.(*storage.SQLiteStorage)
	require.True(t, ok, "Expected SQLite storage")

	newResults, err := newDbStorage.GetMetricsData(pathMap)
	require.NoError(t, err)
	assert.Len(t, newResults, len(results), "Data should persist after restart")
}

func TestSQLiteExporterPerformance(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "perf-test.db")

	// Create larger test structure
	testRoot := filepath.Join(tmpDir, "perf-root")
	
	// Create directory structure with more files
	for i := 0; i < 10; i++ {
		dirPath := filepath.Join(testRoot, fmt.Sprintf("dir_%d", i))
		err := os.MkdirAll(dirPath, 0755)
		require.NoError(t, err)

		// Create files in each directory
		for j := 0; j < 5; j++ {
			filePath := filepath.Join(dirPath, fmt.Sprintf("file_%d.txt", j))
			err := os.WriteFile(filePath, []byte(fmt.Sprintf("content_%d_%d", i, j)), 0644)
			require.NoError(t, err)
		}
	}

	exporter := NewExporter(map[string]int{testRoot: 2}, false)

	// Configure SQLite storage
	err := exporter.SetSQLiteStorage(dbPath, 50)
	require.NoError(t, err)

	// Measure scan performance
	start := time.Now()
	exporter.performScan()
	scanDuration := time.Since(start)

	t.Logf("Scan completed in %v", scanDuration)

	// Verify reasonable performance (should complete within reasonable time)
	assert.Less(t, scanDuration, 30*time.Second, "Scan should complete within 30 seconds")

	// Get database info
	sqliteStorage, ok := exporter.sqliteStorage.(*storage.SQLiteStorage)
	require.True(t, ok)

	info, err := sqliteStorage.GetDatabaseInfo()
	require.NoError(t, err)
	t.Logf("Database info: %s", info)
}

func TestSQLiteErrorHandling(t *testing.T) {
	tmpDir := t.TempDir()
	
	// Test with invalid database path
	invalidPath := filepath.Join("/nonexistent", "path", "test.db")
	exporter := NewExporter(map[string]int{tmpDir: 1}, false)

	err := exporter.SetSQLiteStorage(invalidPath, 100)
	assert.Error(t, err, "Should fail with invalid database path")

	// Test with valid path but no permissions
	readOnlyDir := filepath.Join(tmpDir, "readonly")
	err = os.MkdirAll(readOnlyDir, 0555) // read+execute only
	require.NoError(t, err)
	defer os.Chmod(readOnlyDir, 0755) // restore permissions for cleanup

	readOnlyDBPath := filepath.Join(readOnlyDir, "test.db")
	exporter2 := NewExporter(map[string]int{tmpDir: 1}, false)

	err = exporter2.SetSQLiteStorage(readOnlyDBPath, 100)
	assert.Error(t, err, "Should fail with insufficient permissions")
}

func TestSQLiteCleanupOperations(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "cleanup-test.db")

	exporter := NewExporter(map[string]int{tmpDir: 1}, false)

	// Configure SQLite storage
	err := exporter.SetSQLiteStorage(dbPath, 100)
	require.NoError(t, err)

	// Perform scan to populate database
	exporter.performScan()

	// Test cleanup operations
	err = exporter.sqliteStorage.Cleanup()
	assert.NoError(t, err, "Cleanup should succeed")

	// Verify database still works after cleanup
	results, err := exporter.sqliteStorage.GetMetricsData(map[string]int{tmpDir: 1})
	assert.NoError(t, err, "Should be able to query after cleanup")
	assert.NotNil(t, results, "Results should not be nil after cleanup")

	// Verify database file exists and is readable
	info, err := os.Stat(dbPath)
	assert.NoError(t, err, "Database file should exist")
	assert.Greater(t, info.Size(), int64(0), "Database file should not be empty")
}