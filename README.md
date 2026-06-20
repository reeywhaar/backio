# backio

A Docker image that receives backup archives over HTTP and forwards them to any [rclone](https://rclone.org)-configured cloud provider (Google Drive, S3, Backblaze, etc.).

## How it works

1. Another container (or script) POSTs a tar archive to `POST /backup`
2. backio saves it to a temp file and runs `rclone copyto` to upload it
3. Returns `{"status":"ok","destination":"..."}` on success

## Environment variables

| Variable             | Required | Default | Description                  |
| -------------------- | -------- | ------- | ---------------------------- |
| `RCLONE_CONF_BASE64` | Yes      | —       | base64-encoded `rclone.conf` |
| `PORT`               | No       | `8080`  | HTTP listen port             |

## Endpoint

### `POST /backup`

Multipart form fields:

| Field          | Type   | Description                                          |
| -------------- | ------ | ---------------------------------------------------- |
| `backup`       | file   | The tar archive to upload                            |
| `name`         | string | Filename on the remote (e.g. `myapp-2024-01-15.tar`) |
| `subdirectory` | string | Remote path prefix (e.g. `myapp/production`)         |
| `provider`     | string | rclone remote name (e.g. `gdrive`, `s3`)             |

Uploads to: `provider:subdirectory/name`

**Responses:**

- `200` — `{"status":"ok","destination":"gdrive:myapp/production/myapp-2024-01-15.tar"}`
- `400` — missing or invalid fields (one error per line)
- `500` — rclone stderr verbatim

## Setup: Google Drive

Run the interactive setup to obtain `RCLONE_CONF_BASE64`:

```sh
./setup-gdrive.sh
```

This builds a temporary container, walks you through the OAuth flow, and prints the base64 value to paste into your `.env`.

## Image

Published to GitHub Container Registry on every `v*` tag:

```sh
docker pull ghcr.io/reeywhaar/backio:latest
```

## Build & run

To build locally from source:

```sh
# Build (forces linux/amd64 for Apple Silicon compatibility)
./build.sh

# Run (reads RCLONE_CONF_BASE64 from env or .env file)
./run.sh
```

`run.sh` starts the container on the `backup-net` Docker network (created automatically if it doesn't exist).

## Multi-container setup

backio is designed to run alongside other containers. Connect them via a shared user-defined network so they can reach each other by container name:

```sh
# One-time
docker network create backup-net

# Run backio
docker run -d \
  --network backup-net \
  --name backio \
  -e RCLONE_CONF_BASE64="$RCLONE_CONF_BASE64" \
  backio:latest

# Connect an already-running container
docker network connect backup-net <container-name>
```

The client container calls `http://backio:8080/backup` — no published ports needed.

### Docker Compose

backio runs globally as a standalone container (via `run.sh`). Individual projects just join the existing network:

```yaml
services:
  myapp:
    image: myapp:latest
    environment:
      BACKUP_URL: http://backio:8080/backup
      BACKUP_PROVIDER: gdrive
      BACKUP_SUBDIRECTORY: myapp/production
    networks:
      - backup-net

networks:
  backup-net:
    external: true # created by run.sh, shared across projects
```

## send-backup.sh

A standalone client script. Copy it into other Docker images to send backups:

```dockerfile
COPY send-backup.sh /usr/local/bin/send-backup
```

Usage:

```sh
# Env vars: BACKUP_URL, BACKUP_NAME, BACKUP_SUBDIRECTORY, BACKUP_PROVIDER
tar cf /tmp/backup.tar /data
send-backup /tmp/backup.tar
rm /tmp/backup.tar
```

Or call directly:

```sh
curl -X POST http://backio:8080/backup \
  -F "backup=@/tmp/backup.tar" \
  -F "name=myapp-$(date +%Y%m%d_%H%M%S).tar" \
  -F "subdirectory=myapp/production" \
  -F "provider=gdrive"
```
