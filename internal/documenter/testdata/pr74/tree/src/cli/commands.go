package cli

// Register wires every entry point the command layer dispatches through.
func Register(d *Dispatcher) {
	d.Add("build", runBuild)
	d.Add("check", runCheck)
	d.Add("deploy", runDeploy)
	d.Add("migrate", runMigrate)
	d.Add("rollback", runRollback)
	d.Add("status", runStatus)
}
