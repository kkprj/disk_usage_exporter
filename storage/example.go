package storage

import (
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
)

// ExampleUsage demonstrates how to use the SQLite storage implementation
func ExampleUsage() {
	// Create storage with custom configuration
	config := &StorageConfig{
		DBPath:           "/tmp/disk-usage-example.db",
		MaxConnections:   1,
		BatchSize:        500,
		EnableWAL:        true,
		CacheSize:        5000, // 5MB cache
		MemoryMapSize:    128 * 1024 * 1024, // 128MB memory map
		AutoVacuum:       true,
		CheckpointWAL:    true,
		SyncMode:         "NORMAL",
	}

	// Initialize storage
	storage, err := NewSQLiteStorage(config.DBPath, config)
	if err != nil {
		log.Fatal("Failed to create storage:", err)
	}
	defer storage.Close()

	// Initialize database schema
	if err := storage.Initialize(); err != nil {
		log.Fatal("Failed to initialize storage:", err)
	}

	// Create sample data
	sampleStats := []DiskStat{
		{
			Path:        "/home/user",
			Level:       0,
			Size:        10 * 1024 * 1024 * 1024, // 10GB
			FileCount:   50000,
			DirCount:    500,
			SizeBuckets: map[string]int64{
				SizeRange0To1KB:    15000,
				SizeRange1KBTo1MB:  30000,
				SizeRange1MBTo100MB: 5000,
			},
			TopFiles: []TopFileInfo{
				{Path: "/home/user/large_video.mp4", Size: 2 * 1024 * 1024 * 1024, Rank: 1},
				{Path: "/home/user/backup.tar.gz", Size: 1024 * 1024 * 1024, Rank: 2},
				{Path: "/home/user/presentation.pptx", Size: 500 * 1024 * 1024, Rank: 3},
			},
			OthersTotal: 6596 * 1024 * 1024, // Remaining ~6.6GB
			OthersCount: 49997,              // Remaining files
			LastUpdated: time.Now(),
		},
		{
			Path:        "/home/user/Documents",
			Level:       1,
			Size:        2 * 1024 * 1024 * 1024, // 2GB
			FileCount:   10000,
			DirCount:    100,
			SizeBuckets: map[string]int64{
				SizeRange0To1KB:    3000,
				SizeRange1KBTo1MB:  6500,
				SizeRange1MBTo100MB: 500,
			},
			TopFiles: []TopFileInfo{
				{Path: "/home/user/Documents/project.zip", Size: 200 * 1024 * 1024, Rank: 1},
				{Path: "/home/user/Documents/database.db", Size: 150 * 1024 * 1024, Rank: 2},
			},
			OthersTotal: 1674 * 1024 * 1024, // Remaining ~1.67GB
			OthersCount: 9998,               // Remaining files
			LastUpdated: time.Now(),
		},
		{
			Path:        "/home/user/Pictures",
			Level:       1,
			Size:        5 * 1024 * 1024 * 1024, // 5GB
			FileCount:   2000,
			DirCount:    20,
			SizeBuckets: map[string]int64{
				SizeRange1MBTo100MB: 1800,
				SizeRange100MBPlus:  200,
			},
			TopFiles: []TopFileInfo{
				{Path: "/home/user/Pictures/vacation_2024.zip", Size: 800 * 1024 * 1024, Rank: 1},
				{Path: "/home/user/Pictures/wedding_photos.zip", Size: 600 * 1024 * 1024, Rank: 2},
			},
			OthersTotal: 3696 * 1024 * 1024, // Remaining ~3.7GB
			OthersCount: 1998,               // Remaining files
			LastUpdated: time.Now(),
		},
	}

	// Store data in batch
	fmt.Println("Storing sample data...")
	if err := storage.StoreBatch(sampleStats); err != nil {
		log.Fatal("Failed to store batch:", err)
	}

	// Query data back
	fmt.Println("Querying stored data...")
	paths := map[string]int{
		"/home/user": 2, // Get up to level 2
	}

	results, err := storage.GetMetricsData(paths)
	if err != nil {
		log.Fatal("Failed to query data:", err)
	}

	// Display results
	fmt.Printf("Retrieved %d records:\n", len(results))
	for _, stat := range results {
		fmt.Printf("Path: %s (Level %d)\n", stat.Path, stat.Level)
		fmt.Printf("  Size: %.2f GB\n", float64(stat.Size)/(1024*1024*1024))
		fmt.Printf("  Files: %d, Directories: %d\n", stat.FileCount, stat.DirCount)
		fmt.Printf("  Size Buckets: %d categories\n", len(stat.SizeBuckets))
		fmt.Printf("  Top Files: %d files\n", len(stat.TopFiles))
		fmt.Printf("  Others: %d files (%.2f GB)\n", stat.OthersCount, float64(stat.OthersTotal)/(1024*1024*1024))
		fmt.Printf("  Last Updated: %s\n", stat.LastUpdated.Format("2006-01-02 15:04:05"))
		fmt.Println()
	}

	// Get database information
	fmt.Println("Database information:")
	info, err := storage.GetDatabaseInfo()
	if err != nil {
		log.Warn("Failed to get database info:", err)
	} else {
		for key, value := range info {
			fmt.Printf("  %s: %v\n", key, value)
		}
	}

	// Perform cleanup
	fmt.Println("\nPerforming database cleanup...")
	if err := storage.Cleanup(); err != nil {
		log.Warn("Cleanup failed:", err)
	} else {
		fmt.Println("Database cleanup completed successfully")
	}
}

// BenchmarkStorageOperations demonstrates performance characteristics
func BenchmarkStorageOperations() {
	storage, err := NewSQLiteStorage(":memory:", nil) // Use in-memory database for benchmarking
	if err != nil {
		log.Fatal("Failed to create storage:", err)
	}
	defer storage.Close()

	if err := storage.Initialize(); err != nil {
		log.Fatal("Failed to initialize storage:", err)
	}

	// Generate large dataset
	const numRecords = 10000
	stats := make([]DiskStat, numRecords)
	
	for i := 0; i < numRecords; i++ {
		stats[i] = DiskStat{
			Path:        fmt.Sprintf("/test/path/level0/level1/dir%d", i),
			Level:       2,
			Size:        int64(i * 1000),
			FileCount:   int64(i % 100),
			DirCount:    int64(i % 10),
			SizeBuckets: map[string]int64{GetSizeRange(int64(i * 1000)): 1},
			TopFiles:    []TopFileInfo{{Path: fmt.Sprintf("/test/file%d.txt", i), Size: int64(i * 500), Rank: 1}},
			OthersTotal: int64(i * 500),
			OthersCount: int64(i % 50),
			LastUpdated: time.Now(),
		}
	}

	// Benchmark batch insert
	start := time.Now()
	if err := storage.StoreBatch(stats); err != nil {
		log.Fatal("Batch insert failed:", err)
	}
	insertDuration := time.Since(start)

	// Benchmark query
	start = time.Now()
	paths := map[string]int{"/test/path": 3}
	results, err := storage.GetMetricsData(paths)
	if err != nil {
		log.Fatal("Query failed:", err)
	}
	queryDuration := time.Since(start)

	fmt.Printf("Performance Results:\n")
	fmt.Printf("  Records inserted: %d\n", numRecords)
	fmt.Printf("  Insert time: %v\n", insertDuration)
	fmt.Printf("  Insert rate: %.0f records/sec\n", float64(numRecords)/insertDuration.Seconds())
	fmt.Printf("  Records retrieved: %d\n", len(results))
	fmt.Printf("  Query time: %v\n", queryDuration)
	fmt.Printf("  Query rate: %.0f records/sec\n", float64(len(results))/queryDuration.Seconds())
}