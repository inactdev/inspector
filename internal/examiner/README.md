# Examiner

The examiner is Inspector's independent HTTP outcome judge.

It receives three copied files: the request, a guidebook for operating an already-running app, and Fabrica's base-diffed test-change list. It derives scenarios from the request, calls the app through a constrained HTTP driver, and emits a verdict for every claimed capability. It never mounts the judged repository, its implementation diff, or a parent directory containing either.

The test-change list is the one source exception. Its strict format admits only `*_test.*` files and their before and after content. A confirmed weakening carries a proposed regression line; the examiner never changes the project's tests or any other project file.

The outer command starts the app. The examiner only receives its URL, so it cannot build the app or read the code used to start it. The first version drives HTTP backends. It does not claim to drive native iOS screens.

The sealed agent calls Anthropic's Messages API with `ANTHROPIC_API_KEY`. Its only runtime tool is an HTTP request constrained to the supplied app URL and redirects cannot leave that app's origin. Inspector pins the Alpine runtime by digest and forces its own executable as the container entrypoint; callers cannot substitute a project image that may contain application source.
