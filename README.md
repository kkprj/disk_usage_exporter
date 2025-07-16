# Disk Usage Prometheus Exporter

[![Build Status](https://travis-ci.com/dundee/disk_usage_exporter.svg?branch=master)](https://travis-ci.com/dundee/disk_usage_exporter)
[![codecov](https://codecov.io/gh/dundee/disk_usage_exporter/branch/master/graph/badge.svg)](https://codecov.io/gh/dundee/disk_usage_exporter)
[![Go Report Card](https://goreportcard.com/badge/github.com/dundee/disk_usage_exporter)](https://goreportcard.com/report/github.com/dundee/disk_usage_exporter)
[![Maintainability](https://api.codeclimate.com/v1/badges/74d685f0c638e6109ab3/maintainability)](https://codeclimate.com/github/dundee/disk_usage_exporter/maintainability)
[![CodeScene Code Health](https://codescene.io/projects/14689/status-badges/code-health)](https://codescene.io/projects/14689)

Provides detailed info about disk usage of the selected filesystem path.

Uses [gdu](https://github.com/dundee/gdu) under the hood for the disk usage analysis.

## Demo Grafana dashboard

https://grafana.milde.cz/d/0TfJhs_Mz/disk-usage (credentials: grafana / gdu)

## Usage

```
Usage:
  disk_usage_exporter [flags]

Flags:
  -p, --analyzed-path string              Path where to analyze disk usage (default "/")
      --basic-auth-users stringToString   Basic Auth users and their passwords as bcypt hashes (default [])
  -b, --bind-address string               Address to bind to (default "0.0.0.0:9995")
  -c, --config string                     config file (default is $HOME/.disk_usage_exporter.yaml)
  -l, --dir-level int                     Directory nesting level to show (0 = only selected dir) (default 2)
  -L, --follow-symlinks                   Follow symlinks for files, i.e. show the size of the file to which symlink points to (symlinks to directories are not followed)
  -h, --help                              help for disk_usage_exporter
  -i, --ignore-dirs strings               Absolute paths to ignore (separated by comma) (default [/proc,/dev,/sys,/run,/var/cache/rsnapshot])
      --log-level string                  Log level (trace, debug, info, warn, error, fatal, panic) (default "info")
  -m, --mode string                       Expose method - either 'file' or 'http' (default "http")
      --multi-paths stringToString        Multiple paths where to analyze disk usage, in format /path1=level1,/path2=level2,... (default [])
  -f, --output-file string                Target file to store metrics in (default "./disk-usage-exporter.prom")
  -t, --scan-interval-minutes int         Scan interval in minutes for background caching (0 = disabled)
  -s, --storage-path string               Path to store cached analysis data
  -v, --version                           Print version information and exit
```

### Quick Start Examples

```bash
# Basic usage with single path
disk_usage_exporter --analyzed-path /home --dir-level 2

# Multiple paths with different levels
disk_usage_exporter --multi-paths=/home=2,/var=3

# Using configuration file
disk_usage_exporter --config config-basic.yml

# Background caching for better performance
disk_usage_exporter --storage-path /tmp/cache --scan-interval-minutes 30

# File output mode
disk_usage_exporter --mode file --output-file metrics.prom

# Debug mode with detailed logging
disk_usage_exporter --log-level debug --analyzed-path /tmp

# Environment variable configuration
LOG_LEVEL=debug disk_usage_exporter --config config-basic.yml
```

Either one path can be specified using `--analyzed-path` and `--dir-level` flags or multiple can be set
using `--multi-paths` flag.

## Version Information

You can check the version information using:

```bash
# CLI version check
disk_usage_exporter --version
disk_usage_exporter -v
disk_usage_exporter version

# API version endpoint
curl http://localhost:9995/version
```

## Available Endpoints

When running in HTTP mode, the following endpoints are available:

| Endpoint | Description | Response Format |
|----------|-------------|----------------|
| **/** | Index page with links to all available endpoints | HTML |
| **/metrics** | Prometheus metrics (main endpoint) | Text/Plain |
| **/version** | Version information | JSON |
| **/health** | Health check endpoint | JSON |

### Health Check Endpoint

The `/health` endpoint provides a simple health check for monitoring systems:

```bash
curl http://localhost:9995/health
```

Response:
```json
{
  "status": "ok",
  "version": "v0.6.0-6-gc1ec294"
}
```

### Version Endpoint

The `/version` endpoint provides detailed version information:

```bash
curl http://localhost:9995/version
```

Response:
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

## Example output

```
# HELP node_disk_usage_bytes Disk usage of the directory/file
# TYPE node_disk_usage_bytes gauge
node_disk_usage_bytes{path="/var/cache"} 2.1766144e+09
node_disk_usage_bytes{path="/var/db"} 20480
node_disk_usage_bytes{path="/var/dpkg"} 8192
node_disk_usage_bytes{path="/var/empty"} 4096
node_disk_usage_bytes{path="/var/games"} 4096
node_disk_usage_bytes{path="/var/lib"} 7.554709504e+09
node_disk_usage_bytes{path="/var/local"} 4096
node_disk_usage_bytes{path="/var/lock"} 0
node_disk_usage_bytes{path="/var/log"} 4.247068672e+09
node_disk_usage_bytes{path="/var/mail"} 0
node_disk_usage_bytes{path="/var/named"} 4096
node_disk_usage_bytes{path="/var/opt"} 4096
node_disk_usage_bytes{path="/var/run"} 0
node_disk_usage_bytes{path="/var/snap"} 1.11694848e+10
node_disk_usage_bytes{path="/var/spool"} 16384
node_disk_usage_bytes{path="/var/tmp"} 475136
# HELP node_disk_usage_level_1_bytes Disk usage of the directory/file level 1
# TYPE node_disk_usage_level_1_bytes gauge
node_disk_usage_level_1_bytes{path="/bin"} 0
node_disk_usage_level_1_bytes{path="/boot"} 1.29736704e+08
node_disk_usage_level_1_bytes{path="/etc"} 1.3090816e+07
node_disk_usage_level_1_bytes{path="/home"} 8.7081373696e+10
node_disk_usage_level_1_bytes{path="/lib"} 0
node_disk_usage_level_1_bytes{path="/lib64"} 0
node_disk_usage_level_1_bytes{path="/lost+found"} 4096
node_disk_usage_level_1_bytes{path="/mnt"} 4096
node_disk_usage_level_1_bytes{path="/opt"} 2.979229696e+09
node_disk_usage_level_1_bytes{path="/root"} 4096
node_disk_usage_level_1_bytes{path="/sbin"} 0
node_disk_usage_level_1_bytes{path="/snap"} 0
node_disk_usage_level_1_bytes{path="/srv"} 4.988928e+06
node_disk_usage_level_1_bytes{path="/tmp"} 1.3713408e+07
node_disk_usage_level_1_bytes{path="/usr"} 1.8109427712e+10
node_disk_usage_level_1_bytes{path="/var"} 2.5156793856e+10
```

## Example Prometheus queries

Disk usage of `/var` directory:

```
sum(node_disk_usage_bytes{path=~"/var.*"})
```

## Example config files

Several example configuration files are provided in the repository:

### Basic Configuration (config-basic.yml)
```yaml
# Basic configuration without caching
analyzed-path: "/tmp"
bind-address: "0.0.0.0:9995"
dir-level: 1
mode: "http"
follow-symlinks: false

# Log level configuration
# Available levels: trace, debug, info, warn, error, fatal, panic
log-level: "info"

ignore-dirs:
  - /proc
  - /dev
  - /sys
  - /run
  - /var/cache/rsnapshot
```

### Configuration with Background Caching (config-caching.yml)
```yaml
# Configuration with background caching for better performance
analyzed-path: "/home"
bind-address: "0.0.0.0:9995"
dir-level: 2
mode: "http"
follow-symlinks: false

# Log level configuration
# Available levels: trace, debug, info, warn, error, fatal, panic
log-level: "info"

# Background caching configuration
storage-path: "/tmp/disk-usage-cache"
scan-interval-minutes: 15

ignore-dirs:
  - /proc
  - /dev
  - /sys
  - /run
  - /var/cache/rsnapshot
  - /tmp
```

### Multiple Paths Configuration (config-multipaths.yml)
```yaml
# Monitor multiple directories with different depth levels
bind-address: "0.0.0.0:9995"
mode: "http"
follow-symlinks: false

# Log level configuration
# Available levels: trace, debug, info, warn, error, fatal, panic
log-level: "info"

multi-paths:
  /home: 2
  /var: 3
  /tmp: 1
  /opt: 2

# Background caching configuration
storage-path: "/tmp/disk-usage-cache"
scan-interval-minutes: 20

ignore-dirs:
  - /proc
  - /dev
  - /sys
  - /run
  - /var/cache/rsnapshot

# Basic authentication (optional)
# basic-auth-users:
#   admin: $2b$12$hNf2lSsxfm0.i4a.1kVpSOVyBCfIB51VRjgBUyv6kdnyTlgWj81Ay
```

### File Output Mode Configuration
```yaml
# Output metrics to file instead of HTTP server
analyzed-path: /
mode: file
output-file: ./disk-usage-exporter.prom
dir-level: 2

# Log level configuration
# Available levels: trace, debug, info, warn, error, fatal, panic
log-level: "info"

ignore-dirs:
- /proc
- /dev
- /sys
- /run
```

### Debug Configuration (config-debug.yml)
```yaml
# Debug configuration for troubleshooting
analyzed-path: "/tmp"
bind-address: "0.0.0.0:9995"
dir-level: 1
mode: "http"
follow-symlinks: false

# Log level configuration - Debug mode for troubleshooting
# Available levels: trace, debug, info, warn, error, fatal, panic
log-level: "debug"

# Background caching configuration
storage-path: "/tmp/debug-cache"
scan-interval-minutes: 5

ignore-dirs:
  - /proc
  - /dev
  - /sys
  - /run
  - /var/cache/rsnapshot
```

### Usage with Config Files
```bash
# Use specific config file
./disk_usage_exporter --config config-basic.yml

# Use config with background caching
./disk_usage_exporter --config config-caching.yml

# Use config with multiple paths
./disk_usage_exporter --config config-multipaths.yml

# Use debug configuration for troubleshooting
./disk_usage_exporter --config config-debug.yml

# Override config file settings with CLI flags
./disk_usage_exporter --config config-basic.yml --log-level debug
```

## Prometheus scrape config

Disk usage analysis can be resource heavy.
Set the `scrape_interval` and `scrape_timeout` according to the size of analyzed path.

```yaml
scrape_configs:
  - job_name: 'disk-usage'
    scrape_interval: 5m
    scrape_timeout: 20s
    static_configs:
    - targets: ['localhost:9995']
```

**With background caching enabled**, you can use shorter scrape intervals since `/metrics` responses are served from cache:

```yaml
scrape_configs:
  - job_name: 'disk-usage'
    scrape_interval: 30s
    scrape_timeout: 10s
    static_configs:
    - targets: ['localhost:9995']
```

## Dump to file

The official `node-exporter` allows to specify a folder which contains additional metric files through a [textfile collection mechanism](https://github.com/prometheus/node_exporter#textfile-collector).
In order to make use of this, one has to set up `node-exporter` according to the documentation and set the `output-file`
of this exporter to any name ending in `.prom` within said folder (and of course also `mode` to `file`).

A common use case for this is when the calculation of metrics takes particularly long and therefore can only be done
once in a while. To automate the periodic update of the output file, simply set up a cronjob.

## Basic Auth

You can enable HTTP Basic authorization by setting the `--basic-auth-users` flag (password is "test"):

```
disk_usage_exporter --basic-auth-users='admin=$2b$12$hNf2lSsxfm0.i4a.1kVpSOVyBCfIB51VRjgBUyv6kdnyTlgWj81Ay'
```

or by setting the key in config:

```yaml
basic-auth-users:
  admin: $2b$12$hNf2lSsxfm0.i4a.1kVpSOVyBCfIB51VRjgBUyv6kdnyTlgWj81Ay
```

The password needs to be hashed by [bcrypt](https://bcrypt-generator.com/) in both cases.

## Background Caching

For better performance on large filesystems, you can enable background caching by setting both `storage-path` and `scan-interval-minutes`:

```yaml
analyzed-path: /mnt
bind-address: 0.0.0.0:9995
dir-level: 1
mode: http
follow-symlinks: false
storage-path: /var/cache/gdu
scan-interval-minutes: 30
```

### How Background Caching Works

1. **Background Process**: A separate goroutine runs periodic scans
2. **Data Storage**: Results are cached using BadgerDB key-value store
3. **Fast Response**: `/metrics` endpoint serves cached data instantly
4. **Auto-cleanup**: Storage handles cleanup of old data automatically

### Cache Behavior

| Mode | Background Scan | Response Time | Data Freshness |
|------|----------------|---------------|----------------|
| **With caching enabled** | ✅ Runs every N minutes | ⚡ Fast (cached) | Updated every N minutes |
| **Without caching** | ❌ None | 🐌 Slow (live analysis) | Real-time |

### Configuration Requirements

- **Both required**: `storage-path` AND `scan-interval-minutes` must be set
- **Permissions**: Storage path must be writable by the exporter process
- **Disk space**: Minimal storage required (typically < 1MB per analyzed path)

### Cache Miss Behavior

When cached data is unavailable:
- Returns empty metrics (0 bytes) for missing paths
- Logs debug message about missing cache data
- Continues serving other cached paths normally

**Note:** If either `storage-path` or `scan-interval-minutes` is not set, the exporter will perform live disk analysis for each `/metrics` request (default behavior).

## Logging Configuration

The exporter supports configurable logging levels to help with monitoring and troubleshooting:

### Available Log Levels

| Level | Description | Use Case |
|-------|-------------|----------|
| **trace** | Most detailed logging | Development/debugging |
| **debug** | Debug information including cache operations | Troubleshooting |
| **info** | General information including scan timings | Production monitoring |
| **warn** | Warning messages | Production monitoring |
| **error** | Error messages | Production monitoring |
| **fatal** | Fatal errors (program exits) | Production monitoring |
| **panic** | Panic situations | Production monitoring |

### Configuration Methods

1. **Configuration File**: Add `log-level: "debug"` to your config file
2. **CLI Flag**: Use `--log-level debug` when running the exporter
3. **Environment Variable**: Set `LOG_LEVEL=debug` environment variable

### Example Log Outputs

**Info Level (default)** - Shows scan timings:
```
time="2025-07-16T19:06:39+09:00" level=info msg="Starting live analysis for path: /tmp"
time="2025-07-16T19:06:39+09:00" level=info msg="Live analysis completed for path: /tmp, elapsed time: 17.242744ms"
```

**Debug Level** - Shows cache operations:
```
time="2025-07-16T19:06:39+09:00" level=debug msg="No cached data found for path: /tmp, returning empty metrics"
time="2025-07-16T19:06:39+09:00" level=debug msg="Failed to open storage, returning empty metrics"
```


## Example systemd unit file

```
[Unit]
Description=Prometheus disk usage exporter
Documentation=https://github.com/dundee/disk_usage_exporter

[Service]
Restart=always
User=prometheus
ExecStart=/usr/bin/disk_usage_exporter $ARGS
ExecReload=/bin/kill -HUP $MAINPID
TimeoutStopSec=20s
SendSIGKILL=no

[Install]
WantedBy=multi-user.target
```
