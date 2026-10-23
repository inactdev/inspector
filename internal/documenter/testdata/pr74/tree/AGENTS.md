# Project agent memory

## The command layer

Every command is dispatched through one of the entry points registered in
`src/cli/commands.go`. There are four of them: `runBuild`, `runCheck`,
`runDeploy`, and `runStatus`. A command that needs a fifth one is a design
discussion, not a patch.

Read `src/cli/README.md` before adding one.
