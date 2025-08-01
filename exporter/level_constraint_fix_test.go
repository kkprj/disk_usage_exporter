package exporter

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestLevelConstraintWithDeepFiles tests the exact scenario described:
// /mnt (level 0)
// ├── nfs-user1 (level 1) - should show 40MiB total
// │   ├── 10MiB-file.bin (level 1 file)
// │   ├── nfs-user1-1 (level 2)
// │   │   ├── nfs-user1-11 (level 3) - beyond maxLevel=2, but files should still be counted
// │   │   │   └── 10MiB-file.bin
// │   │   ├── nfs-user1-12 (level 3)
// │   │   │   └── 10MiB-file.bin
// │   │   └── nfs-user1-13 (level 3)
// │   │       └── 10MiB-file.bin
func TestLevelConstraintWithDeepFiles(t *testing.T) {
	// Create test directory structure
	testRoot := "/tmp/test-level-constraint-fix"
	os.RemoveAll(testRoot) // Clean up if exists
	defer os.RemoveAll(testRoot)

	// Create the exact structure as described
	err := os.MkdirAll(filepath.Join(testRoot, "nfs-user1", "nfs-user1-1", "nfs-user1-11"), 0755)
	if err != nil {
		t.Fatalf("Failed to create directory structure: %v", err)
	}
	err = os.MkdirAll(filepath.Join(testRoot, "nfs-user1", "nfs-user1-1", "nfs-user1-12"), 0755)
	if err != nil {
		t.Fatalf("Failed to create directory structure: %v", err)
	}
	err = os.MkdirAll(filepath.Join(testRoot, "nfs-user1", "nfs-user1-1", "nfs-user1-13"), 0755)
	if err != nil {
		t.Fatalf("Failed to create directory structure: %v", err)
	}

	// Create 10MiB files (10 * 1024 * 1024 = 10485760 bytes)
	fileSize := 10 * 1024 * 1024
	fileContent := make([]byte, fileSize)

	testFiles := []string{
		"nfs-user1/10MiB-file.bin",                           // Level 1 file: 10MiB
		"nfs-user1/nfs-user1-1/nfs-user1-11/10MiB-file.bin", // Level 3 file: 10MiB
		"nfs-user1/nfs-user1-1/nfs-user1-12/10MiB-file.bin", // Level 3 file: 10MiB
		"nfs-user1/nfs-user1-1/nfs-user1-13/10MiB-file.bin", // Level 3 file: 10MiB
	}

	for _, testFile := range testFiles {
		fullPath := filepath.Join(testRoot, testFile)
		err = os.WriteFile(fullPath, fileContent, 0644)
		if err != nil {
			t.Fatalf("Failed to create file %s: %v", fullPath, err)
		}
	}

	// Test with level=2 (as described in the issue)
	registry := prometheus.NewRegistry()
	pathMap := map[string]int{testRoot: 2} // maxLevel = 2
	exporter := NewExporter(pathMap, false)
	exporter.SetCollectionFlags(false, false, false) // Only test basic size metric

	// Register only the disk usage metric
	registry.MustRegister(diskUsage)

	// Perform scan using streaming analysis
	exporter.performLiveAnalysis()

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

	// Expected sizes (accounting for filesystem overhead)
	expectedMinSizes := map[string]int64{
		// Root level should contain all 4 files: 40MiB
		fmt.Sprintf("%s:0", testRoot): int64(4 * fileSize),

		// Level 1 directory (nfs-user1) should contain all 4 files: 40MiB
		fmt.Sprintf("%s/nfs-user1:1", testRoot): int64(4 * fileSize),

		// Level 2 directory (nfs-user1-1) should contain 3 files from level 3: 30MiB
		fmt.Sprintf("%s/nfs-user1/nfs-user1-1:2", testRoot): int64(3 * fileSize),
	}

	// Verify that we should NOT have level 3 directory metrics (beyond maxLevel)
	level3Directories := []string{
		fmt.Sprintf("%s/nfs-user1/nfs-user1-1/nfs-user1-11:3", testRoot),
		fmt.Sprintf("%s/nfs-user1/nfs-user1-1/nfs-user1-12:3", testRoot),
		fmt.Sprintf("%s/nfs-user1/nfs-user1-1/nfs-user1-13:3", testRoot),
	}

	for _, level3Dir := range level3Directories {
		if _, found := actualSizes[level3Dir]; found {
			t.Errorf("Should not have metrics for level 3 directory (beyond maxLevel=2): %s", level3Dir)
		}
	}

	// Verify expected minimum sizes
	for expectedKey, expectedMinSize := range expectedMinSizes {
		actualSize, found := actualSizes[expectedKey]
		if !found {
			t.Errorf("Expected metric %s not found", expectedKey)
			continue
		}

		if actualSize < expectedMinSize {
			t.Errorf("Metric %s: expected at least %d bytes (%.1f MiB), got %d bytes (%.1f MiB)", 
				expectedKey, expectedMinSize, float64(expectedMinSize)/(1024*1024), 
				actualSize, float64(actualSize)/(1024*1024))
		} else {
			t.Logf("✓ Metric %s: expected at least %.1f MiB, got %.1f MiB", 
				expectedKey, float64(expectedMinSize)/(1024*1024), float64(actualSize)/(1024*1024))
		}
	}

	// Key test: nfs-user1 should be approximately 40MiB (4 * 10MiB)
	nfsUser1Key := fmt.Sprintf("%s/nfs-user1:1", testRoot)
	nfsUser1Size := actualSizes[nfsUser1Key]
	expectedNfsUser1Size := int64(4 * fileSize) // 40MiB

	if nfsUser1Size < expectedNfsUser1Size {
		t.Errorf("CRITICAL: nfs-user1 should include all 4 files (40MiB), but got %.1f MiB - level 3 files are missing!", 
			float64(nfsUser1Size)/(1024*1024))
	} else {
		t.Logf("✓ PASS: nfs-user1 correctly includes all files: %.1f MiB", float64(nfsUser1Size)/(1024*1024))
	}

	// Log all found metrics for debugging
	t.Logf("Found %d metrics:", len(actualSizes))
	for key, size := range actualSizes {
		t.Logf("  %s: %.1f MiB", key, float64(size)/(1024*1024))
	}
}

// TestLevelConstraintComparisonBefore tests the problem with a simpler structure
func TestLevelConstraintComparisonBefore(t *testing.T) {
	// Create test directory structure  
	testRoot := "/tmp/test-level-constraint-comparison"
	os.RemoveAll(testRoot)
	defer os.RemoveAll(testRoot)

	// Simple structure:
	// /root
	// ├── level1-dir
	// │   ├── level1-file.bin (1MB)
	// │   └── level2-dir
	// │       └── level2-file.bin (1MB) <- This should be included in level1-dir even with maxLevel=1

	fileSize := 1024 * 1024 // 1MB
	fileContent := make([]byte, fileSize)

	// Create structure
	err := os.MkdirAll(filepath.Join(testRoot, "level1-dir", "level2-dir"), 0755)
	if err != nil {
		t.Fatalf("Failed to create directory structure: %v", err)
	}

	// Create files
	testFiles := map[string][]byte{
		"level1-dir/level1-file.bin":            fileContent,
		"level1-dir/level2-dir/level2-file.bin": fileContent,
	}

	for relativePath, content := range testFiles {
		fullPath := filepath.Join(testRoot, relativePath)
		err = os.WriteFile(fullPath, content, 0644)
		if err != nil {
			t.Fatalf("Failed to create file %s: %v", fullPath, err)
		}
	}

	testCases := []struct {
		maxLevel    int
		description string
		// level1DirExpectedMin is minimum expected size for level1-dir
		level1DirExpectedMin int64
	}{
		{
			maxLevel:             1,
			description:          "MaxLevel 1 - should include level2 files in level1-dir",
			level1DirExpectedMin: int64(2 * fileSize), // Both files should be counted
		},
		{
			maxLevel:             2,
			description:          "MaxLevel 2 - should include all files",
			level1DirExpectedMin: int64(2 * fileSize), // Both files should be counted
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			pathMap := map[string]int{testRoot: tc.maxLevel}
			exporter := NewExporter(pathMap, false)
			exporter.SetCollectionFlags(false, false, false)

			registry.MustRegister(diskUsage)
			exporter.performLiveAnalysis()

			metricFamilies, err := registry.Gather()
			if err != nil {
				t.Fatalf("Failed to gather metrics: %v", err)
			}

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

			// Check level1-dir size
			level1DirKey := fmt.Sprintf("%s/level1-dir:1", testRoot)
			level1DirSize, found := sizes[level1DirKey]
			if !found {
				t.Errorf("Level1-dir metric not found")
				return
			}

			if level1DirSize < tc.level1DirExpectedMin {
				t.Errorf("MaxLevel %d: level1-dir should be at least %.1f MB, got %.1f MB - level2 files missing!", 
					tc.maxLevel, float64(tc.level1DirExpectedMin)/(1024*1024), float64(level1DirSize)/(1024*1024))
			} else {
				t.Logf("✓ MaxLevel %d: level1-dir correctly includes deeper files: %.1f MB", 
					tc.maxLevel, float64(level1DirSize)/(1024*1024))
			}

			t.Logf("MaxLevel %d metrics:", tc.maxLevel)
			for key, size := range sizes {
				t.Logf("  %s: %.1f MB", key, float64(size)/(1024*1024))
			}
		})
	}
}