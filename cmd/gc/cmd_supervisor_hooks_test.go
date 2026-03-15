package main

import "io"

func init() {
	ensureSupervisorRunningHook = func(stdout, stderr io.Writer) int { return 0 }
	reloadSupervisorHook = func(stdout, stderr io.Writer) int { return 0 }
	supervisorAliveHook = func() int { return 0 }
}
