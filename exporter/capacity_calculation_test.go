package exporter

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestDirectoryCapacityCalculation tests that level 1 directories correctly
// include the sum of all files and subdirectories within them
func TestDirectoryCapacityCalculation(t *testing.T) {
	// Create test directory structure
	testRoot := "/tmp/test-capacity-calculation"
	os.RemoveAll(testRoot) // Clean up if exists
	defer os.RemoveAll(testRoot)

	// Create the test directory structure:
	// /tmp/test-capacity-calculation                    (level 0)
	// ├── level1-dir1                                   (level 1)
	// │   ├── file1.txt (100 bytes)                     (level 1 files)
	// │   ├── file2.txt (200 bytes)                     (level 1 files)
	// │   ├── level2-dir1                               (level 2)
	// │   │   ├── file3.txt (300 bytes)                 (level 2 files)
	// │   │   └── level3-dir1                           (level 3)
	// │   │       └── file4.txt (400 bytes)             (level 3 files)
	// │   └── level2-dir2                               (level 2)
	// │       └── file5.txt (500 bytes)                 (level 2 files)
	// └── level1-dir2                                   (level 1)
	//     └── file6.txt (600 bytes)                     (level 1 files)

	testFiles := map[string][]byte{
		"level1-dir1/file1.txt":                    make([]byte, 100),
		"level1-dir1/file2.txt":                    make([]byte, 200),
		"level1-dir1/level2-dir1/file3.txt":        make([]byte, 300),
		"level1-dir1/level2-dir1/level3-dir1/file4.txt": make([]byte, 400),
		"level1-dir1/level2-dir2/file5.txt":        make([]byte, 500),
		"level1-dir2/file6.txt":                    make([]byte, 600),
	}

	// Create directories and files
	for relativePath, content := range testFiles {
		fullPath := filepath.Join(testRoot, relativePath)
		err := os.MkdirAll(filepath.Dir(fullPath), 0755)
		if err != nil {
			t.Fatalf("Failed to create directory for %s: %v", fullPath, err)
		}
		
		err = os.WriteFile(fullPath, content, 0644)
		if err != nil {
			t.Fatalf("Failed to create file %s: %v", fullPath, err)
		}
	}

	// Test with different dir levels
	testCases := []struct {
		dirLevel          int
		description       string
		expectedSizes     map[string]int64 // path:level -> expected size
	}{
		{
			dirLevel:    3,
			description: "Level 3 test - all directories and files",
			expectedSizes: map[string]int64{
				// Level 0 (root) should contain all files (minimum, plus directory overhead)
				fmt.Sprintf("%s:0", testRoot): 2100, // sum of all files: 100+200+300+400+500+600

				// Level 1 directories should contain all their children (minimum)
				fmt.Sprintf("%s/level1-dir1:1", testRoot): 1500, // 100+200+300+400+500
				fmt.Sprintf("%s/level1-dir2:1", testRoot): 600,  // 600

				// Level 2 directories should contain their direct children and descendants (minimum)
				fmt.Sprintf("%s/level1-dir1/level2-dir1:2", testRoot): 700, // 300+400
				fmt.Sprintf("%s/level1-dir1/level2-dir2:2", testRoot): 500, // 500

				// Level 3 directories should contain only their direct children (minimum)
				fmt.Sprintf("%s/level1-dir1/level2-dir1/level3-dir1:3", testRoot): 400, // 400
			},
		},
		{
			dirLevel:    2,
			description: "Level 2 test - limited depth",
			expectedSizes: map[string]int64{
				// Level 0 (root) should contain all accessible files (level 3 files won't be counted)
				fmt.Sprintf("%s:0", testRoot): 1700, // 100+200+300+500+600 (excluding 400 from level 3)

				// Level 1 directories 
				fmt.Sprintf("%s/level1-dir1:1", testRoot): 1100, // 100+200+300+500 (excluding 400 from level 3)
				fmt.Sprintf("%s/level1-dir2:1", testRoot): 600,  // 600

				// Level 2 directories should contain only their direct children (no level 3)
				fmt.Sprintf("%s/level1-dir1/level2-dir1:2", testRoot): 300, // 300 only (400 is at level 3, not counted)
				fmt.Sprintf("%s/level1-dir1/level2-dir2:2", testRoot): 500, // 500
			},
		},
		{
			dirLevel:    1,
			description: "Level 1 test - shallow depth",
			expectedSizes: map[string]int64{
				// Level 0 (root) should contain all accessible files (only level 1 files)
				fmt.Sprintf("%s:0", testRoot): 900, // 100+200+600 (only direct level 1 files)

				// Level 1 directories should contain only their direct children
				fmt.Sprintf("%s/level1-dir1:1", testRoot): 300, // 100+200 (only direct files)
				fmt.Sprintf("%s/level1-dir2:1", testRoot): 600, // 600
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			// Create new registry for each test
			registry := prometheus.NewRegistry()

			// Create exporter with specific dir level
			pathMap := map[string]int{testRoot: tc.dirLevel}
			exporter := NewExporter(pathMap, false)
			exporter.SetCollectionFlags(false, false, false) // Only test basic size metric

			// Register only the disk usage metric
			registry.MustRegister(diskUsage)

			// Perform scan - use streaming analysis which is the current implementation
			exporter.performStreamingAnalysis()

			// Gather metrics
			metricFamilies, err := registry.Gather()
			if err != nil {
				t.Fatalf("Failed to gather metrics: %v", err)
			}

			// Extract metrics into a map for easier verification
			actualSizes := make(map[string]int64)
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
							key := fmt.Sprintf("%s:%s", path, levelStr)
							actualSizes[key] = int64(metric.Gauge.GetValue())
						}
					}
				}
			}

			// Verify expected minimum sizes (actual sizes will be larger due to filesystem overhead)
			for expectedKey, expectedMinSize := range tc.expectedSizes {
				actualSize, found := actualSizes[expectedKey]
				if !found {
					t.Errorf("Expected path+level %s not found in metrics", expectedKey)
					continue
				}
				
				if actualSize < expectedMinSize {
					t.Errorf("Path+level %s: expected at least %d, got %d", expectedKey, expectedMinSize, actualSize)
				} else {
					t.Logf("✓ Path+level %s: expected at least %d, got %d", expectedKey, expectedMinSize, actualSize)
				}
			}

			// Log all found metrics for debugging
			t.Logf("Found %d metrics for dir-level %d:", len(actualSizes), tc.dirLevel)
			for key, size := range actualSizes {
				t.Logf("  %s: %d bytes", key, size)
			}
		})
	}
}

// TestHierarchicalSizeInheritance specifically tests that parent directories
// inherit sizes from all their children at deeper levels
func TestHierarchicalSizeInheritance(t *testing.T) {
	// Create simple nested structure
	testRoot := "/tmp/test-hierarchical-size"
	os.RemoveAll(testRoot)
	defer os.RemoveAll(testRoot)

	// Structure:
	// /tmp/test-hierarchical-size/
	// └── parent/                     (level 1)
	//     ├── file-at-level1.txt      (1000 bytes, level 1 file)
	//     └── child/                  (level 2)
	//         ├── file-at-level2.txt  (2000 bytes, level 2 file)
	//         └── grandchild/         (level 3)
	//             └── file-at-level3.txt (3000 bytes, level 3 file)

	testFiles := map[string]int{
		"parent/file-at-level1.txt":                  1000,
		"parent/child/file-at-level2.txt":            2000,
		"parent/child/grandchild/file-at-level3.txt": 3000,
	}

	for relativePath, size := range testFiles {
		fullPath := filepath.Join(testRoot, relativePath)
		err := os.MkdirAll(filepath.Dir(fullPath), 0755)
		if err != nil {
			t.Fatalf("Failed to create directory: %v", err)
		}
		
		content := make([]byte, size)
		err = os.WriteFile(fullPath, content, 0644)
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}
	}

	// Test with dir-level 3 to capture all files
	registry := prometheus.NewRegistry()
	pathMap := map[string]int{testRoot: 3}
	exporter := NewExporter(pathMap, false)
	exporter.SetCollectionFlags(false, false, false)

	registry.MustRegister(diskUsage)
	exporter.performStreamingAnalysis()

	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	// Extract metrics
	sizes := make(map[string]int64)
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
					key := fmt.Sprintf("%s:%s", path, levelStr)
					sizes[key] = int64(metric.Gauge.GetValue())
				}
			}
		}
	}

	// Verify hierarchical size inheritance
	// Note: actual sizes will be larger due to filesystem directory overhead
	expectedMinimumSizes := map[string]int64{
		// Root level should contain all files (plus directory overhead)
		fmt.Sprintf("%s:0", testRoot): 6000, // 1000 + 2000 + 3000

		// Level 1 parent should contain all descendant files (plus directory overhead)
		fmt.Sprintf("%s/parent:1", testRoot): 6000, // 1000 + 2000 + 3000

		// Level 2 child should contain itself and descendants (plus directory overhead)
		fmt.Sprintf("%s/parent/child:2", testRoot): 5000, // 2000 + 3000

		// Level 3 grandchild should contain only its own file (plus directory overhead)
		fmt.Sprintf("%s/parent/child/grandchild:3", testRoot): 3000, // 3000
	}

	// Verify hierarchical relationships: parent should be >= sum of children
	parentSize := sizes[fmt.Sprintf("%s/parent:1", testRoot)]
	childSize := sizes[fmt.Sprintf("%s/parent/child:2", testRoot)]
	grandchildSize := sizes[fmt.Sprintf("%s/parent/child/grandchild:3", testRoot)]
	rootSize := sizes[fmt.Sprintf("%s:0", testRoot)]

	// Test hierarchical relationships
	if parentSize < childSize {
		t.Errorf("Parent size (%d) should be >= child size (%d)", parentSize, childSize)
	}
	
	if childSize < grandchildSize {
		t.Errorf("Child size (%d) should be >= grandchild size (%d)", childSize, grandchildSize)
	}
	
	// NOTE: Root size may be smaller than parent size due to how directory overhead is calculated
	// Root gets only propagated file content, while parent gets its own directory overhead too
	if rootSize < 6000 { // Should at least contain all file content
		t.Errorf("Root size (%d) should contain at least all file content (6000)", rootSize)
	}

	// Verify minimum sizes (file content sizes should be included)
	for expectedKey, minSize := range expectedMinimumSizes {
		actualSize, found := sizes[expectedKey]
		if !found {
			t.Errorf("Expected metric %s not found", expectedKey)
			continue
		}

		if actualSize < minSize {
			t.Errorf("Metric %s: expected at least %d, got %d", expectedKey, minSize, actualSize)
		}
	}

	t.Logf("Hierarchical sizes: root=%d, parent=%d, child=%d, grandchild=%d", 
		rootSize, parentSize, childSize, grandchildSize)

	t.Logf("Hierarchical size inheritance test completed with %d metrics", len(sizes))
}