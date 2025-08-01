package storage

import "time"

// DiskStat represents disk usage statistics for a path
// This struct corresponds to the aggregatedStats structure from the current implementation
type DiskStat struct {
	// Primary identifiers
	Path  string `json:"path"`  // Directory path
	Level int    `json:"level"` // Directory depth level

	// Core metrics
	Size      int64 `json:"size"`       // Total size in bytes
	FileCount int64 `json:"file_count"` // Number of files
	DirCount  int64 `json:"dir_count"`  // Number of subdirectories

	// Size distribution buckets
	SizeBuckets map[string]int64 `json:"size_buckets"` // size_range -> count mapping

	// Top files tracking
	TopFiles    []TopFileInfo `json:"top_files"`    // Largest files list
	OthersTotal int64         `json:"others_total"` // Total size of non-top files
	OthersCount int64         `json:"others_count"` // Count of non-top files

	// Metadata
	LastUpdated time.Time `json:"last_updated"` // When this stat was last updated
}

// TopFileInfo represents information about large files
type TopFileInfo struct {
	Path string `json:"path"` // File path
	Size int64  `json:"size"` // File size in bytes
	Rank int    `json:"rank"` // Ranking by size (1 = largest)
}

// SizeRange constants for bucket categorization
const (
	SizeRange0B      = "0B"
	SizeRange0To1KB  = "0-1KB"
	SizeRange1KBTo1MB = "1KB-1MB"
	SizeRange1MBTo100MB = "1MB-100MB"
	SizeRange100MBPlus = "100MB+"
)

// GetSizeRange returns the appropriate size range bucket for a file size
func GetSizeRange(size int64) string {
	if size == 0 {
		return SizeRange0B
	} else if size <= 1024 {
		return SizeRange0To1KB
	} else if size <= 1024*1024 {
		return SizeRange1KBTo1MB
	} else if size <= 100*1024*1024 {
		return SizeRange1MBTo100MB
	} else {
		return SizeRange100MBPlus
	}
}

// NewDiskStat creates a new DiskStat with initialized maps
func NewDiskStat(path string, level int) *DiskStat {
	return &DiskStat{
		Path:        path,
		Level:       level,
		SizeBuckets: make(map[string]int64),
		TopFiles:    make([]TopFileInfo, 0),
		LastUpdated: time.Now(),
	}
}