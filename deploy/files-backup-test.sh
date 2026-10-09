#!/bin/sh
# Не требует Docker: проверяет содержимое, ротацию и атомарную публикацию.
set -eu
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
trap 'exit 1' HUP INT TERM
mkdir "$fixture/files" "$fixture/backups" "$fixture/restored"
printf attachment > "$fixture/files/0123456789abcdef0123456789abcdef"
printf partial > "$fixture/files/pending.tmp"
printf old > "$fixture/backups/files-20000101T000000Z.tar.gz"
printf old > "$fixture/backups/files-20000102T000000Z.tar.gz"
export FILES_DIR="$fixture/files" FILES_BACKUP_DIR="$fixture/backups" FILES_BACKUP_KEEP=2
sh "$script_dir/files-backup.sh"
test "$(find "$FILES_BACKUP_DIR" -name '*.tar.gz' | wc -l)" -eq 2
archive=$(find "$FILES_BACKUP_DIR" -name '*.tar.gz' | sort | tail -1)
tar -xzf "$archive" -C "$fixture/restored"
test "$(cat "$fixture/restored/0123456789abcdef0123456789abcdef")" = attachment
test ! -e "$fixture/restored/pending.tmp"
FILES_DIR="$fixture/missing"
export FILES_DIR
if sh "$script_dir/files-backup.sh" >/dev/null 2>&1; then
    echo 'backup of missing directory unexpectedly succeeded' >&2
    exit 1
fi
test "$(find "$FILES_BACKUP_DIR" -name '*.tar.gz' | wc -l)" -eq 2
test -z "$(find "$FILES_BACKUP_DIR" -name '.files-*')"
echo 'files backup: round-trip, rotation and failed publication passed'
