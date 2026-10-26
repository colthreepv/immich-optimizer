#!/usr/bin/env bash
set -Eeuo pipefail

test_dir=$(mktemp -d)
cleanup_pid=""
cleanup() {
    if [[ -n $cleanup_pid ]] && kill -0 "$cleanup_pid" 2>/dev/null; then
        kill -TERM "$cleanup_pid" 2>/dev/null || true
        wait "$cleanup_pid" 2>/dev/null || true
    fi
    rm -rf -- "$test_dir"
}
trap cleanup EXIT

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
quiet_exec=${QUIET_EXEC_BIN:-$script_dir/quiet-exec}

mkdir -p "$test_dir/sys/hwmon0"
printf 'it8613_rpm\n' > "$test_dir/sys/hwmon0/name"
printf '3500\n' > "$test_dir/sys/hwmon0/fan1_input"

cat > "$test_dir/fake-child" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
counter=$1
marker=$2
printf '%s\n' "$$" > "$marker.pid"
trap 'printf "terminated\n" > "$marker"; exit 0' TERM INT
while :; do
    printf '.' >> "$counter"
    sleep 0.05 || true
done
EOF
chmod +x "$test_dir/fake-child"

counter="$test_dir/counter"
marker="$test_dir/child-terminated"
QUIET_EXEC_SENSOR_ENABLED=1 \
QUIET_EXEC_SENSOR_ROOT="$test_dir/sys" \
QUIET_EXEC_POLL_SECONDS=0.05 \
QUIET_EXEC_RESUME_SECONDS=1 \
QUIET_EXEC_TERM_GRACE_SECONDS=1 \
    "$quiet_exec" "$test_dir/fake-child" "$counter" "$marker" &
wrapper_pid=$!
cleanup_pid=$wrapper_pid

for _ in {1..40}; do
    [[ -s $counter ]] && break
    sleep 0.05
done
[[ -s $counter ]] || { echo "fake child did not start" >&2; exit 1; }

sleep 0.4
paused_count=$(wc -c < "$counter")
sleep 0.3
[[ $(wc -c < "$counter") -eq $paused_count ]] || { echo "child did not pause above 3000 RPM" >&2; exit 1; }

printf '2000\n' > "$test_dir/sys/hwmon0/fan1_input"
sleep 1.4
resumed_count=$(wc -c < "$counter")
[[ $resumed_count -gt $paused_count ]] || { echo "child did not resume below 2600 RPM" >&2; exit 1; }

kill -TERM "$wrapper_pid"
set +e
wait "$wrapper_pid"
term_status=$?
set -e
cleanup_pid=""
[[ $term_status -eq 143 ]] || { echo "SIGTERM exit status was $term_status, expected 143" >&2; exit 1; }
[[ -f $marker ]] || { echo "fake child did not receive SIGTERM" >&2; exit 1; }

child_pid=$(cat "$marker.pid")
if kill -0 "$child_pid" 2>/dev/null; then
    echo "fake child remained alive after group termination" >&2
    exit 1
fi

mkdir -p "$test_dir/empty-sys"
set +e
QUIET_EXEC_SENSOR_ENABLED=1 \
QUIET_EXEC_SENSOR_ROOT="$test_dir/empty-sys" \
    "$quiet_exec" /bin/true >/dev/null 2>&1
missing_status=$?
set -e
[[ $missing_status -ne 0 ]] || { echo "missing sensor did not fail closed" >&2; exit 1; }

QUIET_EXEC_SENSOR_ENABLED=0 \
    "$quiet_exec" /bin/sh -c 'printf "ok\n" > "$1"' _ "$test_dir/disabled-ok"
[[ -f $test_dir/disabled-ok ]] || { echo "sensor-disabled execution failed" >&2; exit 1; }

set +e
QUIET_EXEC_SENSOR_ENABLED=0 \
QUIET_EXEC_TIMEOUT_SECONDS=0.2 \
QUIET_EXEC_POLL_SECONDS=0.05 \
    "$quiet_exec" /bin/sleep 5 >/dev/null 2>&1
timeout_status=$?
set -e
[[ $timeout_status -eq 124 ]] || { echo "explicit timeout status was $timeout_status, expected 124" >&2; exit 1; }

echo "quiet-exec tests passed"
