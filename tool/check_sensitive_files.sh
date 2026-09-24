#!/usr/bin/env bash

set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  bash tool/check_sensitive_files.sh --staged
  bash tool/check_sensitive_files.sh --range BASE_SHA HEAD_SHA
  bash tool/check_sensitive_files.sh --all
EOF
}

mode='staged'
base_sha=''
head_sha=''

case "${1:-}" in
  --staged)
    mode='staged'
    ;;
  --range)
    if [[ $# -ne 3 ]]; then
      usage >&2
      exit 2
    fi
    mode='range'
    base_sha="$2"
    head_sha="$3"
    ;;
  --all)
    mode='all'
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac

findings=0

report() {
  local path="$1"
  local line="$2"
  local kind="$3"
  printf 'SENSITIVE_CHECK|path=%s|line=%s|type=%s\n' "$path" "$line" "$kind" >&2
  findings=$((findings + 1))
}

is_intentional_fixture() {
  case "$1" in
    core/Clash.Meta/docs/config.yaml \
      | core/Clash.Meta/test/config/example.org-key.pem \
      | core/Clash.Meta/test/config/example.org.pem \
      | core/Clash.Meta/transport/openvpn/config_test.go \
      | core/Clash.Meta/common/convert/converter_test.go \
      | core/Clash.Meta/transport/sudoku/kip.go \
      | core/Clash.Meta/listener/shadowsocks/utils_test.go \
      | core/Clash.Meta/listener/sing_vmess/server_test.go \
      | .github/fixtures/*)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

is_placeholder_line() {
  local line="$1"
  [[ "$line" =~ (test|dummy|example|placeholder|change[-_]me|ci[-_]test|password|secret) ]] \
    || [[ "$line" =~ (192\.0\.2\.|198\.51\.100\.|203\.0\.113\.|example\.test|example\.org|example\.com) ]]
}

check_line() {
  local path="$1"
  local line_number="$2"
  local line="$3"

  if [[ "$line" =~ -----BEGIN[[:space:]][A-Z0-9[:space:]]*PRIVATE[[:space:]]KEY----- ]]; then
    report "$path" "$line_number" 'private-key-material'
  fi

  if [[ "$line" =~ (ghp_|gho_|ghs_|ghr_|ghu_|github_pat_)[A-Za-z0-9_-]{10,} ]]; then
    report "$path" "$line_number" 'github-token'
  fi

  if [[ "$line" =~ (AKIA|ASIA)[0-9A-Z]{16} ]]; then
    report "$path" "$line_number" 'aws-access-key'
  fi

  if [[ "$line" =~ xox[baprs]-[0-9A-Za-z-]{10,} ]]; then
    report "$path" "$line_number" 'slack-token'
  fi

  if [[ "$line" =~ (sk|rk)_live_[0-9A-Za-z]{10,} ]]; then
    report "$path" "$line_number" 'stripe-live-key'
  fi

  if [[ "$line" =~ npm_[A-Za-z0-9]{20,} ]]; then
    report "$path" "$line_number" 'npm-token'
  fi

  if [[ "$line" =~ pypi-[A-Za-z0-9_-]{20,} ]]; then
    report "$path" "$line_number" 'pypi-token'
  fi

  if [[ "$line" =~ AIza[0-9A-Za-z_-]{20,} ]]; then
    report "$path" "$line_number" 'google-api-key'
  fi

  if [[ "$line" =~ (https?|socks4|socks5|ss|vmess|trojan|vless)://[^[:space:]/:@]+:[^[:space:]@]+@ ]]; then
    report "$path" "$line_number" 'credential-url'
  fi

  if [[ "$line" =~ ss://[A-Za-z0-9_=-]{24,} ]]; then
    report "$path" "$line_number" 'shadowsocks-uri'
  fi

  local assignment_re
  assignment_re="(^|[^A-Za-z0-9_])(password|passwd|passphrase|psk|api[_-]?key|access[_-]?token|auth[_-]?token|client[_-]?secret|secret[_-]?key|private[_-]?key|subscription[_-]?url)[^A-Za-z0-9_]?[[:space:]]*[:=][[:space:]]*['\"]*[A-Za-z0-9_./+@?%:-]{8,}"
  if [[ "$line" =~ $assignment_re ]] && ! is_placeholder_line "$line"; then
    report "$path" "$line_number" 'credential-like-assignment'
  fi
}

check_text() {
  local path="$1"
  local content="$2"
  local line_number=0
  local line

  while IFS= read -r line || [[ -n "$line" ]]; do
    line_number=$((line_number + 1))
    check_line "$path" "$line_number" "$line"
  done <<< "$content"
}

check_staged_path() {
  local path="$1"
  local diff

  case "$path" in
    .env|.env.*|*/.env|*/.env.*|*/.ssh/id_*|*/id_rsa|*/id_ed25519|*/id_ecdsa|*/id_dsa|*.p12|*.pfx|*.jks|*.keystore|*.key|*/private*.pem|*-private.pem|*_private.pem|*-key.pem|*_key.pem)
      report "$path" '1' 'credential-bearing-filename'
      ;;
  esac

  diff="$(git diff --cached --no-color --unified=0 -- "$path")"
  while IFS= read -r line; do
    [[ "$line" == ++++* ]] && continue
    [[ "$line" != +* ]] && continue
    check_line "$path" 'diff' "${line:1}"
  done <<< "$diff"
}

check_range_path() {
  local path="$1"
  local diff

  case "$path" in
    .env|.env.*|*/.env|*/.env.*|*/.ssh/id_*|*/id_rsa|*/id_ed25519|*/id_ecdsa|*/id_dsa|*.p12|*.pfx|*.jks|*.keystore|*.key|*/private*.pem|*-private.pem|*_private.pem|*-key.pem|*_key.pem)
      report "$path" '1' 'credential-bearing-filename'
      ;;
  esac

  if [[ -z "$base_sha" || "$base_sha" =~ ^0+$ ]]; then
    diff="$(git show --format= --no-color --unified=0 "$head_sha" -- "$path")"
  else
    diff="$(git diff "$base_sha" "$head_sha" --no-color --unified=0 -- "$path")"
  fi
  while IFS= read -r line; do
    [[ "$line" == ++++* ]] && continue
    [[ "$line" != +* ]] && continue
    check_line "$path" 'diff' "${line:1}"
  done <<< "$diff"
}

check_all() {
  local all_re
  local match
  local entry
  local path
  local line_number
  local content

  all_re='-----BEGIN[[:space:]][A-Z0-9[:space:]]*PRIVATE[[:space:]]KEY-----|ghp_|gho_|ghs_|ghr_|ghu_|github_pat_|(AKIA|ASIA)[0-9A-Z]{16}|xox[baprs]-[0-9A-Za-z-]{10,}|(sk|rk)_live_[0-9A-Za-z]{10,}|npm_[A-Za-z0-9]{20,}|pypi-[A-Za-z0-9_-]{20,}|AIza[0-9A-Za-z_-]{20,}|(https?|socks4|socks5|ss|vmess|trojan|vless)://|ss://[A-Za-z0-9_=-]{24,}|(password|passwd|passphrase|psk|api[_-]?key|access[_-]?token|auth[_-]?token|client[_-]?secret|secret[_-]?key|private[_-]?key|subscription[_-]?url)[^A-Za-z0-9_]*[:=]'
  while IFS= read -r match; do
    entry="${match#HEAD:}"
    path="${entry%%:*}"
    entry="${entry#*:}"
    line_number="${entry%%:*}"
    content="${entry#*:}"
    if is_intentional_fixture "$path"; then
      continue
    fi
    check_line "$path" "$line_number" "$content"
  done < <(git grep -I -n -E -e "$all_re" HEAD -- . || true)
}

case "$mode" in
  staged)
    while IFS= read -r -d '' path; do
      check_staged_path "$path"
    done < <(git diff --cached --name-only --diff-filter=ACMRT -z)
    ;;
  range)
    if [[ -z "$base_sha" || "$base_sha" =~ ^0+$ ]]; then
      while IFS= read -r -d '' path; do
        check_range_path "$path"
      done < <(git diff-tree --root --no-commit-id --name-only --diff-filter=ACMRT -r -z "$head_sha")
    else
      while IFS= read -r -d '' path; do
        check_range_path "$path"
      done < <(git diff --name-only --diff-filter=ACMRT -z "$base_sha" "$head_sha")
    fi
    ;;
  all)
    check_all
    ;;
esac

if (( findings > 0 )); then
  printf 'Sensitive information check failed with %d finding(s).\n' "$findings" >&2
  exit 1
fi

printf 'Sensitive information check passed (%s).\n' "$mode"
