# Disk Usage Prometheus Exporter



## 🚀 주요 기능

- **메모리 효율성**: 대용량 파일시스템(1M+ 파일)에서 99% 메모리 사용량 감소
- **SQLite 기반 저장**: 영구 데이터 저장과 고성능 배치 처리
- **Godirwalk 최적화**: 고성능 디렉토리 순회 라이브러리 활용
- **Worker Pool 아키텍처**: max-procs 설정으로 CPU 사용량 제어
- **계층적 통계**: 디렉토리 레벨, 타입별, 크기별 집계
- **백그라운드 캐싱**: 일관된 성능을 위한 선택적 캐싱
- **다중 경로 지원**: 서로 다른 깊이 레벨의 여러 디렉토리 모니터링
- **설정 가능한 CPU 사용량**: 최적 성능을 위한 병렬 처리 조정


## 사용법

```
Usage:
  disk_usage_exporter [flags]

Flags:
  -p, --analyzed-path string              Path where to analyze disk usage (default "/")
      --basic-auth-users stringToString   Basic Auth users and their passwords as bcypt hashes (default [])
  -b, --bind-address string               Address to bind to (default "0.0.0.0:9995")
  -c, --config string                     config file (default is $HOME/.disk_usage_exporter.yaml)
  -l, --dir-level int                     Directory nesting level to show (0 = only selected dir) (default 2)
      --dir-count                         Collect directory count metrics (default true)
      --file-count                        Collect file count metrics (default true)
      --size-bucket                       Collect size bucket metrics (default true)
  -L, --follow-symlinks                   Follow symlinks for files, i.e. show the size of the file to which symlink points to (symlinks to directories are not followed)
  -h, --help                              help for disk_usage_exporter
  -i, --ignore-dirs strings               Absolute paths to ignore (separated by comma) (default [/proc,/dev,/sys,/run,/var/cache/rsnapshot])
      --log-level string                  Log level (trace, debug, info, warn, error, fatal, panic) (default "info")
  -j, --max-procs int                     Maximum number of CPU cores to use for parallel processing (default 4)
  -m, --mode string                       Expose method - either 'file' or 'http' (default "http")
      --multi-paths stringToString        Multiple paths where to analyze disk usage, in format /path1=level1,/path2=level2,... (default [])
  -f, --output-file string                Target file to store metrics in (default "./disk-usage-exporter.prom")
  -t, --scan-interval-minutes int         Scan interval in minutes for background caching (0 = disabled)
  -s, --storage-path string               Path to store cached analysis data
  -v, --version                           Print version information and exit
```

### 빠른 시작 예제

```bash
# 단일 경로 기본 사용법
disk_usage_exporter --analyzed-path /home --dir-level 2

# 여러 경로를 서로 다른 레벨로 설정
disk_usage_exporter --multi-paths=/home=2,/var=3

# SQLite 데이터베이스를 사용한 고성능 설정
disk_usage_exporter --analyzed-path /home --db-path /tmp/disk-usage.db --batch-size 1000

# 설정 파일 사용
disk_usage_exporter --config config-sqlite.yml

# 환경변수 설정
LOG_LEVEL=debug disk_usage_exporter --config config-sqlite.yml
```

`--analyzed-path`와 `--dir-level` 플래그를 사용하여 단일 경로를 지정하거나 `--multi-paths` 플래그를 사용하여 여러 경로를 설정할 수 있습니다.

## 버전 정보

다음 명령을 사용하여 버전 정보를 확인할 수 있습니다:

```bash
# CLI version check
disk_usage_exporter --version
disk_usage_exporter -v
disk_usage_exporter version

# API version endpoint
curl http://localhost:9995/version
```

## 사용 가능한 엔드포인트

HTTP 모드로 실행할 때 다음 엔드포인트를 사용할 수 있습니다:

| 엔드포인트 | 설명 | 응답 형식 |
|----------|-------------|----------------|
| **/** | 모든 사용 가능한 엔드포인트 링크가 있는 인덱스 페이지 | HTML |
| **/metrics** | Prometheus 메트릭 (메인 엔드포인트) | Text/Plain |
| **/version** | 버전 정보 | JSON |
| **/health** | 헬스 체크 엔드포인트 | JSON |

### 헬스 체크 엔드포인트

`/health` 엔드포인트는 모니터링 시스템을 위한 간단한 헬스 체크를 제공합니다:

```bash
curl http://localhost:9995/health
```

응답:
```json
{
  "status": "ok",
  "version": "v0.6.0-6-gc1ec294"
}
```

### 버전 엔드포인트

`/version` 엔드포인트는 상세한 버전 정보를 제공합니다:

```bash
curl http://localhost:9995/version
```

응답:
```json
{
  "buildDate": "Wed Jul 16 17:26:24 KST 2025",
  "commitSha": "c1ec29420833cbaf67eb925ec950de3a692859b8",
  "goVersion": "go1.22.2",
  "goarch": "amd64",
  "goos": "linux",
  "version": "v0.6.0-6-gc1ec294"
}
```

## 📊 메트릭 출력

이 익스포터는 개별 파일 메트릭 대신 **메모리 효율적인 집계 메트릭**을 제공하여 대용량 파일시스템에서 메모리 사용량을 극적으로 줄입니다.

### 집계 메트릭 구조

```prometheus
# 디렉토리별 총 디스크 사용량
# HELP node_disk_usage Total disk usage by directory
# TYPE node_disk_usage gauge
node_disk_usage{level="0",path="/home"} 8.7081373696e+10
node_disk_usage{level="1",path="/home/user1"} 2.5e+10
node_disk_usage{level="1",path="/home/user2"} 1.2e+10

# 파일 및 디렉토리 수
# HELP node_disk_usage_file_count Number of files in directory
# TYPE node_disk_usage_file_count gauge
node_disk_usage_file_count{level="1",path="/home/user1"} 50000
node_disk_usage_file_count{level="1",path="/home/user2"} 25000

# HELP node_disk_usage_directory_count Number of subdirectories in directory
# TYPE node_disk_usage_directory_count gauge
node_disk_usage_directory_count{level="1",path="/home/user1"} 150
node_disk_usage_directory_count{level="1",path="/home/user2"} 75

# 크기별 버킷 분포
# HELP node_disk_usage_size_bucket File count by size range
# TYPE node_disk_usage_size_bucket gauge
node_disk_usage_size_bucket{level="1",path="/home/user1",size_range="0-1KB"} 15000
node_disk_usage_size_bucket{level="1",path="/home/user1",size_range="1KB-1MB"} 30000
node_disk_usage_size_bucket{level="1",path="/home/user1",size_range="1MB-100MB"} 4500
node_disk_usage_size_bucket{level="1",path="/home/user1",size_range="100MB+"} 500

# 상위 N개 최대 파일 (설정 가능, 기본값: 상위 1000개)
# HELP node_disk_usage_top_files Top N largest files
# TYPE node_disk_usage_top_files gauge
node_disk_usage_top_files{path="/home/user1/large_file1.dat",rank="1"} 5.0e+09
node_disk_usage_top_files{path="/home/user1/large_file2.zip",rank="2"} 3.5e+09
node_disk_usage_top_files{path="/home/user1/backup.tar.gz",rank="3"} 2.8e+09

# 상위 N에 포함되지 않은 파일의 집계 통계
# HELP node_disk_usage_others_total Total size of files not in top N
# TYPE node_disk_usage_others_total gauge
node_disk_usage_others_total{path="/home/user1"} 1.5e+10

# HELP node_disk_usage_others_count Count of files not in top N
# TYPE node_disk_usage_others_count gauge
node_disk_usage_others_count{path="/home/user1"} 49000
```

### 메모리 효율성 이점

| 파일시스템 크기 | 개별 파일 메트릭 | 집계 메트릭 | 메모리 절약 |
|----------------|------------------------|-------------------|----------------|
| **10K 파일** | 10,000 메트릭 | ~100 메트릭 | **99%** |
| **100K 파일** | 100,000 메트릭 | ~500 메트릭 | **99.5%** |
| **1M+ 파일** | 1,000,000+ 메트릭 | ~2,000 메트릭 | **99.8%** |

### 크기 범위 버킷

파일은 자동으로 크기 범위로 분류됩니다:
- **0B**: 빈 파일
- **0-1KB**: 소형 파일 (설정 파일, 소형 스크립트)
- **1KB-1MB**: 중형 파일 (문서, 코드 파일)
- **1MB-100MB**: 대형 파일 (이미지, 소형 데이터베이스)
- **100MB+**: 초대형 파일 (비디오, 대형 데이터베이스, 아카이브)

## 📈 Prometheus 쿼리 예제

### 디렉토리 사용량 쿼리

`/home` 디렉토리와 모든 하위 디렉토리의 총 디스크 사용량:
```promql
sum(node_disk_usage{path=~"/home.*"})
```

`/var` 디렉토리 트리의 총 파일 수:
```promql
sum(node_disk_usage_file_count{path=~"/var.*"})
```

모든 레벨-1 디렉토리의 평균 디렉토리 크기:
```promql
avg(node_disk_usage{level="1"})
```

### 파일 분포 분석

`/home/user1`의 크기별 파일 분포:
```promql
node_disk_usage_size_bucket{path="/home/user1"}
```

디렉토리에서 대형 파일(>100MB)의 비율:
```promql
(
  node_disk_usage_size_bucket{path="/home/user1",size_range="100MB+"}
  /
  sum(node_disk_usage_size_bucket{path="/home/user1"})
) * 100
```

### 상위 파일 분석

모든 경로에서 가장 큰 10개 파일 표시:
```promql
topk(10, node_disk_usage_top_files)
```

상위 N개 파일과 기타 파일이 사용하는 총 공간:
```promql
sum(node_disk_usage_top_files) / (sum(node_disk_usage_top_files) + sum(node_disk_usage_others_total))
```

### 디렉토리 사용량 분석

모니터링되는 모든 경로의 총 디스크 사용량:
```promql
sum(node_disk_usage)
```

시간에 따른 디스크 사용량 증가율:
```promql
rate(node_disk_usage[5m])
```

### 증가량 및 알림 쿼리

100K개 이상의 파일을 가진 디렉토리 (성능 문제 가능):
```promql
node_disk_usage_file_count > 100000
```

정리가 필요할 수 있는 매우 큰 디렉토리 (>1TB):
```promql
node_disk_usage > 1e12
```

비정상적으로 높은 파일 밀도를 가진 디렉토리:
```promql
(node_disk_usage_file_count / node_disk_usage_directory_count) > 1000
```



### SQLite 기반 고성능 설정 (config-sqlite.yml)
```yaml
# SQLite 기반 디스크 사용량 Prometheus 익스포터 설정
# 이 설정은 향상된 성능과 데이터 지속성을 위해 SQLite 데이터베이스를 사용합니다
analyzed-path: "/home"
bind-address: "0.0.0.0:9995"
dir-level: 2
mode: "http"
follow-symlinks: false

# 로그 레벨 설정
log-level: "info"

# 병렬 처리를 위한 최대 CPU 코어 수
max-procs: 8

# 선택적 메트릭 수집 설정
dir-count: true     # 디렉토리 수 메트릭 수집
file-count: true    # 파일 수 메트릭 수집
size-bucket: true   # 크기 버킷 메트릭 수집

# SQLite 데이터베이스 설정
db-path: "/tmp/disk-usage.db"
batch-size: 1000

ignore-dirs:
  - /proc
  - /dev
  - /sys
  - /run
  - /var/cache/rsnapshot
```


### 설정 파일 사용법
```bash
# SQLite 기반 고성능 설정 사용
./disk_usage_exporter --config config-sqlite.yml

# CLI 플래그로 설정 파일 설정 재정의
./disk_usage_exporter --config config-sqlite.yml --log-level debug

# 성능 트닝을 위한 사용자 정의 CPU 코어 수 사용
./disk_usage_exporter --config config-sqlite.yml --max-procs 8
```

## Prometheus 스크래이핑 설정

디스크 사용량 분석은 리소스를 많이 사용할 수 있습니다.
분석 경로의 크기에 따라 `scrape_interval`과 `scrape_timeout`을 설정하세요.

```yaml
scrape_configs:
  - job_name: 'disk-usage'
    scrape_interval: 5m
    scrape_timeout: 20s
    static_configs:
    - targets: ['localhost:9995']
```

**백그라운드 캐싱이 활성화된 경우**, `/metrics` 응답이 캐시에서 제공되므로 더 짧은 스크래이핑 간격을 사용할 수 있습니다:

```yaml
scrape_configs:
  - job_name: 'disk-usage'
    scrape_interval: 30s
    scrape_timeout: 10s
    static_configs:
    - targets: ['localhost:9995']
```
