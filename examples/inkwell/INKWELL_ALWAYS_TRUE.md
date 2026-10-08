# Inkwell always-true list

These are Inkwell backend properties that must remain true while the examiner drives nearby scenarios.

- An inkling id is a UUID and is never accepted with a path separator.
- Required capture fields are non-empty id and text plus RFC 3339 created and updated timestamps.
- `POST /inklings` is an upsert by id: retrying an id updates one record rather than creating a duplicate.
- `GET /inklings` returns JSON records ordered newest created timestamp first.
- Invalid capture input returns `400`; backend failures return `5xx` and leave the caller able to retry.
