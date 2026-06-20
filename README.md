# backio

A Docker image that receives backup archives over HTTP and forwards them to any [rclone](https://rclone.org)-configured cloud provider (Google Drive, S3, Backblaze, etc.).

## How it works

1. Another container (or script) POSTs a tar archive to `POST /backup`
2. backio saves it to a temp file and runs `rclone copyto` to upload it
3. Returns `{"status":"ok","destination":"..."}` on success

List or delete backups via `GET /backup` and `DELETE /backup`.

## Environment variables

| Variable             | Required | Default | Description                  |
| -------------------- | -------- | ------- | ---------------------------- |
| `RCLONE_CONF_BASE64` | Yes      | —       | base64-encoded `rclone.conf` |
| `PORT`               | No       | `8080`  | HTTP listen port             |

## Endpoints

### `GET /backup`

Query parameters:

| Parameter      | Description                                      |
| -------------- | ------------------------------------------------ |
| `provider`     | rclone remote name (e.g. `gdrive`, `s3`)         |
| `subdirectory` | Remote path to list (e.g. `myapp/production`)    |

Returns the `rclone lsjson` output — a JSON array of file objects.

All requests require `Authorization: Bearer <token>` with `read` permission for the target provider and subdirectory.

**Responses:**

- `200` — JSON array (rclone lsjson format)
- `400` — missing or invalid parameters
- `401` — missing token
- `403` — token lacks permission
- `500` — rclone stderr verbatim

Example:

```sh
curl -H "Authorization: Bearer $BACKUP_TOKEN" \
  "http://backio:8080/backup?provider=gdrive&subdirectory=myapp/production"
```

```json
[
  {
    "Path": "myapp-20240115.tar",
    "Name": "myapp-20240115.tar",
    "Size": 1048576,
    "MimeType": "application/x-tar",
    "ModTime": "2024-01-15T10:30:00.000000000Z",
    "IsDir": false
  },
  {
    "Path": "myapp-20240116.tar",
    "Name": "myapp-20240116.tar",
    "Size": 1052672,
    "MimeType": "application/x-tar",
    "ModTime": "2024-01-16T10:30:00.000000000Z",
    "IsDir": false
  }
]
```

---

### `POST /backup`

Multipart form fields:

| Field          | Type   | Description                                          |
| -------------- | ------ | ---------------------------------------------------- |
| `backup`       | file   | The tar archive to upload                            |
| `name`         | string | Filename on the remote (e.g. `myapp-2024-01-15.tar`) |
| `subdirectory` | string | Remote path prefix (e.g. `myapp/production`)         |
| `provider`     | string | rclone remote name (e.g. `gdrive`, `s3`)             |

Uploads to: `provider:subdirectory/name`. Requires `Authorization: Bearer <token>` with `create` permission for the target provider and subdirectory.

**Responses:**

- `200` — `{"status":"ok","destination":"gdrive:myapp/production/myapp-2024-01-15.tar"}`
- `400` — missing or invalid fields
- `401` — missing token
- `403` — token lacks permission
- `500` — rclone stderr verbatim

---

### `DELETE /backup`

Query parameters:

| Parameter      | Description                                              |
| -------------- | -------------------------------------------------------- |
| `provider`     | rclone remote name (e.g. `gdrive`, `s3`)                 |
| `subdirectory` | Remote path prefix (e.g. `myapp/production`)             |
| `name`         | Filename to delete (e.g. `myapp-2024-01-15.tar`)         |

Deletes `provider:subdirectory/name` via `rclone deletefile`. Requires `Authorization: Bearer <token>` with `delete` permission for the target provider and subdirectory.

**Responses:**

- `200` — `{"status":"ok","deleted":"gdrive:myapp/production/myapp-2024-01-15.tar"}`
- `400` — missing or invalid parameters
- `401` — missing token
- `403` — token lacks permission
- `500` — rclone stderr verbatim

Example:

```sh
curl -X DELETE \
  -H "Authorization: Bearer $BACKUP_TOKEN" \
  "http://backio:8080/backup?provider=gdrive&subdirectory=myapp/production&name=myapp-20240115.tar"
```

## Access control

Every request requires an `Authorization: Bearer <token>` header. Tokens are issued via the CLI and stored in `/data/tokens.json` inside the container.

Each token carries one or more grants. A grant specifies a provider, a subdirectory, and a comma-separated list of permissions (`create`, `read`, `delete`). A request is allowed if any grant on the token matches the requested provider, subdirectory, and operation exactly.

**Issue a token:**

```sh
docker exec backio /backio issue-token \
  "gdrive myapp/production create,read" \
  "gdrive myapp/production delete"
```

Prints the token to stdout. Multiple grant strings can be passed as separate arguments.

**List tokens:**

```sh
docker exec backio /backio list-tokens
```

**Persistent storage:** mount a volume at `/data` so tokens survive container restarts:

```sh
docker run -d \
  --network backup-net \
  --name backio \
  -v backio-data:/data \
  -e RCLONE_CONF_BASE64="$RCLONE_CONF_BASE64" \
  ghcr.io/reeywhaar/backio:latest
```

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
  -v backio-data:/data \
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
      BACKUP_TOKEN: "<token issued via docker exec backio /backio issue-token>"
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
# Env vars: BACKUP_URL, BACKUP_NAME, BACKUP_SUBDIRECTORY, BACKUP_PROVIDER, BACKUP_TOKEN
tar cf /tmp/backup.tar /data
send-backup /tmp/backup.tar
rm /tmp/backup.tar
```

Or call directly:

```sh
curl -X POST http://backio:8080/backup \
  -H "Authorization: Bearer $BACKUP_TOKEN" \
  -F "backup=@/tmp/backup.tar" \
  -F "name=myapp-$(date +%Y%m%d_%H%M%S).tar" \
  -F "subdirectory=myapp/production" \
  -F "provider=gdrive"
```
