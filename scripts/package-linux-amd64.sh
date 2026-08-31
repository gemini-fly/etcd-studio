#!/usr/bin/env bash
set -euo pipefail

script_directory=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repository_root=$(cd "$script_directory/.." && pwd)
output_directory="$repository_root/bin"
archive_name=etcd-studio-linux-amd64.tar.gz
archive_path="$output_directory/$archive_name"
stage_directory=$(mktemp -d)
package_root="$stage_directory/etcd-studio"

trap 'rm -rf "$stage_directory"' EXIT
mkdir -p "$package_root/deploy" "$output_directory"

(
  cd "$repository_root"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -trimpath -ldflags='-s -w' \
    -o "$package_root/etcd-studio" \
    ./cmd/etcd-studio
)
install -m 0644 "$repository_root/LICENSE" "$package_root/LICENSE"
install -m 0644 "$repository_root/README.md" "$package_root/README.md"
install -m 0644 "$repository_root/deploy/etcd-studio.env.example" "$package_root/deploy/etcd-studio.env.example"
install -m 0644 "$repository_root/deploy/etcd-studio.service" "$package_root/deploy/etcd-studio.service"

tar_options=(--format=ustar --no-xattrs)
if tar --version 2>&1 | grep -q 'bsdtar'; then
  tar_options+=(--no-mac-metadata --uid 0 --gid 0 --uname root --gname root)
else
  tar_options+=(--owner=0 --group=0)
fi

temporary_archive="$stage_directory/$archive_name"
COPYFILE_DISABLE=1 tar "${tar_options[@]}" -C "$stage_directory" -czf "$temporary_archive" etcd-studio
"$repository_root/scripts/verify-linux-amd64-package.sh" "$temporary_archive"

find "$output_directory" -maxdepth 1 -type f \
  \( -name 'etcd-studio-linux-amd64*.tar.gz' -o -name 'etcd-studio-linux-amd64*.sha256' \) -delete
find "$output_directory" -maxdepth 1 -mindepth 1 -type d \
  -name 'etcd-studio-linux-amd64-*' -exec rm -r -- {} +
mv "$temporary_archive" "$archive_path"

echo "package created: $archive_path"
