This directory contains the hand-written Inkwell feature map and always-true list used by Inspector's first examiner run.

They are operating documents for the sealed examiner, not application source or generated artifacts. `inspector guidebook` (issue #22) now generates a feature map for an arbitrary project, and check 3 (issue #24) drives always-true lists. These committed examples establish both input shapes.

A hand-written feature map like this one carries no `confirmed` front-matter line, so the examiner treats it as a document a human wrote and committed and judges against it exactly as before. A generated guidebook is the other case: until somebody dates and signs its `confirmed` line, every capability driven from it is recorded as `could_not_be_tested`. `examples/inspector/` is a generated one, left unconfirmed.

`inkwell-always.json` is the always-true list in check 3's format (issue #24), and it replaces the earlier prose `INKWELL_ALWAYS_TRUE.md`: the invariants document and the examiner's `--always-true` input are the same artifact, so there is one file. Each line carries the plain-language promise and the console command, driver script, or HTTP call that would falsify it. `inspector always` drives them; `inspector examine --always-true` reads the same file for context while it hunts nearby. Three of its lines deliberately carry no falsifier and say why in their own note, because a promise nothing can currently disprove has to read as not-runnable rather than as passing.

A project's real list lives at `.inspector-always.json` in its own repository, under `protectedPaths`. This one is an example, kept beside the feature map because Inspector's own documentation and tests refer to it.
