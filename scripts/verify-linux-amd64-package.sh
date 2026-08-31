#!/usr/bin/env bash
set -euo pipefail

archive_path=${1:?usage: verify-linux-amd64-package.sh <archive.tar.gz>}

if [[ ! -f "$archive_path" ]]; then
  echo "package does not exist: $archive_path" >&2
  exit 1
fi

expected_entries=$(printf '%s\n' \
  'etcd-studio/' \
  'etcd-studio/etcd-studio' \
  'etcd-studio/LICENSE' \
  'etcd-studio/README.md' \
  'etcd-studio/deploy/' \
  'etcd-studio/deploy/etcd-studio.env.example' \
  'etcd-studio/deploy/etcd-studio.service' | LC_ALL=C sort)
actual_entries=$(tar -tzf "$archive_path" | LC_ALL=C sort)
if [[ "$actual_entries" != "$expected_entries" ]]; then
  echo "package layout must use one stable etcd-studio/ root" >&2
  diff -u <(printf '%s\n' "$expected_entries") <(printf '%s\n' "$actual_entries") || true
  exit 1
fi

if gzip -dc "$archive_path" | LC_ALL=C grep -aEq 'LIBARCHIVE\.xattr|SCHILY\.xattr|com\.apple\.|(^|/)\._'; then
  echo "package contains macOS extended metadata" >&2
  exit 1
fi

extract_directory=$(mktemp -d)
trap 'rm -rf "$extract_directory"' EXIT
tar -xzf "$archive_path" -C "$extract_directory"

binary_description=$(file "$extract_directory/etcd-studio/etcd-studio")
if [[ "$binary_description" != *"ELF 64-bit"* || "$binary_description" != *"x86-64"* || "$binary_description" != *"statically linked"* ]]; then
  echo "package binary is not a static Linux amd64 executable: $binary_description" >&2
  exit 1
fi

echo "package verified: $archive_path"
