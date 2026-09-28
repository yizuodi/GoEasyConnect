#!/usr/bin/env bash

set -Eeuo pipefail

repo_dir="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
# shellcheck source=install.sh
source "${repo_dir}/scripts/install.sh"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_file_content() {
  local path="$1"
  local expected="$2"
  local actual

  actual="$(<"${path}")"
  [[ "${actual}" == "${expected}" ]] || fail "${path}: expected '${expected}', got '${actual}'"
}

write_binary() {
  local path="$1"
  local binary_version="$2"
  local check_result="$3"

  sed \
    -e "s|@VERSION@|${binary_version}|g" \
    -e "s|@CHECK_RESULT@|${check_result}|g" \
    -e "s|@DATABASE@|${test_database}|g" \
    >"${path}" <<'SCRIPT'
#!/usr/bin/env bash
case "${1:-}" in
  --version)
    printf '%s\n' '@VERSION@'
    ;;
  --print-database-path)
    printf '%s\n' '@DATABASE@'
    ;;
  --check)
    if [[ '@CHECK_RESULT@' == 'fail' ]]; then
      printf '%s' 'migrated-by-failed-upgrade' >'@DATABASE@'
      exit 1
    fi
    ;;
esac
SCRIPT
  chmod 0755 "${path}"
}

run_upgrade_case() (
  local check_result="$1"
  local expected_status="$2"
  local case_root
  local status=0

  case_root="$(mktemp -d)"
  trap 'rm -rf -- "${case_root}"' EXIT
  test_database="${case_root}/runtime/custom.db"
  install_dir="${case_root}/install"
  installed_binary="${install_dir}/easyconnect"
  config_path="${case_root}/config.json"
  unit_file="${case_root}/easyconnect.service"
  backup_root="${case_root}/backups"
  service_user="$(id -un)"
  service_group="$(id -gn)"
  service_home="${case_root}/home"
  service_active="yes"
  systemctl_log="${case_root}/systemctl.log"

  mkdir -p "${install_dir}" "$(dirname "${test_database}")" "${service_home}"
  printf '%s' 'original-database' >"${test_database}"
  printf '%s' 'original-wal' >"${test_database}-wal"
  printf '%s\n' '{"auth":{"password":"test"}}' >"${config_path}"
  printf '%s\n' '[Service]' >"${unit_file}"
  write_binary "${installed_binary}" v0.3.1 pass

  require_root() { :; }
  require_debian() { :; }
  detect_architecture() { printf '%s' amd64; }
  detect_existing_installation() { :; }
  download_latest_release() {
    release_version=v0.3.2
    mkdir -p "${temporary_dir}/package"
    write_binary "${temporary_dir}/package/easyconnect" v0.3.2 "${check_result}"
  }
  systemctl() {
    printf '%s\n' "$*" >>"${systemctl_log}"
    case "${1:-}" in
      is-active)
        [[ "${service_active}" == "yes" ]]
        ;;
      stop)
        service_active="no"
        ;;
      start)
        service_active="yes"
        ;;
      daemon-reload)
        ;;
    esac
  }
  runuser() {
    while [[ "${1:-}" != "--" ]]; do shift; done
    shift
    "$@"
  }
  install() {
    local arguments=()
    while (($#)); do
      case "$1" in
        -o|-g)
          shift 2
          ;;
        *)
          arguments+=("$1")
          shift
          ;;
      esac
    done
    command install "${arguments[@]}"
  }
  chown() { :; }

  set +e
  (upgrade_main)
  status=$?
  set -e
  [[ "${status}" == "${expected_status}" ]] || fail "upgrade status: expected ${expected_status}, got ${status}"

  if [[ "${check_result}" == "pass" ]]; then
    [[ "$("${installed_binary}" --version)" == "v0.3.2" ]] || fail "new binary was not installed"
    assert_file_content "${test_database}" original-database
  else
    [[ "$("${installed_binary}" --version)" == "v0.3.1" ]] || fail "old binary was not restored"
    assert_file_content "${test_database}" original-database
    assert_file_content "${test_database}-wal" original-wal
  fi
  grep -q '^stop easyconnect.service$' "${systemctl_log}" || fail "service was not stopped"
  grep -q '^start easyconnect.service$' "${systemctl_log}" || fail "service was not started"
  [[ -n "$(find "${backup_root}" -type f -name easyconnect -print -quit)" ]] || fail "binary backup missing"
)

run_upgrade_case pass 0
run_upgrade_case fail 1
printf '%s\n' 'install upgrade tests passed'
