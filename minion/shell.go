package main

// cmdLine is cmd.exe's command line for command: the shell's path quoted,
// /d (no AutoRun commands: nothing the host's registry adds runs), /s /c and
// the command in quotes, which /s strips, so the command reaches the shell
// as typed, its own quotes and all.
func cmdLine(comspec, command string) string {
	return `"` + comspec + `" /d /s /c "` + command + `"`
}
