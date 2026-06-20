#!/bin/sh
set -e

if [ -f .env ]; then
	. ./.env
fi

if [ -z "$RCLONE_CONF_BASE64" ]; then
	echo "ERROR: RCLONE_CONF_BASE64 is not set" >&2
	exit 1
fi

docker network create backup-net 2>/dev/null || true

docker run -d \
	--network backup-net \
	--name backio \
	--restart unless-stopped \
	-v backio-data:/data \
	-e RCLONE_CONF_BASE64="$RCLONE_CONF_BASE64" \
	-e PORT="${PORT:-8080}" \
	--health-cmd "wget -qO- http://localhost:${PORT:-8080}/health || exit 1" \
	--health-interval 30s \
	--health-timeout 5s \
	--health-retries 3 \
	backio:latest
