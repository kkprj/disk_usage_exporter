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
	"path/filepath"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v3"
	"github.com/dundee/gdu/v5/pkg/analyze"
	"github.com/dundee/gdu/v5/pkg/fs"
	log "github.com/sirupsen/logrus"
)

// JSONItem represents a serializable version of fs.Item without circular references
type JSONItem struct {
	Path         string      `json:"path"`
	Name         string      `json:"name"`
	Size         int64       `json:"size"`
	Usage        int64       `json:"usage"`
	Mtime        time.Time   `json:"mtime"`
	Flag         rune        `json:"flag"`
	IsDirectory  bool        `json:"is_directory"`
	ItemCount    int         `json:"item_count"`
	Mli          uint64      `json:"multi_linked_inode"`
	Files        []JSONItem  `json:"files,omitempty"`
	BasePath     string      `json:"base_path,omitempty"`
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
//   storage := NewJSONStorage("/path/to/storage")
//   closeFn, err := storage.Open()
//   if err != nil { ... }
//   defer closeFn()
//   
//   // Store directory data
//   err = storage.StoreDir("/path/to/dir", fsItem)
//   
//   // Load directory data  
//   fsItem, err := storage.LoadDir("/path/to/dir")
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
func (js *JSONStorage) StoreDir(path string, item fs.Item) error {
	if err := js.checkCount(); err != nil {
		return err
	}

	js.mu.RLock()
	defer js.mu.RUnlock()

	if js.db == nil {
		return fmt.Errorf("database is not open")
	}

	// Convert fs.Item to JSONItem to avoid circular references
	jsonItem, err := js.convertToJSONItem(item, 0, 10) // Limit depth to prevent infinite recursion
	if err != nil {
		return fmt.Errorf("failed to convert item to JSON: %w", err)
	}

	// Serialize to JSON
	jsonData, err := json.Marshal(jsonItem)
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}

	// Store in BadgerDB
	return js.db.Update(func(txn *badger.Txn) error {
		key := []byte(path)
		if err := txn.Set(key, jsonData); err != nil {
			return fmt.Errorf("failed to store in BadgerDB: %w", err)
		}
		log.Debugf("Stored directory data for path: %s (size: %d bytes)", path, len(jsonData))
		return nil
	})
}

// LoadDir loads a directory item from JSON format
func (js *JSONStorage) LoadDir(path string) (fs.Item, error) {
	if err := js.checkCount(); err != nil {
		return nil, err
	}

	js.mu.RLock()
	defer js.mu.RUnlock()

	if js.db == nil {
		return nil, fmt.Errorf("database is not open")
	}

	var jsonData []byte
	err := js.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(path))
		if err != nil {
			return fmt.Errorf("failed to read from BadgerDB: %w", err)
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

	// Deserialize from JSON
	var jsonItem JSONItem
	if err := json.Unmarshal(jsonData, &jsonItem); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON: %w", err)
	}

	// Convert JSONItem back to fs.Item
	fsItem, err := js.convertFromJSONItem(&jsonItem, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to convert from JSON item: %w", err)
	}

	log.Debugf("Loaded directory data for path: %s", path)
	return fsItem, nil
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

