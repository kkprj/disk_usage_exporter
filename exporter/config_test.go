package exporter

import (
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// TestConfigVariations tests different configuration settings
func TestConfigVariations(t *testing.T) {
	// Clean up any existing metrics
	prometheus.DefaultRegisterer = prometheus.NewRegistry()
	
	tests := []struct {
		name      string
		dirLevel  int
		chunkSize int
		maxProcs  int
		testPath  string
	}{
		{
			name:      "DirLevel1_Chunk500_Procs1",
			dirLevel:  1,
			chunkSize: 500,
			maxProcs:  1,
			testPath:  "/tmp/test-complex",
		},
		{
			name:      "DirLevel2_Chunk500_Procs6",
			dirLevel:  2,
			chunkSize: 500,
			maxProcs:  6,
			testPath:  "/tmp/test-complex",
		},
		{
			name:      "DirLevel3_Chunk500_Procs12",
			dirLevel:  3,
			chunkSize: 500,
			maxProcs:  12,
			testPath:  "/tmp/test-complex",
		},
		{
			name:      "DirLevel2_Chunk1000_Procs6",
			dirLevel:  2,
			chunkSize: 1000,
			maxProcs:  6,
			testPath:  "/tmp/test-complex",
		},
		{
			name:      "DirLevel3_Chunk1000_Procs12",
			dirLevel:  3,
			chunkSize: 1000,
			maxProcs:  12,
			testPath:  "/tmp/test-complex",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create new registry for each test
			registry := prometheus.NewRegistry()
			
			// Create exporter with test configuration
			pathMap := map[string]int{tt.testPath: tt.dirLevel}
			exporter := NewExporter(pathMap, false)
			exporter.SetMaxProcs(tt.maxProcs)
			exporter.SetChunkSize(tt.chunkSize)
			exporter.SetCollectionFlags(false, false, true) // Only size buckets enabled
			
			// Register metrics with test registry
			registry.MustRegister(diskUsage, diskUsageSizeBucket)
			
			// Perform scan
			exporter.performScan()
			
			// Verify metrics exist
			metricFamilies, err := registry.Gather()
			if err != nil {
				t.Fatalf("Failed to gather metrics: %v", err)
			}
			
			// Check for node_disk_usage metrics
			var diskUsageFound, sizeBucketFound bool
			var diskUsageCount, sizeBucketCount int
			
			for _, mf := range metricFamilies {
				switch *mf.Name {
				case "node_disk_usage":
					diskUsageFound = true
					diskUsageCount = len(mf.Metric)
					t.Logf("Found %d node_disk_usage metrics", diskUsageCount)
					
					// Verify metrics have correct level constraints
					for _, metric := range mf.Metric {
						var level int
						for _, label := range metric.Label {
							if *label.Name == "level" {
								fmt.Sscanf(*label.Value, "%d", &level)
								if level > tt.dirLevel {
									t.Errorf("Found metric with level %d > dirLevel %d", level, tt.dirLevel)
								}
							}
						}
					}
					
				case "node_disk_usage_size_bucket":
					sizeBucketFound = true
					sizeBucketCount = len(mf.Metric)
					t.Logf("Found %d node_disk_usage_size_bucket metrics", sizeBucketCount)
				}
			}
			
			if !diskUsageFound {
				t.Error("node_disk_usage metrics not found")
			}
			
			if !sizeBucketFound {
				t.Error("node_disk_usage_size_bucket metrics not found")
			}
			
			// Verify that different dir levels produce different metric counts
			expectedMinMetrics := tt.dirLevel // At least one metric per level
			if diskUsageCount < expectedMinMetrics {
				t.Errorf("Expected at least %d disk usage metrics, got %d", expectedMinMetrics, diskUsageCount)
			}
			
			t.Logf("Test %s completed successfully - DirLevel:%d, ChunkSize:%d, MaxProcs:%d, DiskUsage:%d, SizeBucket:%d",
				tt.name, tt.dirLevel, tt.chunkSize, tt.maxProcs, diskUsageCount, sizeBucketCount)
		})
	}
}

// TestDirLevelConstraints specifically tests directory level constraints
func TestDirLevelConstraints(t *testing.T) {
	testCases := []struct {
		dirLevel         int
		expectedMaxLevel int
	}{
		{dirLevel: 1, expectedMaxLevel: 1},
		{dirLevel: 2, expectedMaxLevel: 2},
		{dirLevel: 3, expectedMaxLevel: 3},
	}
	
	for _, tc := range testCases {
		t.Run(fmt.Sprintf("DirLevel_%d", tc.dirLevel), func(t *testing.T) {
			registry := prometheus.NewRegistry()
			
			pathMap := map[string]int{"/tmp/test-complex": tc.dirLevel}
			exporter := NewExporter(pathMap, false)
			exporter.SetMaxProcs(6)
			exporter.SetChunkSize(1000)
			exporter.SetCollectionFlags(false, false, true)
			
			registry.MustRegister(diskUsage, diskUsageSizeBucket)
			
			exporter.performScan()
			
			metricFamilies, err := registry.Gather()
			if err != nil {
				t.Fatalf("Failed to gather metrics: %v", err)
			}
			
			maxLevelFound := -1
			for _, mf := range metricFamilies {
				if *mf.Name == "node_disk_usage" {
					for _, metric := range mf.Metric {
						for _, label := range metric.Label {
							if *label.Name == "level" {
								var level int
								fmt.Sscanf(*label.Value, "%d", &level)
								if level > maxLevelFound {
									maxLevelFound = level
								}
							}
						}
					}
				}
			}
			
			if maxLevelFound > tc.expectedMaxLevel {
				t.Errorf("Found level %d, but dirLevel is %d", maxLevelFound, tc.dirLevel)
			}
			
			if maxLevelFound < 0 {
				t.Error("No metrics with level found")
			}
			
			t.Logf("DirLevel %d constraint verified - max level found: %d", tc.dirLevel, maxLevelFound)
		})
	}
}

// TestChunkSizePerformance tests different chunk sizes for performance
func TestChunkSizePerformance(t *testing.T) {
	chunkSizes := []int{500, 1000}
	
	for _, chunkSize := range chunkSizes {
		t.Run(fmt.Sprintf("ChunkSize_%d", chunkSize), func(t *testing.T) {
			registry := prometheus.NewRegistry()
			
			pathMap := map[string]int{"/tmp/test-complex": 3}
			exporter := NewExporter(pathMap, false)
			exporter.SetMaxProcs(6)
			exporter.SetChunkSize(chunkSize)
			exporter.SetCollectionFlags(false, false, true)
			
			registry.MustRegister(diskUsage, diskUsageSizeBucket)
			
			start := time.Now()
			exporter.performScan()
			elapsed := time.Since(start)
			
			metricFamilies, err := registry.Gather()
			if err != nil {
				t.Fatalf("Failed to gather metrics: %v", err)
			}
			
			var metricCount int
			for _, mf := range metricFamilies {
				if *mf.Name == "node_disk_usage" || *mf.Name == "node_disk_usage_size_bucket" {
					metricCount += len(mf.Metric)
				}
			}
			
			t.Logf("ChunkSize %d: %d metrics generated in %v", chunkSize, metricCount, elapsed)
			
			if metricCount == 0 {
				t.Error("No metrics generated")
			}
		})
	}
}

// TestMaxProcsScaling tests different max-procs settings
func TestMaxProcsScaling(t *testing.T) {
	maxProcsList := []int{1, 6, 12}
	
	for _, maxProcs := range maxProcsList {
		t.Run(fmt.Sprintf("MaxProcs_%d", maxProcs), func(t *testing.T) {
			registry := prometheus.NewRegistry()
			
			pathMap := map[string]int{"/tmp/test-complex": 3}
			exporter := NewExporter(pathMap, false)
			exporter.SetMaxProcs(maxProcs)
			exporter.SetChunkSize(1000)
			exporter.SetCollectionFlags(false, false, true)
			
			registry.MustRegister(diskUsage, diskUsageSizeBucket)
			
			start := time.Now()
			exporter.performScan()
			elapsed := time.Since(start)
			
			metricFamilies, err := registry.Gather()
			if err != nil {
				t.Fatalf("Failed to gather metrics: %v", err)
			}
			
			var metricCount int
			for _, mf := range metricFamilies {
				if *mf.Name == "node_disk_usage" || *mf.Name == "node_disk_usage_size_bucket" {
					metricCount += len(mf.Metric)
				}
			}
			
			t.Logf("MaxProcs %d: %d metrics generated in %v", maxProcs, metricCount, elapsed)
			
			if metricCount == 0 {
				t.Error("No metrics generated")  
			}
		})
	}
}

