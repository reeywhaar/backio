# backio-agent

The backup sidecar. Your service posts an archive to it; it forwards that to backio,
holds the credential, names the file, optionally encrypts it, and prunes old copies.

```
myapp ──POST /backup──▶ backio-agent ──POST /backup──▶ backio ──▶ Drive, S3, B2, …
        (no credential)                 (Bearer token)
```

It speaks backio's own upload protocol, so a service already posting to backio only has
to change one URL. What it adds is everything each project would otherwise reimplement:
the token, the timestamped naming, encryption, and a retention policy applied to the
remote and to local copies alike.

Your service decides *when* to back up and *what* goes in the archive. The agent decides
nothing about the contents and everything about what happens to them afterwards.

## Quick start

Issue a token scoped to where this project's backups will live:

```sh
docker exec backio /backio issue-token "gdrive myapp/production create,read,delete"
```

Then put the sidecar next to your service:

```yaml
services:
  myapp:
    image: myapp:latest
    environment:
      BACKUP_URL: http://backup:8080/backup # post your archive here
    networks: [default]

  backup:
    image: ghcr.io/reeywhaar/backio-agent:latest
    restart: unless-stopped
    environment:
      BACKIO_HOST: http://backio:8080 # scheme included
      BACKIO_PROVIDER: gdrive # rclone remote name
      BACKIO_SUBDIRECTORY: myapp/production # where it lands on that remote
      BACKIO_TOKEN: "<token issued above>"
    networks: [default, backup-net] # default reaches myapp, backup-net reaches backio

networks:
  backup-net:
    external: true # whichever network backio itself is on
```

Your service posts whenever it decides to — hourly, nightly, after a migration:

```sh
tar czf /tmp/backup.tgz /data
curl -sf -X POST "$BACKUP_URL" -F "backup=@/tmp/backup.tgz" -F "name=myapp.tgz"
```

`myapp-production-20260903_041500.tgz` appears in `gdrive:myapp/production`, and anything
the retention policy no longer wants is deleted from the remote.

Note what the service does **not** have: no token, no provider, no subdirectory, no
knowledge of where its backups end up. Only the sidecar has those. A compromised app
container cannot read, overwrite or delete a single existing backup.

## API

### `POST /backup`

No authentication — the sidecar is reachable only from your project's own network, which
is why the token lives here and not in the app.

Two request shapes are accepted:

```sh
# Multipart, exactly as backio itself takes it
curl -X POST http://backup:8080/backup -F "backup=@/tmp/backup.tgz" -F "name=myapp.tgz"

# Or a raw body, for anything without multipart tooling
curl -X POST --data-binary @/tmp/dump.sql.gz "http://backup:8080/backup?name=dump.sql.gz"
```

The `name` is read for its **extension only** — the agent always assigns its own
timestamped filename (see [Naming](#naming)). `subdirectory` and `provider` fields are
ignored if sent: where archives land is the sidecar's decision, not the caller's.

**Responses:**

- `200` — `{"status":"ok","destination":"gdrive:myapp/production/myapp-production-20260903_041500.tgz"}`
- `400` — no archive in the request, or an empty one
- `500` — backio rejected the upload; the body carries its status and message

A failed forward is reported as a failure, so your service can log or retry it. It is
never answered with `ok`.

### `GET /health`

`{"status":"ok","last_backup":"2026-09-03T04:15:00Z"}`, plus `last_error` if the most
recent attempt failed. Returns `503` when `BACKUP_EXPECT_EVERY` is set and nothing has
arrived in twice that window. This is what the image's `HEALTHCHECK` calls.

## Environment variables

**Destination** — where archives go. **All four, or none of them.** All four is the normal
deployment. None of them is the other real configuration: a volume mounted at `/backups`
and no remote at all, described under [Local copies](#local-copies).

Anything in between is refused at startup, because none of the four can be guessed on your
behalf, and the failure that would produce is the worst kind — an agent that starts clean,
accepts every archive, answers `ok`, and uploads nothing until the day you go looking for a
backup.

| Variable              | Description                                                     |
| --------------------- | --------------------------------------------------------------- |
| `BACKIO_HOST`         | Where backio is, **including the scheme**: `http://backio:8080`  |
| `BACKIO_PROVIDER`     | rclone remote name, e.g. `gdrive`, `s3`                          |
| `BACKIO_SUBDIRECTORY` | Remote path, e.g. `myapp/production`                             |
| `BACKIO_TOKEN`        | Token from `backio issue-token`                                  |

The scheme is required rather than assumed: defaulting to `http` would silently send a
bearer token in plaintext to anything that is not a compose-network hostname. A trailing
`/backup` is trimmed, so pasting the endpoint from the root README works.

A half-set group is reported with every missing name listed at once — a sidecar is
configured once, and hunting one variable per restart is the slow way to do it.

**The archive** — what happens to it on the way through. All optional.

| Variable              | Default               | Description                                                        |
| --------------------- | --------------------- | ------------------------------------------------------------------ |
| `BACKUP_PREFIX`       | from the subdirectory | Archive name prefix: `myapp/production` → `myapp-production-…`      |
| `BACKUP_EXTENSION`    | from the upload       | Force an extension, e.g. `tar.zst`, instead of taking it from `name` |
| `BACKUP_PASSWORD`     | —                     | If set, archives are repacked as AES-256 zip before upload           |
| `BACKUP_DIR`          | `/backups` if mounted | Keep local copies here as well as forwarding                        |
| `BACKUP_EXPECT_EVERY` | —                     | How often you expect archives, e.g. `6h`. Enables the staleness check |
| `UPLOAD_TIMEOUT`      | `30m`                 | How long the upload to backio may take                               |
| `PORT`                | `8080`                | HTTP listen port                                                     |

**Retention** — how many archives survive each upload. See [Retention](#retention).

| Variable            | Default | Description                                   |
| ------------------- | ------- | --------------------------------------------- |
| `RETENTION_TODAY`   | `3`     | Newest archives kept from the most recent day |
| `RETENTION_DAILY`   | `3`     | Days that keep their newest archive           |
| `RETENTION_WEEKLY`  | `2`     | ISO weeks that keep their newest archive      |
| `RETENTION_MONTHLY` | `2`     | Months that keep their newest archive         |

## Naming

Archives are named `<prefix>-<UTC timestamp>.<extension>`:

```
myapp-production-20260903_041500.tgz
```

The prefix defaults to `BACKIO_SUBDIRECTORY` with slashes turned into dashes, so a file
downloaded from the remote still says which project and environment it came from.
Override it with `BACKUP_PREFIX`.

The extension comes from the `name` you post (`dump.sql.gz` → `.sql.gz`, both halves
kept), or `BACKUP_EXTENSION` if you would rather state it, or `.tgz` if neither says.

The timestamp is the agent's own, in UTC. It is not taken from the name you post, because
retention can only prune archives it can date, and a name chosen by the service carries no
promise of a parseable one.

## Retention

With the defaults, seven archives survive however often you post: **three from today, the
newest of each of the three most recent days, the newest of the previous week, and the
newest of the previous month.** Hourly backups do not accumulate; the pool is bounded from
the first upload.

The slots count *buckets that contain archives*, not calendar time. The newest week bucket
already holds today's archive, so `RETENTION_WEEKLY: 2` means "one week-old copy" — which
is why the defaults are 2 and not 1. Setting a slot to `0` disables it; `RETENTION_TODAY`
and `RETENTION_DAILY` cannot both be `0`, since that would delete the archive just posted.

The policy runs against the remote after every successful upload, and against the local
directory if you mounted one. Archives whose names it does not recognise are never
deleted — a subdirectory shared with another project's backups is a configuration mistake,
not a licence to prune them.

If the token can create but not list or delete, remote pruning is skipped with a log line
rather than failing the upload.

## Encryption

Set `BACKUP_PASSWORD` and each archive is repacked as an AES-256 zip before it leaves the
container, so what reaches the provider is unreadable without the password:

```yaml
environment:
  BACKUP_PASSWORD: ${BACKUP_PASSWORD:?}
```

Archives become `.zip`. Restore with any 7-Zip:

```sh
docker exec backio /backio download gdrive myapp/production myapp-production-20260903_041500.zip > backup.zip
7z x -p"$BACKUP_PASSWORD" backup.zip
```

Turning the password on or off later is safe — retention recognises archives in both
forms, so the ones taken under the old setting are still pruned rather than piling up.

## Local copies

Mount a volume at `/backups` and the agent keeps a copy there too, under the same
retention policy — useful when you would rather not wait on the provider to restore, and
the copy is kept even when the upload fails:

```yaml
volumes:
  - myapp-backups:/backups
```

The image deliberately does not create `/backups`, so the directory exists exactly when
something is mounted over it. With no volume, archives are forwarded and deleted rather
than accumulating in the container's writable layer.

With a volume and **no** `BACKIO_` variables at all, the agent runs local-only: archives
are kept on the host, nothing is uploaded, and retention still applies to what it keeps.

What is refused is *neither* — no volume and no destination leaves the agent accepting
every archive, writing it to a directory that dies with the container, and deleting it,
while the service posting believes it is backed up. That is a startup error:

```
no /backups directory and no BACKIO_ destination: mount a volume at /backups for local
copies, or set all four BACKIO_ variables to forward to backio
```

## Health

```yaml
environment:
  BACKUP_EXPECT_EVERY: 6h # you post every six hours
```

The container then goes unhealthy after twice that window with nothing arriving, which
turns "backups stopped working in March" into something `docker ps` says in March:

```
$ docker ps
CONTAINER ID   IMAGE                 STATUS
a1b2c3d4e5f6   backio-agent:latest   Up 3 weeks (unhealthy)
```

Without it, the healthcheck only proves the agent is serving — it has no way to know how
often you intended to back up. Every step logs one JSON line to stderr, in backio's own
format.

## Build

```sh
./agent/build.sh
```

Or directly, from the repository root — the build needs `go.mod` and `internal/`, so the
context is the root and the Dockerfile is named explicitly:

```sh
docker build -f agent/Dockerfile -t backio-agent:latest .
```
