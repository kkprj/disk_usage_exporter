package exporter

import (
	"fmt"
	"log"

	"github.com/dundee/gdu/v5/pkg/analyze"
)

// ExampleJSONStorageUsage demonstrates how to use JSONStorage as a replacement
// for the existing Storage implementation to avoid circular reference issues.
func ExampleJSONStorageUsage() {
	// Create a new JSON storage instance
	storage := NewJSONStorage("/tmp/disk_usage_cache")
	
	// Open the storage
	closeFn, err := storage.Open()
	if err != nil {
		log.Fatalf("Failed to open storage: %v", err)
	}
	defer closeFn()
	
	// Example: Store directory analysis results
	// This would typically be called after running directory analysis
	path := "/home/user/documents"
	
	// Create analyzer and analyze directory
	analyzer := analyze.CreateAnalyzer()
	analyzer.SetFollowSymlinks(false)
	
	// Analyze the directory (this would typically be your actual path)
	// dir := analyzer.AnalyzeDir(path, nil, false)
	// dir.UpdateStats(fs.HardLinkedItems{})
	
	// For demonstration, create a mock directory
	dir := &analyze.Dir{
		File: &analyze.File{
			Name:  "documents",
			Size:  1024000,
			Usage: 1024000,
		},
		BasePath: "/home/user",
		ItemCount: 10,
	}
	
	// Store the analysis results
	if err := storage.StoreDir(path, dir); err != nil {
		log.Fatalf("Failed to store directory: %v", err)
	}
	
	fmt.Printf("Successfully stored analysis for path: %s\n", path)
	
	// Later, load the stored results
	loadedDir, err := storage.LoadDir(path)
	if err != nil {
		log.Fatalf("Failed to load directory: %v", err)
	}
	
	fmt.Printf("Loaded directory: %s, size: %d bytes, items: %d\n", 
		loadedDir.GetName(), loadedDir.GetUsage(), loadedDir.GetItemCount())
	
	// Check if storage is open
	if storage.IsOpen() {
		fmt.Println("Storage is open and ready for operations")
	}
}

// IntegrateWithExporter shows how JSONStorage could be integrated into the 
// existing Exporter structure to replace the problematic gob-based storage.
func IntegrateWithExporter(e *Exporter) {
	// This is a conceptual example of how you might integrate JSONStorage
	// into the existing exporter to replace the problematic storage writes
	// that currently cause circular reference issues.
	
	// Example integration points:
	
	// 1. Replace storage initialization:
	if e.storagePath != "" {
		jsonStorage := NewJSONStorage(e.storagePath)
		closeFn, err := jsonStorage.Open()
		if err != nil {
			log.Printf("Failed to initialize JSON storage: %v", err)
			return
		}
		// Store the close function for cleanup
		e.storageCloseFn = closeFn
		
		// Note: You would need to modify the Exporter struct to include
		// a jsonStorage field and update the storage-related methods
		// to use JSONStorage instead of the existing Storage.
	}
	
	// 2. Replace storage writes in performAnalysisWithStored:
	// Instead of: e.queueStorageWrite(path, result)
	// Use: jsonStorage.StoreDir(path, result)
	
	// 3. Replace storage reads in loadFromStorage:
	// Instead of: storage.LoadDir(dir)  
	// Use: fsItem, err := jsonStorage.LoadDir(path)
	
	fmt.Println("JSONStorage integration points identified")
	fmt.Println("To fully integrate:")
	fmt.Println("1. Add JSONStorage field to Exporter struct")
	fmt.Println("2. Replace storage writes with JSONStorage.StoreDir calls")
	fmt.Println("3. Replace storage reads with JSONStorage.LoadDir calls")
	fmt.Println("4. Update cleanup code to use JSONStorage.Close")
}