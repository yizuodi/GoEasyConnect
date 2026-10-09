#!/usr/bin/env bash

set -Eeuo pipefail

repo_owner="yizuodi"
repo_name="GoEasyConnect"
service_name="easyconnect"
default_install_dir="/srv/GoEasyConnect"
default_port="26890"
backup_root="/var/backups/easyconnect"
update_helper_path="/usr/local/libexec/goeasyconnect-updater"
update_unit_name="goeasyconnect-updater.service"
update_sudoers_path="/etc/sudoers.d/goeasyconnect-updater"

die() {
  printf 'Error: %s\n' "$*" >&2
  exit 1
}

info() {
  printf '%s\n' "$*"
}

prompt_value() {
  local label="$1"
  local default_value="$2"
  local value

  if [[ -n "${default_value}" ]]; then
    printf '%s [%s]: ' "${label}" "${default_value}" >&2
  else
    printf '%s: ' "${label}" >&2
  fi
  read -r value </dev/tty || die "interactive input is required"
  printf '%s' "${value:-${default_value}}"
}

prompt_password() {
  local label="$1"
  local value
  local confirmation

  printf '%s: ' "${label}" >&2
  read -rs value </dev/tty || die "interactive input is required"
  printf '\n%s: ' "Confirm password" >&2
  read -rs confirmation </dev/tty || die "interactive input is required"
  printf '\n' >&2

  [[ "${value}" == "${confirmation}" ]] || die "passwords do not match"
  printf '%s' "${value}"
}

confirm() {
  local label="$1"
  local default_answer="${2:-yes}"
  local answer

  if [[ "${default_answer}" == "yes" ]]; then
    printf '%s [Y/n]: ' "${label}" >&2
  else
    printf '%s [y/N]: ' "${label}" >&2
  fi
  read -r answer </dev/tty || die "interactive input is required"
  answer="${answer:-${default_answer}}"
  [[ "${answer}" =~ ^[Yy]([Ee][Ss])?$ ]]
}

json_escape() {
  local value="$1"
  local result=""
  local index
  local character

  for ((index = 0; index < ${#value}; index++)); do
    character="${value:index:1}"
    case "${character}" in
      \"|\\)
        result+="\\${character}"
        ;;
      $'\n')
        result+='\\n'
        ;;
      $'\r')
        result+='\\r'
        ;;
      $'\t')
        result+='\\t'
        ;;
      *)
        if [[ "${character}" =~ [[:cntrl:]] ]]; then
          die "control characters are not supported in this value"
        fi
        result+="${character}"
        ;;
    esac
  done
  printf '%s' "${result}"
}

require_root() {
  [[ ${EUID} -eq 0 ]] || die "this installer must run as root"
}

require_debian() {
  [[ -r /etc/os-release ]] || die "only Debian is supported in this first version"
  # shellcheck disable=SC1091
  . /etc/os-release
  [[ "${ID:-}" == "debian" ]] || die "only Debian is supported in this first version"
}

detect_architecture() {
  case "$(uname -m)" in
    x86_64)
      printf 'amd64'
      ;;
    aarch64)
      printf 'arm64'
      ;;
    *)
      die "unsupported CPU architecture: $(uname -m)"
      ;;
  esac
}

validate_username() {
  local username="$1"

  [[ "${username}" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]] || die "invalid Debian username: ${username}"
  [[ "${username}" != "root" ]] || die "running GoEasyConnect as root is not supported"
}

validate_install_dir() {
  local path="$1"

  [[ -n "${path}" ]] || die "installation directory cannot be empty"
  [[ "${path}" =~ [[:space:]] ]] && die "installation directory cannot contain whitespace"
  [[ "${path}" == /* ]] || die "installation directory must be absolute"
  [[ "${path}" != */../* && "${path}" != */.. ]] || die "installation directory cannot contain '..'"

  case "${path}" in
    /|/bin|/boot|/dev|/etc|/home|/lib|/lib64|/media|/mnt|/opt|/proc|/root|/run|/sbin|/srv|/sys|/tmp|/usr|/var)
      die "refusing to use a top-level system directory as the installation directory"
      ;;
  esac
}

validate_port() {
  local port="$1"

  [[ "${port}" =~ ^[0-9]+$ ]] || die "port must be a number"
  (( port >= 1 && port <= 65535 )) || die "port must be between 1 and 65535"
}

install_sudo_if_needed() {
  if ! command -v sudo >/dev/null 2>&1; then
    info "Installing sudo so the new service user can elevate privileges..."
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y sudo
  fi
}

select_service_user() {
  local default_user="${1:-easyconnect}"
  local username
  local choice
  service_user_password_set="no"

  while true; do
    username="$(prompt_value "GoEasyConnect and AI session user" "${default_user}")"
    validate_username "${username}"

    if getent passwd "${username}" >/dev/null 2>&1; then
      service_user="${username}"
      break
    fi

    printf 'User %s does not exist.\n' "${username}" >&2
    printf '  1) Enter a different username\n' >&2
    printf '  2) Create this user now with a home directory, bash shell, and sudo group membership\n' >&2
    choice="$(prompt_value "Choose an option" "1")"
    case "${choice}" in
      1)
        continue
        ;;
      2)
        install_sudo_if_needed
        useradd --create-home --shell /bin/bash "${username}"
        usermod --append --groups sudo "${username}"
        info "Created ${username} and added it to the sudo group."
        if confirm "Set a login password for ${username} now so it can use sudo?" yes; then
          passwd "${username}"
          service_user_password_set="yes"
        else
          info "Set a login password later before using sudo: passwd ${username}"
        fi
        service_user="${username}"
        break
        ;;
      *)
        ;;
    esac
  done
}

resolve_service_user_details() {
  local passwd_entry

  passwd_entry="$(getent passwd "${service_user}")" || die "cannot read user ${service_user}"
  service_group="$(id -gn "${service_user}")" || die "cannot read group for ${service_user}"
  service_home="$(printf '%s' "${passwd_entry}" | cut -d: -f6)"
  [[ -n "${service_home}" && "${service_home}" == /* ]] || die "user ${service_user} has no usable home directory"

  if [[ ! -d "${service_home}" ]]; then
    install -d -o "${service_user}" -g "${service_group}" -m 0750 "${service_home}"
  fi
}

select_listen_host() {
  local choice

  while true; do
    info "Listen mode:"
    info "  1) Local only, 127.0.0.1 (recommended and default)"
    info "  2) All interfaces, 0.0.0.0"
    choice="$(prompt_value "Choose listen mode" "1")"
    case "${choice}" in
      1)
        listen_host="127.0.0.1"
        break
        ;;
      2)
        listen_host="0.0.0.0"
        info "Warning: use an HTTPS reverse proxy or a trusted private network for 0.0.0.0."
        break
        ;;
    esac
  done
}

select_working_directory() {
  local working_directory

  while true; do
    working_directory="$(prompt_value "Default AI session working directory" "${service_home}")"
    [[ "${working_directory}" == /* ]] || die "working directory must be absolute"
    [[ "${working_directory}" =~ [[:space:]] ]] && die "working directory cannot contain whitespace"
    if [[ ! -d "${working_directory}" ]]; then
      confirm "Create ${working_directory}?" yes || continue
      install -d -o "${service_user}" -g "${service_group}" -m 0750 "${working_directory}"
    fi
    if ! runuser -u "${service_user}" -- test -w "${working_directory}"; then
      info "Warning: ${service_user} cannot write to ${working_directory}."
      confirm "Use this directory anyway?" no || continue
    fi
    session_working_dir="${working_directory}"
    break
  done
}

download_latest_release() {
  local architecture="$1"
  local release_json
  local release_tag
  local archive_name
  local checksums
  local actual_checksum
  local checksum_file
  local checksum_value

  info "Finding the latest GitHub Release..."
  release_json="$(curl --fail --location --silent --show-error \
    --connect-timeout 15 --max-time 120 \
    -H 'Accept: application/vnd.github+json' \
    "https://api.github.com/repos/${repo_owner}/${repo_name}/releases/latest")" \
    || die "cannot query the latest GitHub Release"

  release_tag="$(printf '%s\n' "${release_json}" | python3 -c 'import json,sys; release=json.load(sys.stdin); tag=release.get("tag_name"); assert isinstance(tag,str) and not release.get("draft") and not release.get("prerelease"); print(tag)')" \
    || die "cannot parse the latest GitHub Release JSON"
  [[ "${release_tag}" =~ ^v[0-9A-Za-z._-]+$ ]] || die "latest release returned an invalid tag"
  release_version="${release_tag}"

  archive_name="${repo_name}_${release_tag}_linux_${architecture}.tar.gz"
  curl --fail --location --retry 5 --retry-delay 2 --silent --show-error \
    --connect-timeout 15 --max-time 900 \
    -o "${temporary_dir}/${archive_name}" \
    "https://github.com/${repo_owner}/${repo_name}/releases/download/${release_tag}/${archive_name}" \
    || die "cannot download ${archive_name}"
  curl --fail --location --retry 5 --retry-delay 2 --silent --show-error \
    --connect-timeout 15 --max-time 300 \
    -o "${temporary_dir}/checksums.txt" \
    "https://github.com/${repo_owner}/${repo_name}/releases/download/${release_tag}/checksums.txt" \
    || die "cannot download checksums.txt"

  checksums=""
  while read -r checksum_value checksum_file; do
    if [[ "${checksum_file}" == "${archive_name}" ]]; then
      checksums="${checksum_value}"
      break
    fi
  done <"${temporary_dir}/checksums.txt"
  [[ "${checksums}" =~ ^[0-9a-fA-F]{64}$ ]] || die "checksum entry for ${archive_name} is missing or invalid"
  read -r actual_checksum _ < <(sha256sum "${temporary_dir}/${archive_name}")
  [[ "${actual_checksum,,}" == "${checksums,,}" ]] || die "checksum verification failed for ${archive_name}"

  mkdir -p "${temporary_dir}/package"
  tar -xzf "${temporary_dir}/${archive_name}" -C "${temporary_dir}/package"
  [[ -f "${temporary_dir}/package/easyconnect" && -x "${temporary_dir}/package/easyconnect" ]] \
    || die "release archive does not contain a runnable easyconnect binary"
}

write_new_config() {
  local config_file="$1"
  local escaped_password
  local escaped_claude_binary
  local escaped_codex_binary
  local escaped_working_directory

  escaped_password="$(json_escape "${auth_password}")"
  escaped_claude_binary="$(json_escape "${claude_binary}")"
  escaped_codex_binary="$(json_escape "${codex_binary}")"
  escaped_working_directory="$(json_escape "${session_working_dir}")"

  cat >"${config_file}" <<JSON
{
  "server": {
    "port": ${listen_port},
    "host": "${listen_host}"
  },
  "auth": {
    "password": "${escaped_password}"
  },
  "database": {
    "path": "easyconnect.db"
  },
  "claude": {
    "binary": "${escaped_claude_binary}",
    "runAsUser": ""
  },
  "codex": {
    "binary": "${escaped_codex_binary}",
    "runAsUser": ""
  },
  "session": {
    "defaultAgent": "${default_agent}",
    "defaultWorkingDir": "${escaped_working_directory}"
  },
  "updates": {
    "enabled": true
  },
  "logging": {
    "path": "error.log",
    "maxSizeMB": 10
  }
}
JSON
}

write_systemd_unit() {
  local unit_file="$1"
  local writable_paths="${install_dir}"
  # The web updater is a narrowly scoped sudoers rule which starts only the
  # fixed root-owned updater unit. NoNewPrivileges would prevent that rule
  # from working, while the service account still receives no general sudo
  # access unless the separate AI-session option is enabled below.
  local no_new_privileges="false"
  local protect_system="strict"
  local protect_home="read-only"

  [[ "${session_working_dir}" == "${install_dir}" || " ${writable_paths} " == *" ${session_working_dir} "* ]] \
    || writable_paths="${writable_paths} ${session_working_dir}"
  [[ "${service_home}" == "${install_dir}" || " ${writable_paths} " == *" ${service_home} "* ]] \
    || writable_paths="${writable_paths} ${service_home}"
  if [[ "${allow_session_sudo}" == "yes" ]]; then
    no_new_privileges="false"
    protect_system="false"
    protect_home="false"
  fi

  cat >"${unit_file}" <<UNIT
[Unit]
Description=EasyConnect Go Web Panel
After=network.target

[Service]
Type=simple
User=${service_user}
Group=${service_group}
WorkingDirectory=${install_dir}
ExecStart=${install_dir}/easyconnect --config ${install_dir}/config.json
Restart=on-failure
RestartSec=5
UMask=0077
Environment=HOME=${service_home}
Environment=PATH=/usr/local/bin:/usr/bin:/bin
NoNewPrivileges=${no_new_privileges}
PrivateTmp=true
ProtectSystem=${protect_system}
ProtectHome=${protect_home}
ReadWritePaths=${writable_paths}

[Install]
WantedBy=multi-user.target
UNIT
}

write_update_unit() {
  local unit_file="$1"

  cat >"${unit_file}" <<UNIT
[Unit]
Description=GoEasyConnect web-triggered updater
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
TimeoutStartSec=15min
ExecStart=${update_helper_path}
UMask=0077
UNIT
}

install_update_support() {
  local package_helper="$1"
  local temporary_unit="$2"

  [[ -f "${package_helper}" && -x "${package_helper}" ]] || die "release package does not contain the updater helper"
  install_sudo_if_needed
  install -d -o root -g root -m 0755 "$(dirname "${update_helper_path}")"
  install -o root -g root -m 0755 "${package_helper}" "${update_helper_path}"
  write_update_unit "${temporary_unit}"
  install -o root -g root -m 0644 "${temporary_unit}" "/etc/systemd/system/${update_unit_name}"
  printf '%s ALL=(root) NOPASSWD: /usr/bin/systemctl start --no-block %s\n' "${service_user}" "${update_unit_name}" >"${temporary_unit}.sudoers"
  install -o root -g root -m 0440 "${temporary_unit}.sudoers" "${update_sudoers_path}"
}

backup_existing_file() {
  local path="$1"

  if [[ -e "${path}" ]]; then
    cp -a "${path}" "${path}.backup.$(date +%Y%m%d%H%M%S)"
  fi
}

chown_runtime_files() {
  local path

  for path in \
    "${install_dir}/config.json" \
    "${install_dir}/easyconnect.db" \
    "${install_dir}/easyconnect.db-wal" \
    "${install_dir}/easyconnect.db-shm" \
    "${install_dir}/easyclaude.db" \
    "${install_dir}/easyclaude.db-wal" \
    "${install_dir}/easyclaude.db-shm" \
    "${install_dir}/error.log"; do
    if [[ -e "${path}" ]]; then
      chown "${service_user}:${service_group}" "${path}"
    fi
  done
}

warn_if_agent_missing() {
  local agent="$1"
  local binary="$2"

  if ! runuser -u "${service_user}" -- env HOME="${service_home}" PATH="/usr/local/bin:/usr/bin:/bin" command -v "${binary}" >/dev/null 2>&1; then
    info "Warning: ${agent} command '${binary}' is not currently visible to ${service_user}."
    info "You can install it before creating a session, or reinstall with its absolute path."
  fi
}

read_unit_value() {
  local unit_file="$1"
  local key="$2"
  local line

  while IFS= read -r line; do
    case "${line}" in
      "${key}="*)
        printf '%s\n' "${line#*=}"
        return 0
        ;;
    esac
  done <"${unit_file}"
  return 1
}

detect_existing_installation() {
  local exec_start
  local fragment_path
  local argument
  local expect_config="no"

  fragment_path="$(systemctl show --property=FragmentPath --value "${service_name}.service")"
  [[ -n "${fragment_path}" && -f "${fragment_path}" ]] \
    || die "${service_name}.service is not installed; run the installer without 'upgrade' first"
  unit_file="${fragment_path}"

  service_user="$(read_unit_value "${unit_file}" User)"
  [[ -n "${service_user}" ]] || die "cannot determine the service user from ${unit_file}"
  getent passwd "${service_user}" >/dev/null 2>&1 || die "service user does not exist: ${service_user}"
  resolve_service_user_details

  exec_start="$(read_unit_value "${unit_file}" ExecStart)"
  [[ -n "${exec_start}" ]] || die "cannot determine ExecStart from ${unit_file}"
  read -r -a exec_arguments <<<"${exec_start}"
  ((${#exec_arguments[@]} > 0)) || die "ExecStart in ${unit_file} is empty"
  installed_binary="${exec_arguments[0]}"
  [[ "${installed_binary}" == /* && -x "${installed_binary}" ]] \
    || die "installed EasyConnect binary is missing or not executable: ${installed_binary}"
  install_dir="$(realpath -m "$(dirname "${installed_binary}")")"
  validate_install_dir "${install_dir}"

  config_path="${install_dir}/config.json"
  for argument in "${exec_arguments[@]:1}"; do
    if [[ "${expect_config}" == "yes" ]]; then
      config_path="${argument}"
      expect_config="no"
      continue
    fi
    case "${argument}" in
      --config)
        expect_config="yes"
        ;;
      --config=*)
        config_path="${argument#--config=}"
        ;;
    esac
  done
  [[ "${expect_config}" == "no" ]] || die "ExecStart contains --config without a path"
  [[ "${config_path}" == /* && -f "${config_path}" ]] \
    || die "installed EasyConnect config is missing: ${config_path}"
  config_path="$(realpath -m "${config_path}")"
}

remove_runtime_file() {
  local path="$1"

  if [[ -e "${path}" ]]; then
    unlink -- "${path}"
  fi
}

restore_database_backup() {
  local suffix
  local source
  local target

  for suffix in "" "-wal" "-shm"; do
    source="${backup_dir}/database/database${suffix}"
    target="${database_path}${suffix}"
    if [[ -e "${source}" ]]; then
      cp -a "${source}" "${target}"
    else
      remove_runtime_file "${target}"
    fi
  done
}

rollback_upgrade() {
  local reason="$1"

  upgrade_in_progress="no"
  service_stopped="no"
  trap - ERR HUP INT TERM
  set +e
  info "Upgrade failed: ${reason}"
  info "Rolling back the binary and database..."
  systemctl stop "${service_name}.service" >/dev/null 2>&1
  install -o root -g root -m 0755 "${backup_dir}/easyconnect" "${installed_binary}.rollback"
  mv -f "${installed_binary}.rollback" "${installed_binary}"
  restore_database_backup
  chown "${service_user}:${service_group}" "${database_path}" "${database_path}-wal" "${database_path}-shm" 2>/dev/null
  systemctl daemon-reload
  if [[ "${service_was_active}" == "yes" ]]; then
    systemctl start "${service_name}.service"
  fi
  set -e
  die "the previous version was restored; backup retained at ${backup_dir}"
}

handle_upgrade_failure() {
  local exit_status=$?

  trap - ERR HUP INT TERM
  if [[ "${upgrade_in_progress:-no}" == "yes" ]]; then
    rollback_upgrade "the installer exited unexpectedly (status ${exit_status})"
  fi
  if [[ "${service_stopped:-no}" == "yes" && "${service_was_active:-no}" == "yes" ]]; then
    systemctl start "${service_name}.service" >/dev/null 2>&1 || true
  fi
  exit "${exit_status}"
}

handle_upgrade_signal() {
  trap - ERR HUP INT TERM
  if [[ "${upgrade_in_progress:-no}" == "yes" ]]; then
    rollback_upgrade "the installer was interrupted"
  fi
  if [[ "${service_stopped:-no}" == "yes" && "${service_was_active:-no}" == "yes" ]]; then
    systemctl start "${service_name}.service" >/dev/null 2>&1 || true
  fi
  die "upgrade interrupted before the installed binary was changed"
}

upgrade_main() {
  local architecture
  local installed_version
  local suffix
  local source

  upgrade_in_progress="no"
  service_stopped="no"

  require_root
  require_debian

  for command_name in curl tar sha256sum install mktemp realpath systemctl getent runuser cp mv dirname unlink python3; do
    command -v "${command_name}" >/dev/null 2>&1 \
      || die "required command is missing: ${command_name}"
  done

  detect_existing_installation
  architecture="$(detect_architecture)"
  temporary_dir="$(mktemp -d)"
  trap 'rm -rf -- "${temporary_dir}"' EXIT HUP INT TERM
  trap handle_upgrade_failure ERR
  trap handle_upgrade_signal HUP INT TERM
  chmod 0700 "${temporary_dir}"
  download_latest_release "${architecture}"

  installed_version="$("${installed_binary}" --version 2>/dev/null || true)"
  if [[ "${installed_version}" == "${release_version}" ]]; then
    info "EasyConnect ${release_version} is already installed."
    return 0
  fi

  database_path="$(env HOME="${service_home}" \
    "${temporary_dir}/package/easyconnect" --print-database-path --config "${config_path}")" \
    || die "the new release cannot resolve the existing database path"
  [[ "${database_path}" == /* && "${database_path}" != *$'\n'* && "${database_path}" != *$'\r'* ]] \
    || die "the resolved database path is invalid"
  database_path="$(realpath -m "${database_path}")"

  if systemctl is-active --quiet "${service_name}.service"; then
    service_was_active="yes"
  else
    service_was_active="no"
  fi

  info "Stopping ${service_name}.service for a consistent backup..."
  systemctl stop "${service_name}.service" \
    || die "cannot stop ${service_name}.service"
  service_stopped="yes"

  backup_dir="${backup_root}/$(date +%Y%m%d%H%M%S)-${release_version}-$$"
  install -d -o root -g root -m 0700 "${backup_dir}/database" \
    || { [[ "${service_was_active}" != "yes" ]] || systemctl start "${service_name}.service"; die "cannot create ${backup_dir}"; }
  cp -a "${installed_binary}" "${backup_dir}/easyconnect" \
    || { [[ "${service_was_active}" != "yes" ]] || systemctl start "${service_name}.service"; die "cannot back up the installed binary"; }
  cp -a "${config_path}" "${backup_dir}/config.json" \
    || { [[ "${service_was_active}" != "yes" ]] || systemctl start "${service_name}.service"; die "cannot back up config.json"; }
  cp -a "${unit_file}" "${backup_dir}/easyconnect.service" \
    || { [[ "${service_was_active}" != "yes" ]] || systemctl start "${service_name}.service"; die "cannot back up the systemd unit"; }
  for suffix in "" "-wal" "-shm"; do
    source="${database_path}${suffix}"
    if [[ -e "${source}" ]]; then
      cp -a "${source}" "${backup_dir}/database/database${suffix}" \
        || { [[ "${service_was_active}" != "yes" ]] || systemctl start "${service_name}.service"; die "cannot back up ${source}"; }
    fi
  done
  upgrade_in_progress="yes"

  info "Installing EasyConnect ${release_version}..."
  install -o root -g root -m 0755 "${temporary_dir}/package/easyconnect" "${installed_binary}.new" \
    || rollback_upgrade "cannot stage the new binary"
  mv -f "${installed_binary}.new" "${installed_binary}" \
    || rollback_upgrade "cannot replace the installed binary"

  install_update_support "${temporary_dir}/package/update-helper.sh" "${temporary_dir}/goeasyconnect-updater.service"
  systemctl daemon-reload

  if ! runuser -u "${service_user}" -- env HOME="${service_home}" \
    "${installed_binary}" --check --config "${config_path}"; then
    rollback_upgrade "configuration or database validation failed"
  fi

  if [[ "${service_was_active}" == "yes" ]]; then
    systemctl start "${service_name}.service" \
      || rollback_upgrade "the upgraded service did not start"
    systemctl is-active --quiet "${service_name}.service" \
      || rollback_upgrade "the upgraded service is not active"
  fi
  upgrade_in_progress="no"
  service_stopped="no"
  trap - ERR HUP INT TERM

  info "EasyConnect was upgraded from ${installed_version:-an unversioned build} to ${release_version}."
  info "Backup: ${backup_dir}"
  if [[ "${service_was_active}" == "yes" ]]; then
    info "Service: systemctl status ${service_name}"
  else
    info "The service was inactive before the upgrade and remains inactive."
  fi
}

install_main() {
  local architecture
  local archive_name
  local reuse_config
  local existing_user="easyconnect"
  local generated_config

  require_root
  require_debian

  for command_name in curl tar sha256sum install mktemp realpath systemctl getent runuser python3; do
    command -v "${command_name}" >/dev/null 2>&1 \
      || die "required command is missing: ${command_name}"
  done

  architecture="$(detect_architecture)"

  if [[ -r /etc/systemd/system/${service_name}.service ]]; then
    existing_user="$(read_unit_value "/etc/systemd/system/${service_name}.service" User || true)"
    [[ -n "${existing_user}" ]] || existing_user="easyconnect"
  fi

  install_dir="$(prompt_value "Installation directory" "${default_install_dir}")"
  validate_install_dir "${install_dir}"
  install_dir="$(realpath -m "${install_dir}")"
  validate_install_dir "${install_dir}"

  select_service_user "${existing_user}"
  resolve_service_user_details
  session_working_dir="${service_home}"

  allow_session_sudo="$(confirm "Allow AI sessions started by this user to elevate with sudo?" yes && printf yes || printf no)"
  if [[ "${allow_session_sudo}" == "yes" ]]; then
    install_sudo_if_needed
    usermod --append --groups sudo "${service_user}"
    if [[ "${service_user_password_set:-no}" != "yes" ]]; then
      if confirm "Set or reset the login password for ${service_user} now?" no; then
        passwd "${service_user}"
      fi
    fi
    info "Warning: AI sessions with sudo access can make system-wide changes."
  fi

  if [[ -f "${install_dir}/config.json" ]]; then
    reuse_config="$(confirm "An existing config.json was found. Reuse it and preserve its database/log data?" yes && printf yes || printf no)"
  else
    reuse_config="no"
  fi

  if [[ "${reuse_config}" != "yes" ]]; then
    select_listen_host
    listen_port="$(prompt_value "Listen port" "${default_port}")"
    validate_port "${listen_port}"

    while true; do
      auth_password="$(prompt_password "Web access password")"
      [[ -n "${auth_password}" ]] || die "web access password must be set"
      [[ "${auth_password}" =~ [^[:space:]] ]] || die "web access password cannot contain only whitespace"
      [[ "${auth_password}" != "change-this-password" ]] || die "the example password is not allowed"
      break
    done

    while true; do
      default_agent="$(prompt_value "Default agent: claude or codex" "claude")"
      [[ "${default_agent}" == "claude" || "${default_agent}" == "codex" ]] || continue
      break
    done
    claude_binary="$(prompt_value "Claude CLI binary or path" "claude")"
    codex_binary="$(prompt_value "Codex CLI binary or path" "codex")"
    select_working_directory
  fi

  temporary_dir="$(mktemp -d)"
  trap 'rm -rf -- "${temporary_dir}"' EXIT HUP INT TERM
  chmod 0700 "${temporary_dir}"

  download_latest_release "${architecture}"

  if systemctl is-active --quiet "${service_name}.service"; then
    info "Stopping the existing service before replacing the binary..."
    systemctl stop "${service_name}.service"
  fi

  if [[ -d "${install_dir}" ]]; then
    backup_existing_file "${install_dir}/config.json"
    backup_existing_file "${install_dir}/easyconnect"
  fi

  install -d -o "${service_user}" -g "${service_group}" -m 0700 "${install_dir}"
  install -o root -g root -m 0755 "${temporary_dir}/package/easyconnect" "${install_dir}/easyconnect.new"
  mv -f "${install_dir}/easyconnect.new" "${install_dir}/easyconnect"
  install -o root -g root -m 0644 "${temporary_dir}/package/config.example.json" "${install_dir}/config.example.json"
  install_update_support "${temporary_dir}/package/update-helper.sh" "${temporary_dir}/goeasyconnect-updater.service"

  if [[ "${reuse_config}" == "yes" ]]; then
    info "Preserving existing ${install_dir}/config.json."
  else
    generated_config="${temporary_dir}/config.json"
    write_new_config "${generated_config}"
    if [[ -e "${install_dir}/config.json" ]]; then
      backup_existing_file "${install_dir}/config.json"
    fi
    install -o "${service_user}" -g "${service_group}" -m 0600 "${generated_config}" "${install_dir}/config.json"
  fi

  chown_runtime_files
  warn_if_agent_missing Claude "${claude_binary:-claude}"
  warn_if_agent_missing Codex "${codex_binary:-codex}"

  if ! runuser -u "${service_user}" -- env HOME="${service_home}" \
    "${install_dir}/easyconnect" --check --config "${install_dir}/config.json"; then
    die "EasyConnect configuration check failed; installation was stopped before service startup"
  fi

  write_systemd_unit "${temporary_dir}/easyconnect.service"
  backup_existing_file /etc/systemd/system/${service_name}.service
  install -o root -g root -m 0644 "${temporary_dir}/easyconnect.service" "/etc/systemd/system/${service_name}.service"
  systemctl daemon-reload
  systemctl enable --now "${service_name}.service"

  if ! systemctl is-active --quiet "${service_name}.service"; then
    systemctl status "${service_name}.service" --no-pager || true
    die "service did not start; inspect journalctl -u ${service_name} -e"
  fi

  info ""
  info "GoEasyConnect ${release_version} installed successfully."
  info "Configuration: ${install_dir}/config.json"
  info "Service user:  ${service_user}"
  info "Service:       systemctl status ${service_name}"
  if [[ "${reuse_config}" != "yes" ]]; then
    info "Web address:   http://${listen_host}:${listen_port}"
  fi
  info "API keys and Claude/Codex profiles can be configured in the web UI after login."
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  case "${1:-install}" in
    install)
      (($# <= 1)) || die "usage: install.sh [install|upgrade]"
      install_main
      ;;
    upgrade)
      (($# == 1)) || die "usage: install.sh [install|upgrade]"
      upgrade_main
      ;;
    *)
      die "usage: install.sh [install|upgrade]"
      ;;
  esac
fi
