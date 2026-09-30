#!/usr/bin/env bash

# Root-only, non-interactive updater used by the web-triggered update service.
# It intentionally accepts no arguments so the service account cannot redirect
# the update to an arbitrary command or installation directory.
set -Eeuo pipefail

repo_owner="yizuodi"
repo_name="GoEasyConnect"
service_name="easyconnect"
backup_root="/var/backups/easyconnect"
lock_file="/run/lock/goeasyconnect-updater.lock"

log() { printf '[goeasyconnect-updater] %s\n' "$*" >&2; }
die() { log "Error: $*"; exit 1; }
[[ ${EUID} -eq 0 ]] || die "must run as root"
[[ $# -eq 0 ]] || die "arguments are not accepted"

for command_name in curl tar sha256sum install mktemp realpath systemctl getent runuser cp mv rm flock; do
  command -v "${command_name}" >/dev/null 2>&1 || die "required command is missing: ${command_name}"
done

exec 9>"${lock_file}"
flock -n 9 || die "another update is already running"

read_unit_value() {
  local key="$1" line
  while IFS= read -r line; do
    case "${line}" in
      "${key}="*) printf '%s\n' "${line#*=}"; return 0 ;;
    esac
  done <"${unit_file}"
  return 1
}

unit_file="$(systemctl show --property=FragmentPath --value "${service_name}.service")"
[[ -n "${unit_file}" && -f "${unit_file}" ]] || die "${service_name}.service is not installed"
service_user="$(read_unit_value User || true)"
service_group="$(read_unit_value Group || true)"
service_home="$(read_unit_value Environment | sed -n 's/^HOME=//p' | head -n 1 || true)"
[[ -n "${service_user}" ]] || die "cannot determine service user"
getent passwd "${service_user}" >/dev/null 2>&1 || die "service user does not exist"
service_group="${service_group:-$(id -gn "${service_user}")}"
service_home="${service_home:-$(getent passwd "${service_user}" | cut -d: -f6)}"
exec_start="$(read_unit_value ExecStart || true)"
[[ -n "${exec_start}" ]] || die "cannot determine service command"
read -r -a exec_arguments <<<"${exec_start}"
installed_binary="${exec_arguments[0]}"
[[ "${installed_binary}" == /* && -x "${installed_binary}" ]] || die "installed binary is missing"
install_dir="$(realpath -m "$(dirname "${installed_binary}")")"
config_path="${install_dir}/config.json"
expect_config=no
for argument in "${exec_arguments[@]:1}"; do
  if [[ "${expect_config}" == yes ]]; then config_path="${argument}"; expect_config=no; continue; fi
  case "${argument}" in
    --config) expect_config=yes ;;
    --config=*) config_path="${argument#--config=}" ;;
  esac
done
[[ "${expect_config}" == no && "${config_path}" == /* && -f "${config_path}" ]] || die "configuration file is missing"
config_path="$(realpath -m "${config_path}")"

architecture="$(uname -m)"
case "${architecture}" in x86_64) architecture=amd64 ;; aarch64) architecture=arm64 ;; *) die "unsupported CPU architecture" ;; esac

temporary_dir="$(mktemp -d)"
trap 'rm -rf -- "${temporary_dir}"' EXIT HUP INT TERM
chmod 0700 "${temporary_dir}"
release_json="$(curl --fail --location --silent --show-error --connect-timeout 15 --max-time 120 -H 'Accept: application/vnd.github+json' "https://api.github.com/repos/${repo_owner}/${repo_name}/releases/latest")" || die "cannot query latest release"
release_tag="$(printf '%s\n' "${release_json}" | sed -n 's/^  "tag_name": *"\(v[0-9][0-9A-Za-z._-]*\)".*/\1/p' | head -n 1)"
[[ "${release_tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] || die "latest release tag is invalid"
archive_name="${repo_name}_${release_tag}_linux_${architecture}.tar.gz"
curl --fail --location --retry 5 --retry-delay 2 --silent --show-error --connect-timeout 15 --max-time 900 -o "${temporary_dir}/${archive_name}" "https://github.com/${repo_owner}/${repo_name}/releases/download/${release_tag}/${archive_name}" || die "cannot download release"
curl --fail --location --retry 5 --retry-delay 2 --silent --show-error --connect-timeout 15 --max-time 300 -o "${temporary_dir}/checksums.txt" "https://github.com/${repo_owner}/${repo_name}/releases/download/${release_tag}/checksums.txt" || die "cannot download checksums"
expected_checksum="$(sed -n "s/^\([0-9a-fA-F]\{64\}\)  ${archive_name}$/\1/p" "${temporary_dir}/checksums.txt")"
[[ "${expected_checksum}" =~ ^[0-9a-fA-F]{64}$ ]] || die "release checksum is missing"
actual_checksum="$(sha256sum "${temporary_dir}/${archive_name}" | cut -d' ' -f1)"
[[ "${actual_checksum,,}" == "${expected_checksum,,}" ]] || die "release checksum verification failed"
mkdir -p "${temporary_dir}/package"
tar -xzf "${temporary_dir}/${archive_name}" -C "${temporary_dir}/package"
new_binary="${temporary_dir}/package/easyconnect"
[[ -x "${new_binary}" ]] || die "release does not contain an executable"
new_helper="${temporary_dir}/package/update-helper.sh"
[[ -x "${new_helper}" ]] || die "release does not contain the updater helper"
installed_version="$(${installed_binary} --version 2>/dev/null || true)"
if [[ "${installed_version}" == "${release_tag}" ]]; then log "already running ${release_tag}"; exit 0; fi

database_path="$(runuser -u "${service_user}" -- env HOME="${service_home}" "${new_binary}" --print-database-path --config "${config_path}")" || die "cannot resolve database path"
[[ "${database_path}" == /* && "${database_path}" != *$'\n'* && "${database_path}" != *$'\r'* ]] || die "database path is invalid"
database_path="$(realpath -m "${database_path}")"
service_was_active=no
systemctl is-active --quiet "${service_name}.service" && service_was_active=yes
systemctl stop "${service_name}.service" || die "cannot stop service"
stamp="$(date +%Y%m%d%H%M%S)"
backup_dir="${backup_root}/${stamp}-${release_tag}-web"
install -d -o root -g root -m 0700 "${backup_dir}/database"
cp -a "${installed_binary}" "${backup_dir}/easyconnect"
cp -a "${config_path}" "${backup_dir}/config.json"
cp -a "${unit_file}" "${backup_dir}/easyconnect.service"
for suffix in "" -wal -shm; do [[ -e "${database_path}${suffix}" ]] && cp -a "${database_path}${suffix}" "${backup_dir}/database/database${suffix}" || true; done

rollback() {
  trap - ERR HUP INT TERM
  log "upgrade failed; restoring ${backup_dir}"
  systemctl stop "${service_name}.service" >/dev/null 2>&1 || true
  install -o root -g root -m 0755 "${backup_dir}/easyconnect" "${installed_binary}.rollback"
  mv -f "${installed_binary}.rollback" "${installed_binary}"
  for suffix in "" -wal -shm; do
    if [[ -e "${backup_dir}/database/database${suffix}" ]]; then cp -a "${backup_dir}/database/database${suffix}" "${database_path}${suffix}"; else rm -f -- "${database_path}${suffix}"; fi
  done
  if [[ "${service_was_active}" == yes ]]; then systemctl start "${service_name}.service" >/dev/null 2>&1 || true; fi
  die "previous version restored; backup retained at ${backup_dir}"
}
trap 'rollback' ERR
install -o root -g root -m 0755 "${new_binary}" "${installed_binary}.new"
mv -f "${installed_binary}.new" "${installed_binary}"
runuser -u "${service_user}" -- env HOME="${service_home}" "${installed_binary}" --check --config "${config_path}" || rollback
if [[ "${service_was_active}" == yes ]]; then systemctl start "${service_name}.service" || rollback; systemctl is-active --quiet "${service_name}.service" || rollback; fi
trap - ERR
install -o root -g root -m 0755 "${new_helper}" "/usr/local/libexec/goeasyconnect-updater"
log "upgraded from ${installed_version:-unknown} to ${release_tag}; backup=${backup_dir}"
