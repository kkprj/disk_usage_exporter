# SQLite 기반 디스크 사용량 익스포터 마이그레이션 계획

## 개요

현재 메모리 기반 캐시 시스템을 SQLite 로컬 데이터베이스로 마이그레이션하여 수억 개 파일 처리 시 메모리 사용량 급증 문제를 해결합니다.

## 현재 상황 분석

### 메모리 사용량 문제
- **현재**: `aggregatedStats` 구조체가 디렉토리당 ~40KB 메모리 사용
- **1억 개 파일 시나리오**: 약 404GB 메모리 필요 (치명적)
- **근본 원인**: 모든 디렉토리 통계를 메모리에 보관

### 기존 아키텍처
```
godirwalk → aggregatedStats (메모리) → Prometheus 메트릭(/metrics API 응답)
```

### 목표 아키텍처
```
godirwalk → SQLite 데이터베이스 → Prometheus 메트릭(/metrics API 응답)
```

## 마이그레이션 계획

### Phase 1: 사전 준비 및 기반 작업

#### 1.1 사용하지 않는 코드 제거 ✅
- [x] `chunkSize` 관련 코드 완전 제거
- [x] 단위 테스트 검증 완료
- [x] 설정 파일 정리 완료

#### 1.2 Git 체크포인트 생성
```bash
git add .
git commit -m "Phase 1 완료: 사용하지 않는 코드 제거 및 정리

- chunkSize 관련 코드 완전 제거
- 단위 테스트 통과 확인
- 설정 파일 정리
🤖 Generated with [Claude Code](https://claude.ai/code)"
```

### Phase 2: SQLite 스토리지 계층 구현

#### 2.1 의존성 추가
```bash
go get github.com/mattn/go-sqlite3
```

#### 2.2 SQLite 스토리지 인터페이스 설계
```go
// storage/interface.go
type Storage interface {
    Initialize() error
    StoreBatch(stats []DiskStat) error
    GetMetricsData(paths map[string]int) ([]DiskStat, error)
    Close() error
    Cleanup() error
}

// storage/sqlite.go
type SQLiteStorage struct {
    db       *sql.DB
    dbPath   string
    batchTx  *sql.Tx
    batchStmt *sql.Stmt
}
```

#### 2.3 데이터베이스 스키마 설계
```sql
CREATE TABLE IF NOT EXISTS disk_stats (
    path TEXT NOT NULL,
    level INTEGER NOT NULL,
    size INTEGER NOT NULL,
    file_count INTEGER NOT NULL,
    dir_count INTEGER NOT NULL,
    last_updated DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (path, level)
);

CREATE INDEX IF NOT EXISTS idx_level ON disk_stats(level);
CREATE INDEX IF NOT EXISTS idx_path_prefix ON disk_stats(path);
CREATE INDEX IF NOT EXISTS idx_size ON disk_stats(size DESC);
```

#### 2.4 성능 최적화 설정
```go
func (s *SQLiteStorage) optimizeDatabase() error {
    optimizations := []string{
        "PRAGMA journal_mode=WAL",      // 동시 읽기/쓰기
        "PRAGMA synchronous=NORMAL",    // 성능 vs 안정성 균형
        "PRAGMA cache_size=10000",      // 10MB 캐시
        "PRAGMA temp_store=memory",     // 임시 테이블 메모리 저장
        "PRAGMA mmap_size=268435456",   // 256MB 메모리 맵
    }
    
    for _, pragma := range optimizations {
        if _, err := s.db.Exec(pragma); err != nil {
            return err
        }
    }
    return nil
}
```

### Phase 3: 프로세서 계층 리팩토링

#### 3.1 새로운 프로세서 구조
```go
// exporter/processor.go
type processor struct {
    exporter    *Exporter
    storage     Storage
    rootPath    string
    maxLevel    int
    batchBuffer []DiskStat
    batchSize   int
    mu          sync.Mutex
}
```

#### 3.2 배치 처리 메커니즘
```go
func (p *processor) processBatch() error {
    if len(p.batchBuffer) == 0 {
        return nil
    }
    
    // SQLite에 배치 저장
    if err := p.storage.StoreBatch(p.batchBuffer); err != nil {
        return err
    }
    
    // 메모리 정리
    p.batchBuffer = p.batchBuffer[:0]
    runtime.GC()
    
    return nil
}
```

#### 3.3 godirwalk 콜백 수정
```go
func (p *processor) walkCallback(osPathname string, de *godirwalk.Dirent) error {
    // 기존 통계 수집 로직
    stat := p.collectStats(osPathname, de)
    
    // 배치 버퍼에 추가
    p.mu.Lock()
    p.batchBuffer = append(p.batchBuffer, stat)
    
    // 배치 크기 도달 시 처리
    if len(p.batchBuffer) >= p.batchSize {
        if err := p.processBatch(); err != nil {
            p.mu.Unlock()
            return err
        }
    }
    p.mu.Unlock()
    
    return nil
}
```

### Phase 4: 메트릭 수집 계층 수정

#### 4.1 SQLite 기반 메트릭 수집
```go
func (e *Exporter) publishMetricsFromStorage() error {
    // SQLite에서 데이터 조회
    stats, err := e.storage.GetMetricsData(e.paths)
    if err != nil {
        return err
    }
    
    // Prometheus 메트릭 발행
    for _, stat := range stats {
        e.publishSingleMetric(stat)
    }
    
    return nil
}
```

#### 4.2 쿼리 최적화
```go
func (s *SQLiteStorage) GetMetricsData(paths map[string]int) ([]DiskStat, error) {
    query := `
        SELECT path, level, size, file_count, dir_count 
        FROM disk_stats 
        WHERE level <= ? AND (
    `
    
    var args []interface{}
    var conditions []string
    
    for rootPath, maxLevel := range paths {
        conditions = append(conditions, "path = ? OR path LIKE ?")
        args = append(args, maxLevel, rootPath, rootPath+"/%")
    }
    
    query += strings.Join(conditions, " OR ") + ")"
    
    rows, err := s.db.Query(query, args...)
    // 결과 처리...
}
```

### Phase 5: 설정 및 플래그 수정

#### 5.1 설정 구조체 수정
```go
type Config struct {
    // 기존 설정들...
    
    // SQLite 관련 설정
    DBPath          string `yaml:"db-path"`
    BatchSize       int    `yaml:"batch-size"`
    DBMaxConnections int   `yaml:"db-max-connections"`
    
    // 제거할 설정
    // DiskCaching     bool   `yaml:"disk-caching"` // 제거
}
```

#### 5.2 CLI 플래그 수정
```go
// cmd/root.go
func init() {
    // 새로운 플래그
    rootCmd.PersistentFlags().String("db-path", "/tmp/disk-usage.db", "SQLite database path")
    rootCmd.PersistentFlags().Int("batch-size", 1000, "Batch size for database operations")
    
    // 제거할 플래그
    // rootCmd.PersistentFlags().Bool("disk-caching", false, "Enable disk caching") // 제거
}
```

#### 5.3 설정 파일 업데이트
```yaml
# config-sqlite.yml
# SQLite 기반 디스크 사용량 익스포터 설정

# 기본 설정
analyzed-path: "/srv/nfs/shared/meta-test"
bind-address: "0.0.0.0:9996"
dir-level: 3
mode: "http"
follow-symlinks: false

# SQLite 데이터베이스 설정
db-path: "/tmp/disk-usage.db"
batch-size: 1000
db-max-connections: 1

# 성능 설정
max-procs: 12
log-level: "debug"

# 메트릭 수집 설정
dir-count: false
file-count: false
size-bucket: true

# 스캔 설정 (백그라운드 모드용)
storage-path: "/tmp/disk-usage-cache"
scan-interval-minutes: 60

# 무시할 디렉토리
ignore-dirs:
  - /proc
  - /dev
  - /sys
  - /run
  - /tmp
```

### Phase 6: 동시성 및 성능 최적화

#### 6.1 워커 풀 SQLite 통합
```go
func (e *Exporter) streamingWorker(workerID int, workChan <-chan workUnit, resultChan chan<- workResult, wg *sync.WaitGroup) {
    defer wg.Done()
    
    // 워커별 SQLite 연결 (읽기 전용)
    storage, err := NewSQLiteStorage(e.dbPath, true) // readOnly = true
    if err != nil {
        log.Errorf("Worker %d: Failed to create storage: %v", workerID, err)
        return
    }
    defer storage.Close()
    
    for work := range workChan {
        processor := newProcessor(e, storage, work.rootPath, work.maxLevel)
        
        // 스캔 실행 후 결과를 메인 스토리지에 전송
        if err := processor.processDirectory(); err != nil {
            resultChan <- workResult{rootPath: work.rootPath, err: err}
            continue
        }
        
        resultChan <- workResult{rootPath: work.rootPath, err: nil}
    }
}
```

#### 6.2 배치 쓰기 최적화
```go
func (s *SQLiteStorage) StoreBatch(stats []DiskStat) error {
    tx, err := s.db.Begin()
    if err != nil {
        return err
    }
    defer tx.Rollback()
    
    stmt, err := tx.Prepare(`
        INSERT OR REPLACE INTO disk_stats 
        (path, level, size, file_count, dir_count, last_updated) 
        VALUES (?, ?, ?, ?, ?, datetime('now'))
    `)
    if err != nil {
        return err
    }
    defer stmt.Close()
    
    for _, stat := range stats {
        if _, err := stmt.Exec(stat.Path, stat.Level, stat.Size, stat.FileCount, stat.DirCount); err != nil {
            return err
        }
    }
    
    return tx.Commit()
}
```

### Phase 7: 테스트 및 검증

#### 7.1 단위 테스트 수정
```go
// exporter/sqlite_test.go
func TestSQLiteStorage(t *testing.T) {
    storage := NewSQLiteStorage(":memory:", false)
    defer storage.Close()
    
    // 테스트 데이터 생성
    testStats := []DiskStat{
        {Path: "/test", Level: 0, Size: 1000, FileCount: 10},
        {Path: "/test/sub", Level: 1, Size: 500, FileCount: 5},
    }
    
    // 배치 저장 테스트
    err := storage.StoreBatch(testStats)
    assert.NoError(t, err)
    
    // 데이터 조회 테스트
    paths := map[string]int{"/test": 2}
    results, err := storage.GetMetricsData(paths)
    assert.NoError(t, err)
    assert.Len(t, results, 2)
}
```

#### 7.2 성능 테스트
```go
func BenchmarkSQLiteStorage(b *testing.B) {
    storage := NewSQLiteStorage(":memory:", false)
    defer storage.Close()
    
    // 대량 데이터 생성
    stats := make([]DiskStat, 10000)
    for i := range stats {
        stats[i] = DiskStat{
            Path: fmt.Sprintf("/test/path/%d", i),
            Level: i % 5,
            Size: int64(i * 1000),
            FileCount: i % 100,
        }
    }
    
    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        storage.StoreBatch(stats)
    }
}
```

#### 7.3 통합 테스트
```go
func TestSQLiteMigrationIntegration(t *testing.T) {
    // 임시 DB 파일 생성
    dbPath := "/tmp/test-disk-usage.db"
    defer os.Remove(dbPath)
    
    // SQLite 기반 익스포터 생성
    pathMap := map[string]int{"/tmp/test-complex": 3}
    exporter := NewExporter(pathMap, false)
    exporter.SetDBPath(dbPath)
    exporter.SetBatchSize(100)
    
    // 스캔 실행
    err := exporter.performScan()
    assert.NoError(t, err)
    
    // 메트릭 검증
    registry := prometheus.NewRegistry()
    registry.MustRegister(diskUsage, diskUsageSizeBucket)
    
    err = exporter.publishMetricsFromStorage()
    assert.NoError(t, err)
    
    metricFamilies, err := registry.Gather()
    assert.NoError(t, err)
    assert.NotEmpty(t, metricFamilies)
}
```

### Phase 8: 배포 및 마이그레이션

#### 8.1 설정 파일 마이그레이션 가이드
```bash
# 기존 설정에서 제거할 항목들
# - disk-caching: true
# - chunk-size: 1000

# 새로 추가할 항목들
# + db-path: "/var/lib/disk-usage/data.db"
# + batch-size: 1000
```

#### 8.2 데이터베이스 초기화 스크립트
```go
func (e *Exporter) initializeDatabase() error {
    if e.storage == nil {
        storage, err := NewSQLiteStorage(e.dbPath, false)
        if err != nil {
            return fmt.Errorf("failed to initialize SQLite storage: %v", err)
        }
        e.storage = storage
    }
    
    return e.storage.Initialize()
}
```

#### 8.3 기존 캐시 정리
```go
func (e *Exporter) cleanupLegacyCache() error {
    // 기존 aggregatedStats 맵 정리
    if e.cachedStats != nil {
        e.cachedStats = nil
    }
    
    // 메모리 정리
    runtime.GC()
    
    log.Info("Legacy cache cleaned up successfully")
    return nil
}
```

## 예상 성능 개선

### 메모리 사용량
- **현재**: 1억 파일 → ~404GB 메모리
- **개선 후**: 1억 파일 → ~100MB 메모리 (4000배 개선)

### 처리 속도
- **배치 처리**: 1000개 레코드 단위로 일괄 처리
- **인덱스 활용**: path, level 기반 빠른 검색
- **WAL 모드**: 동시 읽기/쓰기 성능 향상

### 확장성
- **디스크 기반**: 메모리 제약 없이 무제한 확장
- **지속성**: 프로세스 재시작 후에도 데이터 유지
- **쿼리 최적화**: SQL 기반 복잡한 집계 연산

## Git 커밋 전략

각 Phase별로 체크포인트 생성:

```bash
# Phase 2 완료
git commit -m "Phase 2 완료: SQLite 스토리지 계층 구현

- SQLite 인터페이스 및 구현체 추가
- 데이터베이스 스키마 설계
- 성능 최적화 설정 적용
🤖 Generated with [Claude Code](https://claude.ai/code)"

# Phase 3 완료  
git commit -m "Phase 3 완료: 프로세서 계층 리팩토링

- 메모리 기반 처리를 SQLite 배치 처리로 변경
- godirwalk 콜백 SQLite 통합
- 배치 처리 메커니즘 구현
🤖 Generated with [Claude Code](https://claude.ai/code)"

# 최종 완료
git commit -m "SQLite 마이그레이션 완료

- 메모리 사용량 4000배 개선 (404GB → 100MB)
- 수억 개 파일 처리 지원
- 단위 테스트 및 성능 테스트 통과
🤖 Generated with [Claude Code](https://claude.ai/code)"
```

## 다음 단계

1. **Phase 1**: 기반 작업 (완료) ✅
2. **Phase 2**: SQLite 스토리지 구현 (진행 예정)
3. **Phase 3**: 프로세서 리팩토링
4. **Phase 4**: 메트릭 수집 수정  
5. **Phase 5**: 설정 업데이트
6. **Phase 6**: 성능 최적화
7. **Phase 7**: 테스트 및 검증
8. **Phase 8**: 배포 준비

이 계획을 통해 현재 메모리 급증 문제를 근본적으로 해결하고, 수억 개 파일도 안정적으로 처리할 수 있는 시스템으로 업그레이드할 수 있습니다.