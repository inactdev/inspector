# The command layer

Commands are registered in `commands.go`, one entry point each:

- `runBuild` - compiles the project
- `runCheck` - runs the project's checks
- `runDeploy` - ships a built artifact
- `runMigrate` - applies pending schema migrations
- `runRollback` - undoes the last migration
- `runStatus` - prints what is deployed where
