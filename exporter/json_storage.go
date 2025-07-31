// Package exporter provides JSON-based storage implementation for the disk usage exporter.
//
// The JSONStorage implementation solves circular reference issues that occur with
// the default gob serialization when storing fs.Item structures in BadgerDB.
//
// Key features:
// - JSON serialization avoids circular reference issues
// - BadgerDB provides efficient key-value storage
// - Depth limiting prevents infinite recursion
// - Automatic database connection recycling
// - Thread-safe operations with proper locking
//
// The storage flattens fs.Item hierarchies into JSONItem structures that can be
// safely serialized without circular references, then reconstructs the fs.Item
// hierarchy when loading from storage.
package exporter

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v3"
	"github.com/dundee/gdu/v5/pkg/analyze"
	"github.com/dundee/gdu/v5/pkg/fs"
	log "github.com/sirupsen/logrus"
)

// JSONItem represents a serializable version of fs.Item without circular references
type JSONItem struct {
	Path        string     `json:"path"`
	Name        string     `json:"name"`
	Size        int64      `json:"size"`
	Usage       int64      `json:"usage"`
	Mtime       time.Time  `json:"mtime"`
	Flag        rune       `json:"flag"`
	IsDirectory bool       `json:"is_directory"`
	ItemCount   int        `json:"item_count"`
	Mli         uint64     `json:"multi_linked_inode"`
	Files       []JSONItem `json:"files,omitempty"`
	BasePath    string     `json:"base_path,omitempty"`
}

// JSONStorageInterface defines the interface for JSON-based storage operations
type JSONStorageInterface interface {
	Open() (func(), error)
	StoreDir(path string, item fs.Item) error
	LoadDir(path string) (fs.Item, error)
	Close() error
	IsOpen() bool
}

// JSONStorage implements JSON-based storage for BadgerDB
//
// This storage implementation solves the circular reference issues that occur
// with gob serialization of fs.Item structures. By converting fs.Item to a
// flattened JSONItem structure and back, we avoid the stack overflow errors
// that can happen with deeply nested directory structures.
//
// Usage example:
//
//	storage := NewJSONStorage("/path/to/storage")
//	closeFn, err := storage.Open()
//	if err != nil { ... }
//	defer closeFn()
//
//	// Store directory data
//	err = storage.StoreDir("/path/to/dir", fsItem)
//
//	// Load directory data
//	fsItem, err := storage.LoadDir("/path/to/dir")
type JSONStorage struct {
	db          *badger.DB
	storagePath string
	mu          sync.RWMutex
	counter     int
	counterMu   sync.Mutex
}

// NewJSONStorage creates a new JSONStorage instance
func NewJSONStorage(storagePath string) *JSONStorage {
	return &JSONStorage{
		storagePath: storagePath,
	}
}

// Open opens the BadgerDB database
func (js *JSONStorage) Open() (func(), error) {
	js.mu.Lock()
	defer js.mu.Unlock()

	if js.db != nil {
		return func() {}, nil // Already open
	}

	options := badger.DefaultOptions(js.storagePath)
	options.Logger = nil // Disable badger logging

	db, err := badger.Open(options)
	if err != nil {
		return nil, fmt.Errorf("failed to open BadgerDB: %w", err)
	}

	js.db = db
	log.Infof("JSONStorage opened at path: %s", js.storagePath)

	return func() {
		js.mu.Lock()
		defer js.mu.Unlock()
		if js.db != nil {
			js.db.Close()
			js.db = nil
			log.Info("JSONStorage closed")
		}
	}, nil
}

// IsOpen returns true if the database is open
func (js *JSONStorage) IsOpen() bool {
	js.mu.RLock()
	defer js.mu.RUnlock()
	return js.db != nil
}

// StoreDir stores a directory item in JSON format
// Only stores directories up to the specified dir-level to optimize cache size
func (js *JSONStorage) StoreDir(path string, item fs.Item) error {
	return js.StoreDirWithLevel(path, item, -1) // Default: no level limit for backward compatibility
}

// StoreDirWithLevel stores a directory item with level-based filtering
func (js *JSONStorage) StoreDirWithLevel(path string, item fs.Item, maxLevel int) error {
	if err := js.checkCount(); err != nil {
		return err
	}

	js.mu.RLock()
	defer js.mu.RUnlock()

	if js.db == nil {
		return fmt.Errorf("database is not open")
	}

	if item == nil {
		log.Warnf("Attempted to store nil item for path: %s", path)
		return fmt.Errorf("item cannot be nil")
	}

	// Store the item with level filtering
	return js.storeDirRecursive(path, item, 0, maxLevel)
}

// storeDirRecursive recursively stores directories up to maxLevel
func (js *JSONStorage) storeDirRecursive(path string, item fs.Item, currentLevel, maxLevel int) error {
	// Skip if we've exceeded the maximum level (unless maxLevel is -1 for no limit)
	if maxLevel >= 0 && currentLevel > maxLevel {
		return nil
	}

	// OPTIMIZATION: Store lightweight aggregated stats instead of full fs.Item
	// This dramatically reduces storage size and memory usage

	// Create lightweight storage structure
	stats := make(map[string]interface{})
	stats["path"] = path
	stats["total_size"] = item.GetUsage()
	stats["stored_at"] = time.Now().Unix()
	stats["level"] = currentLevel

	// Only store what's actually needed based on collection flags
	// Since we don't have access to Exporter here, we store minimal data
	// and let the loading side filter what's needed
	stats["is_dir"] = item.IsDir()

	// Store basic file count for compatibility
	if item.IsDir() && item.GetFiles() != nil {
		stats["file_count"] = len(item.GetFiles())
	} else {
		stats["file_count"] = 0
	}

	// Serialize lightweight stats to JSON
	jsonData, err := json.Marshal(stats)
	if err != nil {
		return fmt.Errorf("failed to marshal lightweight stats: %w", err)
	}

	// Store in BadgerDB with level-specific key
	err = js.db.Update(func(txn *badger.Txn) error {
		key := []byte(fmt.Sprintf("%s:level:%d:stats", path, currentLevel))
		if err := txn.Set(key, jsonData); err != nil {
			return fmt.Errorf("failed to store in BadgerDB: %w", err)
		}
		log.Tracef("Stored stats for path: %s (level: %d, size: %d bytes)", path, currentLevel, len(jsonData))
		return nil
	})

	if err != nil {
		return err
	}

	// Recursively store subdirectories if within level limit
	if item.IsDir() && (maxLevel == -1 || currentLevel < maxLevel) {
		files := item.GetFiles()
		if files != nil {
			for _, subItem := range files {
				if subItem != nil && subItem.IsDir() {
					subPath := subItem.GetPath()
					if err := js.storeDirRecursive(subPath, subItem, currentLevel+1, maxLevel); err != nil {
						log.Warnf("Failed to store subdirectory %s at level %d: %v", subPath, currentLevel+1, err)
						// Continue with other subdirectories instead of failing completely
					}
				}
			}
		}
	}

	return nil
}

// LoadDir loads a directory item from JSON format
func (js *JSONStorage) LoadDir(path string) (fs.Item, error) {
	return js.LoadDirWithLevel(path, -1) // Default: no level limit for backward compatibility
}

// LoadDirWithLevel loads a directory item with level filtering
func (js *JSONStorage) LoadDirWithLevel(path string, maxLevel int) (fs.Item, error) {
	if err := js.checkCount(); err != nil {
		return nil, err
	}

	js.mu.RLock()
	defer js.mu.RUnlock()

	if js.db == nil {
		return nil, fmt.Errorf("database is not open")
	}

	// Try to load level-specific data first (new format)
	if maxLevel >= 0 {
		return js.loadLevelBasedData(path, maxLevel)
	}

	// OPTIMIZATION: Try to load lightweight stats first (legacy format)
	var jsonData []byte
	err := js.db.View(func(txn *badger.Txn) error {
		// Try lightweight stats key first
		item, err := txn.Get([]byte(path + ":stats"))
		if err != nil {
			// Fallback to old format for backward compatibility
			item, err = txn.Get([]byte(path))
			if err != nil {
				return fmt.Errorf("failed to read from BadgerDB: %w", err)
			}
		}

		return item.Value(func(val []byte) error {
			jsonData = make([]byte, len(val))
			copy(jsonData, val)
			return nil
		})
	})

	if err != nil {
		return nil, err
	}

	// Try to unmarshal as lightweight stats first
	var stats map[string]interface{}
	if err := json.Unmarshal(jsonData, &stats); err == nil {
		if _, hasPath := stats["path"]; hasPath {
			// This is lightweight stats format
			return js.createMinimalItemFromStats(stats), nil
		}
	}

	// Fallback to old format for backward compatibility
	var jsonItem JSONItem
	if err := json.Unmarshal(jsonData, &jsonItem); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON: %w", err)
	}

	// Convert JSONItem back to fs.Item
	fsItem, err := js.convertFromJSONItem(&jsonItem, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to convert from JSON item: %w", err)
	}

	log.Debugf("Loaded directory data for path: %s (format: legacy)", path)
	return fsItem, nil
}

// loadLevelBasedData loads level-specific cached data
func (js *JSONStorage) loadLevelBasedData(path string, maxLevel int) (fs.Item, error) {
	// Try to load data for level 0 (root level)
	var jsonData []byte
	levelKey := fmt.Sprintf("%s:level:0:stats", path)

	err := js.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(levelKey))
		if err != nil {
			return fmt.Errorf("failed to read level-based data from BadgerDB: %w", err)
		}

		return item.Value(func(val []byte) error {
			jsonData = make([]byte, len(val))
			copy(jsonData, val)
			return nil
		})
	})

	if err != nil {
		// If level-based data doesn't exist, fall back to legacy format
		log.Debugf("Level-based data not found for path: %s, falling back to legacy format", path)
		return js.loadLegacyFormat(path)
	}

	// Unmarshal level-specific stats
	var stats map[string]interface{}
	if err := json.Unmarshal(jsonData, &stats); err != nil {
		return nil, fmt.Errorf("failed to unmarshal level-based stats: %w", err)
	}

	// Create minimal item from level-specific stats
	item := js.createMinimalItemFromStats(stats)
	log.Debugf("Loaded level-based data for path: %s (max level: %d)", path, maxLevel)
	return item, nil
}

// loadLegacyFormat loads data using the legacy format
func (js *JSONStorage) loadLegacyFormat(path string) (fs.Item, error) {
	var jsonData []byte
	err := js.db.View(func(txn *badger.Txn) error {
		// Try lightweight stats key first
		item, err := txn.Get([]byte(path + ":stats"))
		if err != nil {
			// Fallback to old format for backward compatibility
			item, err = txn.Get([]byte(path))
			if err != nil {
				return fmt.Errorf("failed to read from BadgerDB: %w", err)
			}
		}

		return item.Value(func(val []byte) error {
			jsonData = make([]byte, len(val))
			copy(jsonData, val)
			return nil
		})
	})

	if err != nil {
		return nil, err
	}

	// Try to unmarshal as lightweight stats first
	var stats map[string]interface{}
	if err := json.Unmarshal(jsonData, &stats); err == nil {
		if _, hasPath := stats["path"]; hasPath {
			// This is lightweight stats format
			return js.createMinimalItemFromStats(stats), nil
		}
	}

	// Fallback to old format for backward compatibility
	var jsonItem JSONItem
	if err := json.Unmarshal(jsonData, &jsonItem); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON: %w", err)
	}

	// Convert JSONItem back to fs.Item
	fsItem, err := js.convertFromJSONItem(&jsonItem, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to convert from JSON item: %w", err)
	}

	return fsItem, nil
}

// createMinimalItemFromStats creates a minimal fs.Item from lightweight stats
func (js *JSONStorage) createMinimalItemFromStats(stats map[string]interface{}) fs.Item {
	// Create a minimal fs.Item that only contains essential information
	// This dramatically reduces memory usage compared to full fs.Item structure

	path, _ := stats["path"].(string)
	totalSize, _ := stats["total_size"].(float64)
	isDir, _ := stats["is_dir"].(bool)
	fileCount, _ := stats["file_count"].(float64)

	// Create minimal item that satisfies fs.Item interface
	item := &MinimalItem{
		path:      path,
		size:      int64(totalSize),
		isDir:     isDir,
		fileCount: int(fileCount),
	}

	log.Debugf("Created minimal item from stats for path: %s (size: %d, files: %d)", path, item.size, item.fileCount)
	return item
}

// MinimalItem implements fs.Item interface with minimal memory footprint
type MinimalItem struct {
	path      string
	size      int64
	isDir     bool
	fileCount int
}

// GetPath returns the path
func (m *MinimalItem) GetPath() string {
	return m.path
}

// GetUsage returns the size
func (m *MinimalItem) GetUsage() int64 {
	return m.size
}

// IsDir returns if it's a directory
func (m *MinimalItem) IsDir() bool {
	return m.isDir
}

// GetFiles returns nil - we don't store file structure for memory optimization
func (m *MinimalItem) GetFiles() fs.Files {
	return nil // Optimized: don't store full file structure
}

// UpdateStats does nothing for minimal item
func (m *MinimalItem) UpdateStats(hardLinkedItems fs.HardLinkedItems) {
	// No-op for minimal item
}

// GetItemCount returns the file count
func (m *MinimalItem) GetItemCount() int {
	return m.fileCount
}

// GetSize returns the size (alias for GetUsage)
func (m *MinimalItem) GetSize() int64 {
	return m.size
}

// AddFile does nothing for minimal item - not used for memory optimization
func (m *MinimalItem) AddFile(item fs.Item) {
	// No-op for minimal item to save memory
}

// GetMtime returns zero time for minimal item
func (m *MinimalItem) GetMtime() time.Time {
	return time.Time{}
}

// GetFlag returns 0 for minimal item
func (m *MinimalItem) GetFlag() rune {
	return 0
}

// SetFlag does nothing for minimal item
func (m *MinimalItem) SetFlag(flag rune) {
	// No-op for minimal item
}

// GetParent returns nil for minimal item
func (m *MinimalItem) GetParent() fs.Item {
	return nil
}

// SetParent does nothing for minimal item
func (m *MinimalItem) SetParent(parent fs.Item) {
	// No-op for minimal item
}

// GetMultiLinkedInode returns 0 for minimal item
func (m *MinimalItem) GetMultiLinkedInode() uint64 {
	return 0
}

// IsSymlink returns false for minimal item
func (m *MinimalItem) IsSymlink() bool {
	return false
}

// EncodeJSON does nothing for minimal item
func (m *MinimalItem) EncodeJSON(writer io.Writer, topLevel bool) error {
	// No-op for minimal item - we don't need JSON encoding for memory optimization
	return nil
}

// GetItemStats returns empty stats for minimal item
func (m *MinimalItem) GetItemStats(hardLinkedItems fs.HardLinkedItems) (int, int64, int64) {
	return m.fileCount, m.size, m.size
}

// GetName returns the file/directory name
func (m *MinimalItem) GetName() string {
	if m.path == "" {
		return ""
	}
	// Extract name from path
	parts := strings.Split(m.path, "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return m.path
}

// GetType returns 'd' for directory, 'f' for file
func (m *MinimalItem) GetType() string {
	if m.isDir {
		return "d"
	}
	return "f"
}

// RemoveFile does nothing for minimal item
func (m *MinimalItem) RemoveFile(item fs.Item) {
	// No-op for minimal item to save memory
}

// SetFiles does nothing for minimal item
func (m *MinimalItem) SetFiles(files fs.Files) {
	// No-op for minimal item to save memory
}

// Close closes the database
func (js *JSONStorage) Close() error {
	js.mu.Lock()
	defer js.mu.Unlock()

	if js.db != nil {
		err := js.db.Close()
		js.db = nil
		return err
	}
	return nil
}

// convertToJSONItem converts fs.Item to JSONItem, avoiding circular references
func (js *JSONStorage) convertToJSONItem(item fs.Item, depth, maxDepth int) (*JSONItem, error) {
	if depth > maxDepth {
		log.Warnf("Maximum depth reached while converting item: %s", item.GetPath())
		return nil, fmt.Errorf("maximum depth %d exceeded", maxDepth)
	}

	jsonItem := &JSONItem{
		Path:        item.GetPath(),
		Name:        item.GetName(),
		Size:        item.GetSize(),
		Usage:       item.GetUsage(),
		Mtime:       item.GetMtime(),
		Flag:        item.GetFlag(),
		IsDirectory: item.IsDir(),
		ItemCount:   item.GetItemCount(),
		Mli:         item.GetMultiLinkedInode(),
	}

	// For directories, include BasePath and Files
	if item.IsDir() {
		// Extract BasePath if this is an analyze.Dir type
		if dir, ok := item.(*analyze.Dir); ok {
			jsonItem.BasePath = dir.BasePath
		} else {
			// For other directory types, derive BasePath from the path
			jsonItem.BasePath = filepath.Dir(item.GetPath())
		}

		// Convert child files (but don't go too deep to avoid circular references)
		files := item.GetFiles()
		if len(files) > 0 && depth < maxDepth {
			jsonItem.Files = make([]JSONItem, 0, len(files))
			for _, file := range files {
				if file == nil {
					continue
				}

				childItem, err := js.convertToJSONItem(file, depth+1, maxDepth)
				if err != nil {
					log.Warnf("Failed to convert child item %s: %v", file.GetPath(), err)
					continue
				}
				jsonItem.Files = append(jsonItem.Files, *childItem)
			}
		}
	}

	return jsonItem, nil
}

// convertFromJSONItem converts JSONItem back to fs.Item
func (js *JSONStorage) convertFromJSONItem(jsonItem *JSONItem, parent fs.Item) (fs.Item, error) {
	if jsonItem.IsDirectory {
		// Create an analyze.Dir
		dir := &analyze.Dir{
			File: &analyze.File{
				Name:   jsonItem.Name,
				Size:   jsonItem.Size,
				Usage:  jsonItem.Usage,
				Mtime:  jsonItem.Mtime,
				Flag:   jsonItem.Flag,
				Parent: parent,
				Mli:    jsonItem.Mli,
			},
			BasePath:  jsonItem.BasePath,
			ItemCount: jsonItem.ItemCount,
		}

		// Convert child files
		if len(jsonItem.Files) > 0 {
			files := make(fs.Files, 0, len(jsonItem.Files))
			for i := range jsonItem.Files {
				child, err := js.convertFromJSONItem(&jsonItem.Files[i], dir)
				if err != nil {
					log.Warnf("Failed to convert child item %s: %v", jsonItem.Files[i].Path, err)
					continue
				}
				files = append(files, child)
			}
			dir.Files = files
		}

		return dir, nil
	} else {
		// Create an analyze.File
		file := &analyze.File{
			Name:   jsonItem.Name,
			Size:   jsonItem.Size,
			Usage:  jsonItem.Usage,
			Mtime:  jsonItem.Mtime,
			Flag:   jsonItem.Flag,
			Parent: parent,
			Mli:    jsonItem.Mli,
		}
		return file, nil
	}
}

// checkCount manages database connection recycling to prevent resource leaks
func (js *JSONStorage) checkCount() error {
	js.counterMu.Lock()
	defer js.counterMu.Unlock()

	js.counter++
	if js.counter >= 10000 {
		log.Debug("JSONStorage: Recycling database connection after 10000 operations")
		js.counter = 0

		// Close and reopen the database
		js.mu.Lock()
		if js.db != nil {
			if err := js.db.Close(); err != nil {
				js.mu.Unlock()
				return fmt.Errorf("failed to close database during recycling: %w", err)
			}
		}

		options := badger.DefaultOptions(js.storagePath)
		options.Logger = nil
		db, err := badger.Open(options)
		if err != nil {
			js.mu.Unlock()
			return fmt.Errorf("failed to reopen database during recycling: %w", err)
		}
		js.db = db
		js.mu.Unlock()
	}

	return nil
}

// GetDirForPath creates a directory item for the specified path (similar to Storage.GetDirForPath)
func (js *JSONStorage) GetDirForPath(path string) (fs.Item, error) {
	return js.LoadDir(path)
}
