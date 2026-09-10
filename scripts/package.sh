#!/bin/sh

set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
version=${VERSION:-dev}
output_dir=${OUTPUT_DIR:-"$repo_dir/dist"}
architectures=${ARCHES:-"amd64 arm64"}
archives=""

case "$version" in
  *[!A-Za-z0-9._-]*|'')
    echo "VERSION may contain only letters, numbers, dots, underscores, and hyphens" >&2
    exit 1
    ;;
esac

stage_root=$(mktemp -d)
trap 'rm -rf -- "$stage_root"' EXIT HUP INT TERM
mkdir -p "$output_dir"

for architecture in $architectures; do
  case "$architecture" in
    amd64|arm64) ;;
    *)
      echo "unsupported architecture: $architecture" >&2
      exit 1
      ;;
  esac

  archive="GoEasyConnect_${version}_linux_${architecture}.tar.gz"
  stage="$stage_root/GoEasyConnect_${version}_linux_${architecture}"
  mkdir -p "$stage"

  (
    cd "$repo_dir"
    CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" \
      go build -buildvcs=false -trimpath -ldflags="-s -w" \
      -o "$stage/easyconnect" .
  )
  chmod 0755 "$stage/easyconnect"
  cp "$repo_dir/config.example.json" "$stage/"
  cp "$repo_dir/easyconnect.service.example" "$stage/"
  cp "$repo_dir/README.md" "$stage/"
  cp "$repo_dir/LICENSE" "$stage/"
  cp "$repo_dir/THIRD_PARTY_NOTICES.md" "$stage/"
  tar -C "$stage" -czf "$output_dir/$archive" .
  archives="$archives $archive"
done

(
  cd "$output_dir"
  # Version and architecture values are restricted above, so word splitting is
  # deliberate here and cannot expand user-controlled shell metacharacters.
  sha256sum $archives > checksums.txt
)

printf 'Release packages written to %s\n' "$output_dir"
