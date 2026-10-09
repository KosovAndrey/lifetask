#!/bin/sh
# Один снимок локальных вложений; публикация только после успешного tar/gzip.
set -eu

files_dir=${FILES_DIR:-/data/files}
backup_dir=${FILES_BACKUP_DIR:-/backups/files}
keep=${FILES_BACKUP_KEEP:-30}
case "$keep" in ''|*[!0-9]*|0) echo 'FILES_BACKUP_KEEP must be a positive integer' >&2; exit 1;; esac

umask 022
mkdir -p "$backup_dir"
archive="$backup_dir/files-$(date -u +%Y%m%dT%H%M%SZ).tar.gz"
tmp=$(mktemp "$backup_dir/.files-XXXXXX")
trap 'rm -f "$tmp"' EXIT
trap 'exit 1' HUP INT TERM
tar -czf "$tmp" --exclude='*.tmp' -C "$files_dir" .
gzip -t "$tmp"
chmod 644 "$tmp"
mv "$tmp" "$archive"

# Имена содержат время UTC; скрытые временные файлы не участвуют в ротации.
find "$backup_dir" -maxdepth 1 -type f -name 'files-*.tar.gz' | sort -r |
    tail -n +$((keep + 1)) | while IFS= read -r old; do rm -f "$old"; done
echo "files backup: $archive"
