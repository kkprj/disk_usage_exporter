package exporter

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestLevelCalculation tests the calculateRelativeLevel function
// /mnt                       (level 0)
// ├── nfs-user1-pvc-cb83123  (level 1)
// │   ├── nfs-user1-1  	   (level 2)
// │   │   ├── nfs-user1-11   (level 3)
// │   │   ├── nfs-user1-12   (level 3)
// │   │   └── nfs-user1-13   (level 3)
// │   ├── nfs-user1-2  	   (level 2)
// │   └── nfs-user1-3  	   (level 2)
// ├── nfs-user2-pvc-cb83123  (level 1)
// │   ├── nfs-user2-1  	   (level 2)
// │   └── nfs-user2-2  	   (level 2)
// └── nfs-user3-pvc-cb83123  (level 1)
//     ├── nfs-user3-1        (level 3)
//     ├── nfs-user3-2        (level 3)
//     ├── nfs-user3-3        (level 3)
//     └── nfs-user3-4        (level 3)

func TestLevelCalculation(t *testing.T) {
	// Create a streaming processor for testing
	rootPath := "/tmp/test-level-root"
	processor := &streamingProcessor{
		rootPath: rootPath,
		maxLevel: 3,
		stats:    make(map[string]*aggregatedStats),
	}

	testCases := []struct {
		name     string
		path     string
		expected int
	}{
		{
			name:     "Root path",
			path:     "/tmp/test-level-root",
			expected: 0,
		},
		{
			name:     "Level 1 directory",
			path:     "/tmp/test-level-root/level1-dir",
			expected: 1,
		},
		{
			name:     "Level 2 directory",
			path:     "/tmp/test-level-root/level1-dir/level2-dir",
			expected: 2,
		},
		{
			name:     "Level 3 directory",
			path:     "/tmp/test-level-root/level1-dir/level2-dir/level3-dir",
			expected: 3,
		},
		{
			name:     "Level 4 directory (deep nesting)",
			path:     "/tmp/test-level-root/a/b/c/d",
			expected: 4,
		},
		{
			name:     "Path outside root (safety check)",
			path:     "/tmp/other-root/some-dir",
			expected: 0,
		},
		{
			name:     "Path with same prefix but different root",
			path:     "/tmp/test-level-root-other/dir",
			expected: 0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := processor.calculateRelativeLevel(tc.path)
			if result != tc.expected {
				t.Errorf("calculateRelativeLevel(%s) = %d, expected %d", tc.path, result, tc.expected)
			}
		})
	}
}

// TestLevelConstraintsInMetrics tests that metrics respect directory level limits
func TestLevelConstraintsInMetrics(t *testing.T) {
	// Create test directory structure
	testRoot := "/tmp/test-level-constraints"
	os.RemoveAll(testRoot) // Clean up if exists
	defer os.RemoveAll(testRoot)

	// Create nested directory structure
	deepPath := filepath.Join(testRoot, "level1", "level2", "level3", "level4", "level5")
	err := os.MkdirAll(deepPath, 0755)
	if err != nil {
		t.Fatalf("Failed to create test directories: %v", err)
	}

	// Create test files at different levels
	testFiles := []string{
		filepath.Join(testRoot, "level1", "file1.txt"),
		filepath.Join(testRoot, "level1", "level2", "file2.txt"),
		filepath.Join(testRoot, "level1", "level2", "level3", "file3.txt"),
		filepath.Join(testRoot, "level1", "level2", "level3", "level4", "file4.txt"),
	}
	
	for _, testFile := range testFiles {
		err = os.WriteFile(testFile, []byte("test content"), 0644)
		if err != nil {
			t.Fatalf("Failed to create test file %s: %v", testFile, err)
		}
	}

	testCases := []struct {
		dirLevel           int
		expectedMaxLevel   int
		expectedMinMetrics int
	}{
		{dirLevel: 1, expectedMaxLevel: 1, expectedMinMetrics: 1},
		{dirLevel: 2, expectedMaxLevel: 2, expectedMinMetrics: 2},
		{dirLevel: 3, expectedMaxLevel: 3, expectedMinMetrics: 3},
		{dirLevel: 4, expectedMaxLevel: 4, expectedMinMetrics: 4},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("DirLevel_%d", tc.dirLevel), func(t *testing.T) {
			// Create new registry for each test
			registry := prometheus.NewRegistry()

			// Create exporter with specific dir level
			pathMap := map[string]int{testRoot: tc.dirLevel}
			exporter := NewExporter(pathMap, false)
			exporter.SetCollectionFlags(false, false, true) // Enable size buckets

			// Register metrics
			registry.MustRegister(diskUsage, diskUsageSizeBucket)

			// Perform scan
			exporter.performScan()

			// Gather metrics
			metricFamilies, err := registry.Gather()
			if err != nil {
				t.Fatalf("Failed to gather metrics: %v", err)
			}

			// Check metrics
			var maxLevelFound = -1
			var metricCount = 0

			for _, mf := range metricFamilies {
				if *mf.Name == "node_disk_usage" {
					metricCount = len(mf.Metric)
					for _, metric := range mf.Metric {
						for _, label := range metric.Label {
							if *label.Name == "level" {
								var level int
								fmt.Sscanf(*label.Value, "%d", &level)
								if level > maxLevelFound {
									maxLevelFound = level
								}
								
								// Verify level doesn't exceed dirLevel
								if level > tc.dirLevel {
									t.Errorf("Found metric with level %d > dirLevel %d", level, tc.dirLevel)
								}
							}
						}
					}
				}
			}

			// Verify we found expected metrics
			if metricCount < tc.expectedMinMetrics {
				t.Errorf("Expected at least %d metrics, got %d", tc.expectedMinMetrics, metricCount)
			}

			if maxLevelFound > tc.expectedMaxLevel {
				t.Errorf("Max level found %d > expected max %d", maxLevelFound, tc.expectedMaxLevel)
			}

			if maxLevelFound < 1 && tc.dirLevel > 0 {
				t.Error("No metrics with valid levels found")
			}

			t.Logf("DirLevel %d: Found %d metrics, max level: %d", tc.dirLevel, metricCount, maxLevelFound)
		})
	}
}

// TestLevelAssignmentAccuracy tests accurate level assignment for complex structures
func TestLevelAssignmentAccuracy(t *testing.T) {
	// Create test structure matching the example
	testRoot := "/tmp/test-level-accuracy" 
	os.RemoveAll(testRoot)
	defer os.RemoveAll(testRoot)

	// Create the exact structure from the user's example
	structure := []string{
		"nfs-user1-pvc-cb83123/nfs-user1-1/nfs-user1-11",
		"nfs-user1-pvc-cb83123/nfs-user1-1/nfs-user1-12", 
		"nfs-user1-pvc-cb83123/nfs-user1-1/nfs-user1-13",
		"nfs-user1-pvc-cb83123/nfs-user1-2",
		"nfs-user1-pvc-cb83123/nfs-user1-3",
		"nfs-user2-pvc-cb83123/nfs-user2-1",
		"nfs-user2-pvc-cb83123/nfs-user2-2",
		"nfs-user3-pvc-cb83123/nfs-user3-1",
		"nfs-user3-pvc-cb83123/nfs-user3-2",
		"nfs-user3-pvc-cb83123/nfs-user3-3",
		"nfs-user3-pvc-cb83123/nfs-user3-4",
	}

	// Create directories
	for _, dir := range structure {
		fullPath := filepath.Join(testRoot, dir)
		err := os.MkdirAll(fullPath, 0755)
		if err != nil {
			t.Fatalf("Failed to create directory %s: %v", fullPath, err)
		}
	}

	// Add some test files
	testFiles := []string{
		"nfs-user1-pvc-cb83123/nfs-user1-1/nfs-user1-11/file1.txt",
		"nfs-user2-pvc-cb83123/nfs-user2-1/file2.txt", 
		"nfs-user3-pvc-cb83123/nfs-user3-1/file3.txt",
	}

	for _, file := range testFiles {
		fullPath := filepath.Join(testRoot, file)
		err := os.WriteFile(fullPath, []byte("test content"), 0644)
		if err != nil {
			t.Fatalf("Failed to create file %s: %v", fullPath, err)
		}
	}

	// Test with dir-level 3
	registry := prometheus.NewRegistry()
	pathMap := map[string]int{testRoot: 3}
	exporter := NewExporter(pathMap, false)
	exporter.SetCollectionFlags(false, false, true)

	registry.MustRegister(diskUsage, diskUsageSizeBucket)
	exporter.performScan()

	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	// Expected level assignments
	expectedLevels := map[string]int{
		// Level 1: nfs-userX-pvc-cb83123 directories
		fmt.Sprintf("%s/nfs-user1-pvc-cb83123", testRoot): 1,
		fmt.Sprintf("%s/nfs-user2-pvc-cb83123", testRoot): 1, 
		fmt.Sprintf("%s/nfs-user3-pvc-cb83123", testRoot): 1,
		
		// Level 2: subdirectories  
		fmt.Sprintf("%s/nfs-user1-pvc-cb83123/nfs-user1-1", testRoot): 2,
		fmt.Sprintf("%s/nfs-user1-pvc-cb83123/nfs-user1-2", testRoot): 2,
		fmt.Sprintf("%s/nfs-user1-pvc-cb83123/nfs-user1-3", testRoot): 2,
		fmt.Sprintf("%s/nfs-user2-pvc-cb83123/nfs-user2-1", testRoot): 2,
		fmt.Sprintf("%s/nfs-user2-pvc-cb83123/nfs-user2-2", testRoot): 2,
		fmt.Sprintf("%s/nfs-user3-pvc-cb83123/nfs-user3-1", testRoot): 2,
		fmt.Sprintf("%s/nfs-user3-pvc-cb83123/nfs-user3-2", testRoot): 2,
		fmt.Sprintf("%s/nfs-user3-pvc-cb83123/nfs-user3-3", testRoot): 2,
		fmt.Sprintf("%s/nfs-user3-pvc-cb83123/nfs-user3-4", testRoot): 2,
		
		// Level 3: deep subdirectories
		fmt.Sprintf("%s/nfs-user1-pvc-cb83123/nfs-user1-1/nfs-user1-11", testRoot): 3,
		fmt.Sprintf("%s/nfs-user1-pvc-cb83123/nfs-user1-1/nfs-user1-12", testRoot): 3,
		fmt.Sprintf("%s/nfs-user1-pvc-cb83123/nfs-user1-1/nfs-user1-13", testRoot): 3,
	}

	// Verify level assignments
	foundPaths := make(map[string]int)
	
	for _, mf := range metricFamilies {
		if *mf.Name == "node_disk_usage" {
			for _, metric := range mf.Metric {
				var path, levelStr string
				for _, label := range metric.Label {
					if *label.Name == "path" {
						path = *label.Value
					}
					if *label.Name == "level" {
						levelStr = *label.Value
					}
				}
				
				if path != "" && levelStr != "" {
					var level int
					fmt.Sscanf(levelStr, "%d", &level)
					foundPaths[path] = level
				}
			}
		}
	}

	// Check expected levels
	for expectedPath, expectedLevel := range expectedLevels {
		if foundLevel, exists := foundPaths[expectedPath]; exists {
			if foundLevel != expectedLevel {
				t.Errorf("Path %s: expected level %d, got level %d", expectedPath, expectedLevel, foundLevel)
			}
		} else {
			t.Errorf("Expected path %s with level %d not found in metrics", expectedPath, expectedLevel)
		}
	}

	t.Logf("Verified %d paths with correct level assignments", len(expectedLevels))
}

// TestLevelCalculationEdgeCases tests edge cases for level calculation
func TestLevelCalculationEdgeCases(t *testing.T) {
	testCases := []struct {
		name     string
		rootPath string
		testPath string
		expected int
	}{
		{
			name:     "Root with trailing slash",
			rootPath: "/tmp/root/",
			testPath: "/tmp/root/subdir",
			expected: 1,
		},
		{
			name:     "Path with trailing slash",
			rootPath: "/tmp/root",
			testPath: "/tmp/root/subdir/",
			expected: 1,
		},
		{
			name:     "Both with trailing slashes",
			rootPath: "/tmp/root/",
			testPath: "/tmp/root/subdir/",
			expected: 1,
		},
		{
			name:     "Multiple separators",
			rootPath: "/tmp/root",
			testPath: "/tmp/root//subdir///deep",
			expected: 2,
		},
		{
			name:     "Dot components",
			rootPath: "/tmp/root",
			testPath: "/tmp/root/./subdir/../subdir2",
			expected: 1,
		},
		{
			name:     "Same path (different representations)",
			rootPath: "/tmp/root/",
			testPath: "/tmp/root/.",
			expected: 0,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			processor := &streamingProcessor{
				rootPath: tc.rootPath,
				maxLevel: 5,
				stats:    make(map[string]*aggregatedStats),
			}
			
			result := processor.calculateRelativeLevel(tc.testPath)
			if result != tc.expected {
				t.Errorf("calculateRelativeLevel(%s) with root %s = %d, expected %d", 
					tc.testPath, tc.rootPath, result, tc.expected)
			}
		})
	}
}

// TestLevelConstraintsWithSizeBuckets tests that size bucket metrics also respect level constraints
func TestLevelConstraintsWithSizeBuckets(t *testing.T) {
	// Create test structure
	testRoot := "/tmp/test-size-bucket-levels"
	os.RemoveAll(testRoot)
	defer os.RemoveAll(testRoot)

	// Create nested structure with files at different levels
	paths := []string{
		"level1/file1.txt",
		"level1/level2/file2.txt", 
		"level1/level2/level3/file3.txt",
		"level1/level2/level3/level4/file4.txt",
	}

	for _, path := range paths {
		fullPath := filepath.Join(testRoot, path)
		err := os.MkdirAll(filepath.Dir(fullPath), 0755)
		if err != nil {
			t.Fatalf("Failed to create directory: %v", err)
		}

		err = os.WriteFile(fullPath, []byte("test content for size bucket"), 0644)
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}
	}

	testCases := []struct {
		dirLevel         int
		expectedMaxLevel int
	}{
		{dirLevel: 1, expectedMaxLevel: 1},
		{dirLevel: 2, expectedMaxLevel: 2},
		{dirLevel: 3, expectedMaxLevel: 3},
		{dirLevel: 4, expectedMaxLevel: 4},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("SizeBucket_DirLevel_%d", tc.dirLevel), func(t *testing.T) {
			registry := prometheus.NewRegistry()
			pathMap := map[string]int{testRoot: tc.dirLevel}
			exporter := NewExporter(pathMap, false)
			exporter.SetCollectionFlags(false, false, true)

			registry.MustRegister(diskUsage, diskUsageSizeBucket)
			exporter.performScan()

			metricFamilies, err := registry.Gather()
			if err != nil {
				t.Fatalf("Failed to gather metrics: %v", err)
			}

			var maxLevelFound = -1
			var sizeBucketCount = 0

			for _, mf := range metricFamilies {
				if *mf.Name == "node_disk_usage_size_bucket" {
					sizeBucketCount = len(mf.Metric)
					for _, metric := range mf.Metric {
						for _, label := range metric.Label {
							if *label.Name == "level" {
								var level int
								fmt.Sscanf(*label.Value, "%d", &level)
								if level > maxLevelFound {
									maxLevelFound = level
								}

								if level > tc.dirLevel {
									t.Errorf("Size bucket metric with level %d > dirLevel %d", level, tc.dirLevel)
								}
							}
						}
					}
				}
			}

			if maxLevelFound > tc.expectedMaxLevel {
				t.Errorf("Size bucket max level %d > expected max %d", maxLevelFound, tc.expectedMaxLevel)
			}

			t.Logf("DirLevel %d: Found %d size bucket metrics, max level: %d", 
				tc.dirLevel, sizeBucketCount, maxLevelFound)
		})
	}
}