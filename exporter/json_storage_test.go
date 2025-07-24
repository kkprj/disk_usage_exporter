package exporter

import (
	"os"
	"testing"
	"time"

	"github.com/dundee/gdu/v5/pkg/analyze"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJSONStorage(t *testing.T) {
	// Create a temporary directory for the test database
	tmpDir, err := os.MkdirTemp("", "json_storage_test")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	// Create JSONStorage instance
	storage := NewJSONStorage(tmpDir)

	// Open the storage
	closeFn, err := storage.Open()
	require.NoError(t, err)
	defer closeFn()

	// Create a test directory structure
	testFile := &analyze.File{
		Name:  "test.txt",
		Size:  1024,
		Usage: 1024,
		Mtime: time.Now(),
		Flag:  ' ',
		Mli:   0,
	}

	testDir := &analyze.Dir{
		File: &analyze.File{
			Name:  "testdir",
			Size:  2048,
			Usage: 2048,
			Mtime: time.Now(),
			Flag:  ' ',
			Mli:   0,
		},
		BasePath:  "/tmp",
		ItemCount: 2,
	}

	// Set up parent-child relationships
	testFile.SetParent(testDir)
	testDir.AddFile(testFile)

	// Test storing directory
	testPath := "/tmp/testdir"
	err = storage.StoreDir(testPath, testDir)
	assert.NoError(t, err)

	// Test loading directory
	loadedItem, err := storage.LoadDir(testPath)
	require.NoError(t, err)
	assert.NotNil(t, loadedItem)

	// Verify loaded data
	assert.Equal(t, "testdir", loadedItem.GetName())
	assert.True(t, loadedItem.IsDir())
	assert.Equal(t, int64(2048), loadedItem.GetUsage())
	assert.Equal(t, 2, loadedItem.GetItemCount())

	// Verify child files
	files := loadedItem.GetFiles()
	assert.Len(t, files, 1)
	assert.Equal(t, "test.txt", files[0].GetName())
	assert.Equal(t, int64(1024), files[0].GetUsage())

	// Test IsOpen
	assert.True(t, storage.IsOpen())

	// Test Close
	err = storage.Close()
	assert.NoError(t, err)
	assert.False(t, storage.IsOpen())
}

func TestJSONStorageInterface(t *testing.T) {
	// Verify that JSONStorage implements the interface
	var _ JSONStorageInterface = (*JSONStorage)(nil)
}

func TestJSONItemSerialization(t *testing.T) {
	// Create a temporary directory for the test database
	tmpDir, err := os.MkdirTemp("", "json_serialization_test")
	require.NoError(t, err)
	defer os.RemoveAll(tmpDir)

	storage := NewJSONStorage(tmpDir)
	closeFn, err := storage.Open()
	require.NoError(t, err)
	defer closeFn()

	// Create a complex directory structure with multiple levels
	rootDir := &analyze.Dir{
		File: &analyze.File{
			Name:  "root",
			Size:  5000,
			Usage: 5000,
			Mtime: time.Now(),
			Flag:  ' ',
		},
		BasePath:  "/",
		ItemCount: 3,
	}

	subDir := &analyze.Dir{
		File: &analyze.File{
			Name:   "subdir",
			Size:   3000,
			Usage:  3000,
			Mtime:  time.Now(),
			Flag:   ' ',
			Parent: rootDir,
		},
		BasePath:  "/root",
		ItemCount: 2,
	}

	file1 := &analyze.File{
		Name:   "file1.txt",
		Size:   1000,
		Usage:  1000,
		Mtime:  time.Now(),
		Flag:   ' ',
		Parent: subDir,
	}

	file2 := &analyze.File{
		Name:   "file2.txt",
		Size:   2000,
		Usage:  2000,
		Mtime:  time.Now(),
		Flag:   ' ',
		Parent: rootDir,
	}

	// Build the directory structure
	subDir.AddFile(file1)
	rootDir.AddFile(subDir)
	rootDir.AddFile(file2)

	// Store and load
	testPath := "/root"
	err = storage.StoreDir(testPath, rootDir)
	assert.NoError(t, err)

	loadedItem, err := storage.LoadDir(testPath)
	require.NoError(t, err)

	// Verify the structure is preserved
	assert.Equal(t, "root", loadedItem.GetName())
	assert.Equal(t, int64(5000), loadedItem.GetUsage())

	files := loadedItem.GetFiles()
	assert.Len(t, files, 2)

	// Find the subdirectory and regular file
	var loadedSubDir *analyze.Dir
	for _, f := range files {
		if f.IsDir() {
			loadedSubDir = f.(*analyze.Dir)
		} else {
			// This should be file2
			assert.Equal(t, "file2.txt", f.GetName())
			assert.Equal(t, int64(2000), f.GetUsage())
		}
	}

	require.NotNil(t, loadedSubDir)
	assert.Equal(t, "subdir", loadedSubDir.GetName())

	subFiles := loadedSubDir.GetFiles()
	assert.Len(t, subFiles, 1)
	assert.Equal(t, "file1.txt", subFiles[0].GetName())
	assert.Equal(t, int64(1000), subFiles[0].GetUsage())
}