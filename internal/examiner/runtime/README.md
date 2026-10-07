# Examiner runtime

The examiner runtime is the locally built Docker image that runs Inspector's sealed agent.

Build it before running `inspector examine`:

```
internal/examiner/runtime/build.sh
```

The Dockerfile compiles the reviewed agent source with a pinned multi-architecture Go builder and copies it into a digest-pinned runtime base. The image is local only. Examination refuses if the image is absent instead of building or pulling while it holds app access and an API key.
