package storage

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkSQLiteStorage_StoreBatch(b *testing.B) {
	tmpDir := b.TempDir()
	dbPath := filepath.Join(tmpDir, "benchmark.db")

	storage, err := NewSQLiteStorage(dbPath, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer storage.Close()

	err = storage.Initialize()
	if err != nil {
		b.Fatal(err)
	}

	// Generate test data
	stats := make([]DiskStat, 1000)
	for i := range stats {
		stats[i] = DiskStat{
			Path:        fmt.Sprintf("/test/path/%d", i),
			Level:       i % 5,
			Size:        int64(i * 1000),
			FileCount:   int64(i % 100),
			DirCount:    int64(i % 10),
			SizeBuckets: map[string]int64{
				SizeRange0To1KB:    int64(i % 10),
				SizeRange1KBTo1MB:  int64(i % 20),
				SizeRange1MBTo100MB: int64(i % 5),
			},
			TopFiles: []TopFileInfo{
				{Path: fmt.Sprintf("/test/path/%d/file1.txt", i), Size: int64(i * 100), Rank: 1},
				{Path: fmt.Sprintf("/test/path/%d/file2.txt", i), Size: int64(i * 50), Rank: 2},
			},
			OthersTotal: int64(i * 10),
			OthersCount: int64(i % 50),
			LastUpdated: time.Now(),
		}
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		err := storage.StoreBatch(stats)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSQLiteStorage_GetMetricsData(b *testing.B) {
	tmpDir := b.TempDir()
	dbPath := filepath.Join(tmpDir, "benchmark.db")

	storage, err := NewSQLiteStorage(dbPath, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer storage.Close()

	err = storage.Initialize()
	if err != nil {
		b.Fatal(err)
	}

	// Populate database with test data
	stats := make([]DiskStat, 10000)
	for i := range stats {
		stats[i] = DiskStat{
			Path:        fmt.Sprintf("/test/path/%d", i),
			Level:       i % 5,
			Size:        int64(i * 1000),
			FileCount:   int64(i % 100),
			DirCount:    int64(i % 10),
			SizeBuckets: map[string]int64{
				SizeRange0To1KB:    int64(i % 10),
				SizeRange1KBTo1MB:  int64(i % 20),
			},
			TopFiles: []TopFileInfo{
				{Path: fmt.Sprintf("/test/path/%d/file.txt", i), Size: int64(i * 100), Rank: 1},
			},
			OthersTotal: int64(i * 10),
			OthersCount: int64(i % 50),
			LastUpdated: time.Now(),
		}
	}

	err = storage.StoreBatch(stats)
	if err != nil {
		b.Fatal(err)
	}

	paths := map[string]int{
		"/test/path": 3,
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := storage.GetMetricsData(paths)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSQLiteStorage_ConcurrentAccess(b *testing.B) {
	tmpDir := b.TempDir()
	dbPath := filepath.Join(tmpDir, "concurrent.db")

	storage, err := NewSQLiteStorage(dbPath, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer storage.Close()

	err = storage.Initialize()
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			stats := []DiskStat{{
				Path:        fmt.Sprintf("/concurrent/path/%d", i),
				Level:       i % 3,
				Size:        int64(i * 1000),
				FileCount:   int64(i % 50),
				SizeBuckets: map[string]int64{SizeRange1KBTo1MB: int64(i % 10)},
				TopFiles:    []TopFileInfo{{Path: fmt.Sprintf("/file%d.txt", i), Size: int64(i * 100), Rank: 1}},
				OthersTotal: int64(i * 5),
				OthersCount: int64(i % 25),
				LastUpdated: time.Now(),
			}}

			err := storage.StoreBatch(stats)
			if err != nil {
				b.Fatal(err)
			}
			i++
		}
	})
}

func BenchmarkSQLiteStorage_LargeDataset(b *testing.B) {
	tmpDir := b.TempDir()
	dbPath := filepath.Join(tmpDir, "large.db")

	storage, err := NewSQLiteStorage(dbPath, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer storage.Close()

	err = storage.Initialize()
	if err != nil {
		b.Fatal(err)
	}

	// Test with various batch sizes
	batchSizes := []int{100, 500, 1000, 5000}

	for _, batchSize := range batchSizes {
		b.Run(fmt.Sprintf("BatchSize_%d", batchSize), func(b *testing.B) {
			stats := make([]DiskStat, batchSize)
			for i := range stats {
				stats[i] = DiskStat{
					Path:        fmt.Sprintf("/large/dataset/path/%d", i),
					Level:       i % 10,
					Size:        int64(i * 1024),
					FileCount:   int64(i % 200),
					DirCount:    int64(i % 20),
					SizeBuckets: map[string]int64{
						SizeRange0To1KB:     int64(i % 15),
						SizeRange1KBTo1MB:   int64(i % 25),
						SizeRange1MBTo100MB: int64(i % 8),
						SizeRange100MBPlus:  int64(i % 3),
					},
					TopFiles: []TopFileInfo{
						{Path: fmt.Sprintf("/large/dataset/path/%d/large1.txt", i), Size: int64(i * 200), Rank: 1},
						{Path: fmt.Sprintf("/large/dataset/path/%d/large2.txt", i), Size: int64(i * 150), Rank: 2},
						{Path: fmt.Sprintf("/large/dataset/path/%d/large3.txt", i), Size: int64(i * 100), Rank: 3},
					},
					OthersTotal: int64(i * 25),
					OthersCount: int64(i % 75),
					LastUpdated: time.Now(),
				}
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				err := storage.StoreBatch(stats)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSQLiteStorage_QueryComplexity(b *testing.B) {
	tmpDir := b.TempDir()
	dbPath := filepath.Join(tmpDir, "query.db")

	storage, err := NewSQLiteStorage(dbPath, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer storage.Close()

	err = storage.Initialize()
	if err != nil {
		b.Fatal(err)
	}

	// Populate with test data
	stats := make([]DiskStat, 5000)
	for i := range stats {
		stats[i] = DiskStat{
			Path:        fmt.Sprintf("/query/test/level%d/path%d", i%3, i),
			Level:       i % 5,
			Size:        int64(i * 1024),
			FileCount:   int64(i % 100),
			DirCount:    int64(i % 10),
			SizeBuckets: map[string]int64{SizeRange1KBTo1MB: int64(i % 20)},
			TopFiles:    []TopFileInfo{{Path: fmt.Sprintf("/file%d.txt", i), Size: int64(i * 50), Rank: 1}},
			OthersTotal: int64(i * 5),
			OthersCount: int64(i % 30),
			LastUpdated: time.Now(),
		}
	}

	err = storage.StoreBatch(stats)
	if err != nil {
		b.Fatal(err)
	}

	// Test different query complexities
	queryTests := []struct {
		name  string
		paths map[string]int
	}{
		{
			name:  "SinglePath",
			paths: map[string]int{"/query/test/level0": 2},
		},
		{
			name: "MultiplePaths",
			paths: map[string]int{
				"/query/test/level0": 2,
				"/query/test/level1": 3,
				"/query/test/level2": 1,
			},
		},
		{
			name: "DeepHierarchy",
			paths: map[string]int{
				"/query/test": 5,
			},
		},
	}

	for _, qt := range queryTests {
		b.Run(qt.name, func(b *testing.B) {
			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				_, err := storage.GetMetricsData(qt.paths)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSQLiteStorage_DatabaseOperations(b *testing.B) {
	tmpDir := b.TempDir()
	dbPath := filepath.Join(tmpDir, "ops.db")

	b.Run("Initialize", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			storage, err := NewSQLiteStorage(dbPath, nil)
			if err != nil {
				b.Fatal(err)
			}
			
			err = storage.Initialize()
			if err != nil {
				b.Fatal(err)
			}
			
			storage.Close()
		}
	})

	b.Run("Cleanup", func(b *testing.B) {
		storage, err := NewSQLiteStorage(dbPath, nil)
		if err != nil {
			b.Fatal(err)
		}
		defer storage.Close()

		err = storage.Initialize()
		if err != nil {
			b.Fatal(err)
		}

		// Add some data
		stats := []DiskStat{{
			Path: "/cleanup/test", Level: 0, Size: 1000,
			SizeBuckets: map[string]int64{SizeRange1KBTo1MB: 5},
			TopFiles:    []TopFileInfo{{Path: "/file.txt", Size: 500, Rank: 1}},
			LastUpdated: time.Now(),
		}}
		storage.StoreBatch(stats)

		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			err := storage.Cleanup()
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("DatabaseInfo", func(b *testing.B) {
		storage, err := NewSQLiteStorage(dbPath, nil)
		if err != nil {
			b.Fatal(err)
		}
		defer storage.Close()

		err = storage.Initialize()
		if err != nil {
			b.Fatal(err)
		}

		b.ResetTimer()
		b.ReportAllocs()

		for i := 0; i < b.N; i++ {
			_, err := storage.GetDatabaseInfo()
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}