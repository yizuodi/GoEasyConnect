#!/usr/bin/env bash

set -Eeuo pipefail

repo_owner="yizuodi"
repo_name="GoEasyConnect"
service_name="easyconnect"
default_install_dir="/srv/GoEasyConnect"
default_port="26890"

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

  info "Finding the latest GitHub Release..."
  release_json="$(curl --fail --location --silent --show-error \
    --connect-timeout 15 --max-time 120 \
    -H 'Accept: application/vnd.github+json' \
    "https://api.github.com/repos/${repo_owner}/${repo_name}/releases/latest")" \
    || die "cannot query the latest GitHub Release"

  release_tag="$(printf '%s\n' "${release_json}" | sed -n 's/^  "tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)"
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

  checksums="$(awk -v file="${archive_name}" '$2 == file {print $1}' "${temporary_dir}/checksums.txt")"
  [[ "${checksums}" =~ ^[0-9a-fA-F]{64}$ ]] || die "checksum entry for ${archive_name} is missing or invalid"
  actual_checksum="$(sha256sum "${temporary_dir}/${archive_name}" | awk '{print $1}')"
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
  local no_new_privileges="true"
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

main() {
  local architecture
  local archive_name
  local reuse_config
  local existing_user="easyconnect"
  local generated_config

  require_root
  require_debian

  for command_name in curl tar sha256sum awk install mktemp realpath systemctl getent runuser; do
    command -v "${command_name}" >/dev/null 2>&1 \
      || die "required command is missing: ${command_name}"
  done

  architecture="$(detect_architecture)"

  if [[ -r /etc/systemd/system/${service_name}.service ]]; then
    existing_user="$(awk -F= '$1 == "User" {print $2; exit}' /etc/systemd/system/${service_name}.service)"
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

main "$@"
