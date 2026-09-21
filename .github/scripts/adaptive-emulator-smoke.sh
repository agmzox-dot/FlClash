#!/usr/bin/env bash
set -euo pipefail

: "${GITHUB_WORKSPACE:?GITHUB_WORKSPACE is required}"
: "${CANDIDATE_PACKAGE:?CANDIDATE_PACKAGE is required}"

apk="$(find dist -type f -name '*android-x86_64*.apk' -print -quit)"
arm64_apk="$GITHUB_WORKSPACE/adaptive-artifact/FlClash-Adaptive-Candidate-0.8.98-a1-arm64.apk"
log_file="$GITHUB_WORKSPACE/emulator.log"
error_file="$GITHUB_WORKSPACE/emulator-error.log"
ui_file="$GITHUB_WORKSPACE/emulator-ui.xml"
service_file="$GITHUB_WORKSPACE/emulator-services.txt"
signature_file="$GITHUB_WORKSPACE/emulator-signature.txt"
acceptance_file="$GITHUB_WORKSPACE/emulator-acceptance.txt"
fixture_config="$GITHUB_WORKSPACE/.github/fixtures/adaptive-emulator-config.yaml"
fixture_preferences="$GITHUB_WORKSPACE/.github/fixtures/adaptive-emulator-shared-preferences.xml"
adb_timeout_seconds=12
apk_install_timeout_seconds=600

: > "$log_file"
: > "$error_file"
: > "$ui_file"
: > "$service_file"
: > "$signature_file"
: > "$acceptance_file"

record_error() {
  printf '%s\n' "$*" >> "$error_file"
  printf 'FAIL: %s\n' "$*" | tee -a "$acceptance_file" >&2
  printf '%s\n' "$*" >&2
}

record_pass() {
  printf 'PASS: %s\n' "$*" | tee -a "$acceptance_file"
}

record_phase() {
  printf 'PHASE: %s\n' "$*" | tee -a "$acceptance_file"
}

record_unverified() {
  printf 'UNVERIFIED: %s\n' "$*" | tee -a "$acceptance_file"
}

bounded_timeout() {
  timeout --foreground --signal=TERM --kill-after=5s "$@"
}

adb_retry() {
  local attempts=1
  while (( attempts <= 3 )); do
    if bounded_timeout "${adb_timeout_seconds}s" adb "$@" >> "$error_file" 2>&1; then
      return 0
    fi
    record_error "adb command failed (attempt $attempts/3): adb $*"
    attempts=$((attempts + 1))
    if (( attempts <= 3 )); then
      sleep 2
    fi
  done
  return 1
}

adb_install_candidate() {
  local attempts=1
  local install_pid
  local output_file
  local detail
  local status
  local started_at
  local elapsed
  while (( attempts <= 2 )); do
    output_file="$GITHUB_WORKSPACE/adb-install-attempt-${attempts}.log"
    : > "$output_file"
    printf 'starting adb install attempt %s (timeout %ss)\n' "$attempts" "$apk_install_timeout_seconds" >> "$error_file"
    setsid adb install -r --abi x86_64 "$apk" > "$output_file" 2>&1 &
    install_pid=$!
    started_at=$SECONDS
    status=''
    while kill -0 "$install_pid" 2>/dev/null; do
      elapsed=$((SECONDS - started_at))
      if (( elapsed >= apk_install_timeout_seconds )); then
        status=124
        printf 'adb install attempt %s exceeded its %ss timeout; terminating its process group\n' "$attempts" "$apk_install_timeout_seconds" >> "$error_file"
        kill -TERM -- "-$install_pid" 2>/dev/null || kill -TERM "$install_pid" 2>/dev/null || true
        sleep 5
        kill -KILL -- "-$install_pid" 2>/dev/null || kill -KILL "$install_pid" 2>/dev/null || true
        wait "$install_pid" 2>/dev/null || true
        break
      fi
      sleep 2
    done
    if [[ -z "$status" ]]; then
      if wait "$install_pid"; then
        printf 'adb install attempt %s succeeded:\n' "$attempts" >> "$error_file"
        cat "$output_file" >> "$error_file"
        return 0
      else
        status=$?
      fi
    fi
    printf 'adb install attempt %s failed (exit %s):\n' "$attempts" "$status" >> "$error_file"
    cat "$output_file" >> "$error_file"
    detail="$(tr '\r\n' ' ' < "$output_file" | tr -s ' ')"
    if (( ${#detail} > 240 )); then
      detail="${detail: -240}"
    fi
    if (( status == 124 || status == 137 )); then
      record_error "APK adb install attempt $attempts/2 timed out after ${apk_install_timeout_seconds} seconds"
    elif [[ -n "$detail" ]]; then
      record_error "APK adb install attempt $attempts/2 failed: $detail"
    else
      record_error "APK adb install attempt $attempts/2 failed with exit $status"
    fi
    attempts=$((attempts + 1))
    if (( attempts <= 2 )); then
      sleep 3
    fi
  done
  return 1
}

adb_once() {
  bounded_timeout "${adb_timeout_seconds}s" adb "$@" >> "$error_file" 2>&1
}

adb_retry_output() {
  local attempts=1
  local output
  while (( attempts <= 3 )); do
    if output="$(bounded_timeout "${adb_timeout_seconds}s" adb "$@" 2>> "$error_file")"; then
      printf '%s' "$output"
      return 0
    fi
    record_error "adb output command failed (attempt $attempts/3): adb $*"
    attempts=$((attempts + 1))
    if (( attempts <= 3 )); then
      sleep 2
    fi
  done
  return 1
}

adb_output_once() {
  bounded_timeout "${adb_timeout_seconds}s" adb "$@" 2>> "$error_file"
}

capture_log() {
  bounded_timeout 15s adb logcat -b all -d -v threadtime > "$log_file" 2>> "$error_file" || true
}

dump_ui() {
  bounded_timeout "${adb_timeout_seconds}s" adb shell uiautomator dump /sdcard/flclash-window.xml >> "$error_file" 2>&1 || return 1
  adb_output_once shell cat /sdcard/flclash-window.xml > "$ui_file"
}

dump_services() {
  adb_output_once shell dumpsys activity services "$CANDIDATE_PACKAGE" > "$service_file" || true
}

tap_text() {
  local text="$1"
  local line
  local bounds
  line="$(grep -F "text=\"$text\"" "$ui_file" | head -n 1 || true)"
  bounds="$(printf '%s' "$line" | sed -n 's/.*bounds="\[\([0-9][0-9]*\),\([0-9][0-9]*\)\]\[\([0-9][0-9]*\),\([0-9][0-9]*\)\]".*/\1 \2 \3 \4/p')"
  if [[ -z "$bounds" ]]; then
    return 1
  fi
  read -r left top right bottom <<< "$bounds"
  adb_once shell input tap "$(( (left + right) / 2 ))" "$(( (top + bottom) / 2 ))"
}

handle_first_run_dialogs() {
  local dismissed=0
  if tap_text 'Agree' || tap_text '同意'; then
    record_pass 'first-run disclaimer was dismissed by the emulator script'
    dismissed=1
  fi
  if tap_text 'Confirm' || tap_text '确定'; then
    record_pass 'first-run data-collection dialog was dismissed by the emulator script'
    dismissed=1
  fi
  if [[ "$dismissed" == 1 ]]; then
    return 0
  fi
  return 1
}

find_sdk_tool() {
  local tool="$1"
  local sdk_root="${ANDROID_HOME:-${ANDROID_SDK_ROOT:-}}"
  if command -v "$tool" >/dev/null 2>&1; then
    command -v "$tool"
    return 0
  fi
  if [[ -z "$sdk_root" || ! -d "$sdk_root" ]]; then
    return 1
  fi
  find "$sdk_root/build-tools" -type f -name "$tool" -print 2>/dev/null | sort -V | tail -n 1
}

verify_apk_identity_and_signatures() {
  local apksigner="$(find_sdk_tool apksigner || true)"
  local aapt="$(find_sdk_tool aapt || true)"
  local item label package_name cert_output digest
  local x86_digest='' arm64_digest=''
  if [[ -z "$apksigner" || -z "$aapt" ]]; then
    record_error 'Android build-tools apksigner/aapt were not found'
    return 1
  fi
  for item in "$apk" "$arm64_apk"; do
    [[ -f "$item" ]] || { record_error "APK for signature verification was not found: $item"; return 1; }
    label='x86_64'
    [[ "$item" == "$arm64_apk" ]] && label='arm64'
    package_name="$("$aapt" dump badging "$item" 2>> "$error_file" | sed -n "s/^package: name='\([^']*\)'.*/\1/p" | head -n 1)"
    if [[ "$package_name" != "$CANDIDATE_PACKAGE" ]]; then
      record_error "$label APK package is '$package_name', expected '$CANDIDATE_PACKAGE'"
      return 1
    fi
    cert_output="$("$apksigner" verify --verbose --print-certs "$item" 2>&1)" || {
      printf '%s\n' "$cert_output" >> "$signature_file"
      record_error "apksigner rejected the $label APK: $item"
      return 1
    }
    digest="$(printf '%s\n' "$cert_output" | sed -n 's/.*SHA-256 digest: //p' | head -n 1 | tr -d ' ' | tr '[:lower:]' '[:upper:]')"
    [[ -n "$digest" ]] || { printf '%s\n' "$cert_output" >> "$signature_file"; record_error "could not read the $label APK certificate digest"; return 1; }
    {
      printf '%s APK: %s\n' "$label" "$item"
      printf 'package: %s\ncertificate_sha256: %s\n' "$package_name" "$digest"
      printf '%s\n' "$cert_output"
    } >> "$signature_file"
    if [[ "$label" == 'x86_64' ]]; then x86_digest="$digest"; else arm64_digest="$digest"; fi
  done
  if [[ "$x86_digest" != "$arm64_digest" ]]; then
    record_error 'x86_64 and arm64 Candidate APKs do not use the same signing certificate'
    return 1
  fi
  record_pass "x86_64 and arm64 Candidate APKs have package $CANDIDATE_PACKAGE and the same certificate"
  printf 'upgrade_check: adb install -r is performed twice on the same certificate in this run\n' >> "$signature_file"
  printf 'known_previous_candidate_certificate: unavailable in CI; overwrite of an unknown old install is not proven\n' >> "$signature_file"
  record_unverified 'direct overwrite of an older Candidate is only safe when its installed certificate matches the recorded SHA-256 digest; no old APK is available to compare'
}

dump_diagnostics() {
  local status=$?
  if command -v adb >/dev/null 2>&1; then
    capture_log
    dump_ui || true
    dump_services
    {
      printf '\n--- emulator script exit: %s ---\n' "$status"
      printf '\n--- process ---\n'
      bounded_timeout 30s adb shell pidof "$CANDIDATE_PACKAGE" || true
      printf '\n--- package ---\n'
      bounded_timeout 30s adb shell dumpsys package "$CANDIDATE_PACKAGE" || true
      printf '\n--- activity ---\n'
      bounded_timeout 30s adb shell dumpsys activity activities || true
      printf '\n--- services ---\n'
      cat "$service_file" || true
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
if [[ ! -f "$fixture_config" || ! -f "$fixture_preferences" ]]; then
  record_error 'CI no-credential Android fixture is missing'
  exit 1
fi

zip_entries="$(unzip -Z1 "$apk")"
grep -q '^lib/x86_64/' <<< "$zip_entries" || { record_error "selected APK does not contain x86_64 native libraries: $apk"; exit 1; }
if grep -q '^lib/arm64-v8a/' <<< "$zip_entries"; then
  record_error "selected APK contains arm64 native libraries: $apk"
  exit 1
fi
record_phase 'APK architecture, package identity, and signing certificate verification'
record_pass "selected emulator APK is x86_64: $apk"
verify_apk_identity_and_signatures

record_phase 'emulator device and boot readiness'
device_state=''
device_deadline=$((SECONDS + 180))
while (( SECONDS < device_deadline )); do
  device_state="$(adb_output_once get-state || true)"
  [[ "$device_state" == 'device' ]] && break
  sleep 2
done
[[ "$device_state" == 'device' ]] || { record_error "emulator did not become ready: $device_state"; exit 1; }
boot_completed=''
boot_deadline=$((SECONDS + 180))
while (( SECONDS < boot_deadline )); do
  boot_completed="$(adb_output_once shell getprop sys.boot_completed | tr -d '\r' || true)"
  [[ "$boot_completed" == '1' ]] && break
  sleep 2
done
[[ "$boot_completed" == '1' ]] || { record_error "emulator boot did not complete: $boot_completed"; exit 1; }
record_pass 'Android emulator reached sys.boot_completed=1'

record_phase 'APK install, replacement install, and Flutter launch'
record_phase 'emulator package verification setup'
adb_retry shell settings put global verifier_verify_adb_installs 0 || {
  record_error 'failed to disable adb install verification on the test emulator'
  exit 1
}
adb_retry shell settings put global package_verifier_enable 0 || {
  record_error 'failed to disable package verification on the test emulator'
  exit 1
}
record_pass 'emulator package verification disabled for deterministic APK installation'
record_phase 'initial x86_64 Candidate APK install'
adb_install_candidate || { record_error "failed to install x86_64 APK: $apk"; exit 1; }
record_pass 'initial x86_64 Candidate APK install succeeded'
record_phase 'same-certificate Candidate replacement install'
adb_install_candidate || { record_error 'same-certificate replacement install failed'; exit 1; }
record_pass 'same-certificate Candidate replacement install succeeded'
record_pass 'Candidate APK installed and replacement install succeeded'
record_phase 'Flutter launcher dispatch'
adb_retry logcat -c || { record_error 'failed to clear logcat before launch'; exit 1; }
adb_retry shell am force-stop "$CANDIDATE_PACKAGE" || { record_error "failed to reset $CANDIDATE_PACKAGE before launch"; exit 1; }
adb_retry shell monkey -p "$CANDIDATE_PACKAGE" -c android.intent.category.LAUNCHER 1 || { record_error "failed to launch $CANDIDATE_PACKAGE"; exit 1; }
record_pass 'Flutter launcher dispatch returned successfully'

pid=''
pid_deadline=$((SECONDS + 180))
while (( SECONDS < pid_deadline )); do
  pid="$(adb_output_once shell pidof "$CANDIDATE_PACKAGE" | tr -d '\r' | awk '{print $1}' || true)"
  [[ "$pid" =~ [0-9] ]] && break
  sleep 2
done
[[ "$pid" =~ [0-9] ]] || { record_error "application process did not start: $CANDIDATE_PACKAGE"; exit 1; }

startup_complete=0
activity=''
record_phase 'main activity, Flutter initialization, and visible main navigation'
startup_deadline=$((SECONDS + 300))
while (( SECONDS < startup_deadline )); do
  capture_log
  dump_ui || true
  if handle_first_run_dialogs; then sleep 2; continue; fi
  activity="$(adb_output_once shell dumpsys activity activities | tr -d '\r' | grep -m 1 'mResumedActivity' || true)"
  has_core_init=0; has_core_ready=0; has_config_setup=0; has_init_status=0; has_main_ui=0
  grep -q '\[APP\] Invoke method initClash completed' "$log_file" && has_core_init=1
  grep -q '\[APP\] Invoke method getIsInit completed' "$log_file" && has_core_ready=1
  grep -q '\[APP\] Invoke method setupConfig completed' "$log_file" && has_config_setup=1
  grep -q '\[APP\] init status' "$log_file" && has_init_status=1
  grep -Eq 'text="(Dashboard|Profiles|Proxies|Tools|仪表盘|配置|代理|工具)"' "$ui_file" && has_main_ui=1
  if [[ "$activity" == *"$CANDIDATE_PACKAGE/.MainActivity"* && "$has_core_init" == 1 && "$has_core_ready" == 1 && "$has_config_setup" == 1 && "$has_init_status" == 1 && "$has_main_ui" == 1 ]]; then
    startup_complete=1
    break
  fi
  sleep 2
done
if [[ "$startup_complete" != 1 ]]; then
  record_error 'startup did not reach MainActivity, Flutter initialization, core readiness, setup completion, and a visible main navigation label within 240 seconds'
  printf 'last_resumed_activity: %s\nrequired_markers: initClash getIsInit setupConfig init_status main_navigation\n' "$activity" >> "$acceptance_file"
  exit 1
fi
record_pass 'MainActivity reached the main navigation UI'
record_pass 'Flutter startup initialization completed (initClash, getIsInit, setupConfig, init status)'
record_pass 'Flutter-to-Android-to-JNI/Go method channel completed without a startup error'

# Install the fixture after the UI check and after stopping Flutter, so Flutter cannot overwrite it.
record_phase 'no-credential native QuickAction START and Adaptive fixture validation'
adb_retry shell am force-stop "$CANDIDATE_PACKAGE" || { record_error 'failed to stop the UI process before service-chain validation'; exit 1; }
adb_retry push "$fixture_config" /data/local/tmp/flclash-adaptive-config.yaml || { record_error 'failed to stage the no-credential config fixture'; exit 1; }
adb_retry push "$fixture_preferences" /data/local/tmp/flclash-adaptive-preferences.xml || { record_error 'failed to stage the no-credential shared-state fixture'; exit 1; }
adb_retry shell run-as "$CANDIDATE_PACKAGE" cp /data/local/tmp/flclash-adaptive-config.yaml files/config.yaml || { record_error 'failed to install the no-credential config inside the Candidate sandbox'; exit 1; }
adb_retry shell run-as "$CANDIDATE_PACKAGE" cp /data/local/tmp/flclash-adaptive-preferences.xml shared_prefs/FlutterSharedPreferences.xml || { record_error 'failed to install the no-credential shared-state inside the Candidate sandbox'; exit 1; }
adb_retry logcat -c || { record_error 'failed to clear logcat before native service validation'; exit 1; }
adb_retry shell am start -a "$CANDIDATE_PACKAGE.action.START" -n "$CANDIDATE_PACKAGE/.QuickActionActivity" || { record_error 'failed to dispatch the native QuickAction START intent'; exit 1; }

proxy_service_started=0
adaptive_fixture_loaded=0
service_deadline=$((SECONDS + 180))
while (( SECONDS < service_deadline )); do
  capture_log
  dump_services
  grep -q 'ProxyService' "$service_file" && proxy_service_started=1
  grep -q '\[ADAPTIVE\] disabled: inline SS source' "$log_file" && adaptive_fixture_loaded=1
  if grep -Eq 'Unable to set up core|Unable to bind background service|Unable to start background service|No configuration found|Invalid configuration' "$log_file"; then
    record_error 'native service fixture reported a setup or start failure'
    exit 1
  fi
  [[ "$proxy_service_started" == 1 && "$adaptive_fixture_loaded" == 1 ]] && break
  sleep 2
done
[[ "$proxy_service_started" == 1 ]] || { record_error 'ProxyService did not become active from the native QuickAction path'; exit 1; }
[[ "$adaptive_fixture_loaded" == 1 ]] || { record_error 'Go Adaptive config hook did not report the expected fail-closed result for the fake endpoint'; exit 1; }
record_pass 'native QuickAction -> Android service -> JNI/Go quickSetup chain started ProxyService'
record_pass 'no-credential config reached Adaptive and failed closed without the real endpoint'

adb_retry shell am start -a "$CANDIDATE_PACKAGE.action.STOP" -n "$CANDIDATE_PACKAGE/.QuickActionActivity" || { record_error 'failed to dispatch the native QuickAction STOP intent'; exit 1; }
record_phase 'native service stop and fatal-condition scan'
service_stopped=0
stop_deadline=$((SECONDS + 90))
while (( SECONDS < stop_deadline )); do
  dump_services
  if ! grep -q 'ProxyService' "$service_file"; then service_stopped=1; break; fi
  sleep 2
done
[[ "$service_stopped" == 1 ]] || { record_error 'ProxyService did not stop after the native QuickAction STOP intent'; exit 1; }
record_pass 'native service stop path completed'
record_unverified 'VpnService/TUN establishment was not granted by the emulator system-consent flow'
record_unverified 'Adaptive real-server routing and failover were not exercised; the fixture uses 192.0.2.1 and dummy credentials only'

capture_log
if grep -Eq 'FATAL EXCEPTION|Fatal signal|SIGSEGV|UnsatisfiedLinkError|ANR in|Process .* has died' "$log_file"; then
  grep -E -C 3 'FATAL EXCEPTION|Fatal signal|SIGSEGV|UnsatisfiedLinkError|ANR in|Process .* has died' "$log_file" || true
  record_error "application reported a fatal condition during startup or native service validation: $CANDIDATE_PACKAGE"
  exit 1
fi
record_pass 'no fatal exception, native crash, or ANR marker was observed during the completed checks'
