#!/bin/sh
set -eu
files-backup.sh
# После ночного дампа БД. TZ=Europe/Moscow задаётся в compose.
printf '%s\n' '10 3 * * * /usr/local/bin/files-backup.sh >/proc/1/fd/1 2>/proc/1/fd/2' > /etc/crontabs/root
exec crond -f -l 2 -L /dev/stdout
