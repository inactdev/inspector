# Inkwell examiner guidebook

This guidebook lets the examiner judge Inkwell's backend through HTTP without reading Inkwell source.

## Coverage

This first guidebook covers the Go backend's HTTP interface. It does not drive the native iOS app, Simulator, microphone, speech recognition, or SwiftUI screens. A backend request is a real app driver for the covered half, but it is not evidence that the iOS half works.

## Start the app outside the examiner boundary

The caller starts Inkwell before launching `inspector examine`; the examiner never builds or starts the project. From a clean Inkwell checkout, the normal command is:

```
./dev.sh up -d
./dev.sh info
```

`dev.sh info` reports the worktree-specific backend address. Give that running address to the examiner as `--app-url`. The examiner container must be on a Docker network that can reach it. On Docker Desktop, a backend listening on the host can normally be addressed through `host.docker.internal`; when the backend and examiner share a Docker network, use the backend service name and port on that network.

For a backend-only run in one worktree, start it with a new storage directory and an address reachable from the examiner network:

```
cd backend
go run . --addr 0.0.0.0:8080 --storage-dir /tmp/inkwell-examiner-storage
```

Do not reuse production or personal Inkwell storage. The examiner creates and updates records while judging.

## Data shape

An inkling has a UUID `id`, UTC RFC 3339 `created` and `updated` timestamps, non-empty UTF-8 `text`, and a response-only `hasAudio` boolean. List responses are JSON arrays ordered newest `created` first.

## Drive the backend

Create or update an inkling with `POST /inklings`. Send `multipart/form-data` with required text parts `id`, `created`, `updated`, and `text`; `audio` is optional. A new id returns `201 Created` with JSON containing that id and `syncedAt`. The same id is an upsert and returns `200 OK`; it updates the existing record instead of creating another one.

For a tool that needs a literal multipart request, use a boundary such as `examiner-boundary`, set `Content-Type: multipart/form-data; boundary=examiner-boundary`, and put each text field between `--examiner-boundary` lines with its `Content-Disposition: form-data; name="..."` header. Finish with `--examiner-boundary--`.

Read records with `GET /inklings`. It returns `200 OK` and the JSON array of stored inklings. Use it to verify a create, an update, ordering, and that retrying an id did not make a duplicate.

Malformed input is observable behavior: missing required parts, empty text, malformed timestamps, and a non-UUID id return `400 Bad Request`. A body over 64 MiB returns `413`, but do not send oversized bodies in ordinary examiner runs.

## Safe scenarios

Use fresh UUIDs per examination. At minimum, exercise creating a record, listing it, sending the same id with changed text and timestamp, then listing again to verify one updated record. When the request implies validation behavior, send malformed input separately and verify the documented error. Preserve enough response evidence to name the missing capability rather than reporting a generic failure.

## Known limits

The backend writes markdown and commits it to its configured storage repository. That storage effect is outside this guidebook's observation surface. The examiner may observe the HTTP contract only. If the running backend cannot be reached, report the affected request-derived outcome as `could_not_be_tested`, not as a backend failure.
