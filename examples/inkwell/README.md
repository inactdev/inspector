This directory contains the hand-written Inkwell feature map and always-true list used by Inspector's first examiner run.

They are operating documents for the sealed examiner, not application source or generated artifacts. `inspector guidebook` (issue #22) now generates a feature map for an arbitrary project; #24 will produce always-true lists. These committed examples establish both input shapes.

A hand-written feature map like this one carries no `confirmed` front-matter line, so the examiner treats it as a document a human wrote and committed and judges against it exactly as before. A generated guidebook is the other case: until somebody dates and signs its `confirmed` line, every capability driven from it is recorded as `could_not_be_tested`. `examples/inspector/` is a generated one, left unconfirmed.
