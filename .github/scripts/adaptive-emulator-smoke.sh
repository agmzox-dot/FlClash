#!/usr/bin/env bash
set -euo pipefail

: "${GITHUB_WORKSPACE:?GITHUB_WORKSPACE is required}"
: "${CANDIDATE_PACKAGE:?CANDIDATE_PACKAGE is required}"

apk="$(find dist -type f -name '*android-x86_64*.apk' -print -quit)"
log_file="$GITHUB_WORKSPACE/emulator.log"
error_file="$GITHUB_WORKSPACE/emulator-error.log"
: > "$log_file"
: > "$error_file"

record_error() {
  printf '%s\n' "$*" >> "$error_file"
  printf '%s\n' "$*" >&2
}

adb_retry() {
  local attempts=1
  while (( attempts <= 5 )); do
    if adb "$@" >> "$error_file" 2>&1; then
      return 0
    fi
    record_error "adb command failed (attempt $attempts/5): adb $*"
    attempts=$((attempts + 1))
    if (( attempts <= 5 )); then
      sleep 5
    fi
  done
  return 1
}

adb_retry_output() {
  local attempts=1
  local output
  while (( attempts <= 5 )); do
    if output="$(adb "$@" 2>> "$error_file")"; then
      printf '%s' "$output"
      return 0
    fi
    record_error "adb output command failed (attempt $attempts/5): adb $*"
    attempts=$((attempts + 1))
    if (( attempts <= 5 )); then
      sleep 5
    fi
  done
  return 1
}

dump_diagnostics() {
  local status=$?
  if command -v adb >/dev/null 2>&1; then
    timeout --signal=TERM 30s adb logcat -b all -d -v threadtime > "$log_file" 2>> "$error_file" || true
    {
      printf '\n--- emulator script exit: %s ---\n' "$status"
      printf '\n--- process ---\n'
      timeout --signal=TERM 30s adb shell pidof "$CANDIDATE_PACKAGE" || true
      printf '\n--- package ---\n'
      timeout --signal=TERM 30s adb shell dumpsys package "$CANDIDATE_PACKAGE" || true
      printf '\n--- activity ---\n'
      timeout --signal=TERM 30s adb shell dumpsys activity activities || true
    } >> "$error_file" 2>&1
  else
    record_error 'adb was not available while collecting diagnostics'
  fi
  return "$status"
}
trap dump_diagnostics EXIT

if [[ -z "$apk" || ! -f "$apk" ]]; then
  record_error 'x86_64 emulator APK was not produced'
  find dist -type f -name '*.apk' -print >> "$error_file" 2>&1 || true
  exit 1
fi

zip_entries="$(unzip -Z1 "$apk")"
if ! grep -q '^lib/x86_64/' <<< "$zip_entries"; then
  record_error "selected APK does not contain x86_64 native libraries: $apk"
  exit 1
fi
if grep -q '^lib/arm64-v8a/' <<< "$zip_entries"; then
  record_error "selected APK contains arm64 native libraries: $apk"
  exit 1
fi

device_state=''
for attempt in $(seq 1 90); do
  device_state="$(adb get-state 2>> "$error_file" || true)"
  if [[ "$device_state" == 'device' ]]; then
    break
  fi
  sleep 2
done
if [[ "$device_state" != 'device' ]]; then
  record_error "emulator did not become ready: $device_state"
  exit 1
fi

boot_completed=''
for attempt in $(seq 1 90); do
  boot_completed="$(adb shell getprop sys.boot_completed 2>> "$error_file" | tr -d '\r' || true)"
  if [[ "$boot_completed" == '1' ]]; then
    break
  fi
  sleep 2
done
if [[ "$boot_completed" != '1' ]]; then
  record_error "emulator boot did not complete: $boot_completed"
  exit 1
fi

if ! adb_retry install -r "$apk"; then
  record_error "failed to install x86_64 APK: $apk"
  exit 1
fi
if ! adb_retry logcat -c; then
  record_error 'failed to clear logcat before launch'
  exit 1
fi
if ! adb_retry shell monkey -p "$CANDIDATE_PACKAGE" -c android.intent.category.LAUNCHER 1; then
  record_error "failed to launch $CANDIDATE_PACKAGE"
  exit 1
fi

pid=''
for attempt in $(seq 1 60); do
  pid="$(adb_retry_output shell pidof "$CANDIDATE_PACKAGE" | tr -d '\r' | awk '{print $1}' || true)"
  if [[ "$pid" =~ [0-9] ]]; then
    break
  fi
  sleep 2
done
if [[ ! "$pid" =~ [0-9] ]]; then
  record_error "application process did not start: $CANDIDATE_PACKAGE"
  exit 1
fi

sleep 20
final_pid="$(adb_retry_output shell pidof "$CANDIDATE_PACKAGE" | tr -d '\r' | awk '{print $1}' || true)"
if [[ ! "$final_pid" =~ [0-9] ]]; then
  record_error "application process exited after launch: $CANDIDATE_PACKAGE"
  exit 1
fi

if ! adb logcat -b all --pid="$final_pid" -d -v threadtime > "$log_file" 2>> "$error_file"; then
  record_error 'pid-filtered logcat failed; capturing all logcat buffers'
  timeout --signal=TERM 30s adb logcat -b all -d -v threadtime > "$log_file" 2>> "$error_file" || true
fi
if grep -Eq 'FATAL EXCEPTION|Fatal signal|SIGSEGV|UnsatisfiedLinkError|ANR in' "$log_file"; then
  grep -E -C 3 'FATAL EXCEPTION|Fatal signal|SIGSEGV|UnsatisfiedLinkError|ANR in' "$log_file" || true
  record_error "application reported a fatal startup condition: $CANDIDATE_PACKAGE"
  exit 1
fi
