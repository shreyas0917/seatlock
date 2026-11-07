#!/usr/bin/env bash
# loadtest/env.sh — prints the environment a benchmark ran in ("key: value"
# lines). Numbers without their environment are meaningless, so run.sh saves
# this next to every run. Works on Windows (Git Bash) and Linux (CI).
cd "$(dirname "$0")/.." || exit 1

ps() { powershell.exe -NoProfile -Command "$1" 2>/dev/null | tr -d '\r'; }
envof() { docker compose exec -T "$1" printenv "$2" 2>/dev/null | tr -d '\r'; }

echo "date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "git_commit: $(git rev-parse --short HEAD)$(git diff --quiet || echo ' (dirty)')"
if command -v powershell.exe > /dev/null 2>&1; then
  echo "cpu: $(ps '(Get-CimInstance Win32_Processor | Select-Object -First 1).Name' | sed 's/ *$//')"
  echo "cpu_cores_physical: $(ps '(Get-CimInstance Win32_Processor | Measure-Object NumberOfCores -Sum).Sum')"
  echo "cpu_threads: $(ps '(Get-CimInstance Win32_Processor | Measure-Object NumberOfLogicalProcessors -Sum).Sum')"
  echo "ram_host_gib: $(ps '[math]::Round((Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory/1GB,1)')"
  echo "os: $(ps '$o=Get-CimInstance Win32_OperatingSystem; "$($o.Caption) $($o.Version)"')"
else
  echo "cpu: $(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2 | sed 's/^ *//')"
  echo "cpu_threads: $(nproc)"
  echo "ram_host_gib: $(awk '/MemTotal/ {printf "%.1f", $2/1048576}' /proc/meminfo)"
  echo "os: $(. /etc/os-release && echo "$PRETTY_NAME") $(uname -r)"
fi
echo "docker: $(docker info --format '{{.OperatingSystem}}, server {{.ServerVersion}}, {{.NCPU}} CPUs' 2>/dev/null), $(docker info --format '{{.MemTotal}}' | awk '{printf "%.2f GiB memory", $1/1073741824}')"
echo "go_host: $(go version 2>/dev/null | awk '{print $3}')"
echo "go_build_image: $(grep -m1 -oE 'FROM golang:[^ ]+' Dockerfile | cut -d: -f2)"
echo "postgres: $(docker compose exec -T postgres psql -U "${POSTGRES_USER:-seatlock}" -d "${POSTGRES_DB:-seatlock}" -tAc 'SHOW server_version' 2>/dev/null | tr -d '\r')"
echo "redis: $(docker compose exec -T redis redis-server --version 2>/dev/null | grep -oE 'v=[0-9.]+' | cut -d= -f2)"
echo "k6: $(grep -m1 -oE 'grafana/k6:[0-9.]+' loadtest/run.sh | cut -d: -f2)"
echo "api_replicas: $(docker compose ps --status running --format '{{.Service}}' 2>/dev/null | grep -cE '^api(-[0-9]+)?$')"
echo "db_max_conns: $(envof api DB_MAX_CONNS)  db_min_conns: $(envof api DB_MIN_CONNS)"
echo "redis_pool_size: $(envof api REDIS_POOL_SIZE)  redis_timeouts: $(envof api REDIS_READ_TIMEOUT)"
echo "hold_ttl: $(envof api HOLD_TTL)  payment_timeout: $(envof api PAYMENT_TIMEOUT)"
echo "gateway_delay: $(envof payment PAYMENT_MIN_DELAY)-$(envof payment PAYMENT_MAX_DELAY)"
echo "pprof_block_rate: $(envof api PPROF_BLOCK_RATE)  pprof_mutex_fraction: $(envof api PPROF_MUTEX_FRACTION)"
echo "postgres_max_connections: $(docker compose exec -T postgres psql -U "${POSTGRES_USER:-seatlock}" -d "${POSTGRES_DB:-seatlock}" -tAc 'SHOW max_connections' 2>/dev/null | tr -d '\r')"
echo "containers_on_host: $(docker ps -q | wc -l | tr -d ' ') running (k6, Prometheus and Grafana share the CPUs with the system under test)"
