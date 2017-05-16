// +build !windows

package main

import (
	"fmt"
	"log/syslog"
	"os"
)

// ShellPath points to a Bourne-compatible shell.
// /bin/sh is the standard path that should work on any Unix.
const ShellPath string = "/bin/sh"

var logs *syslog.Writer

func logInit() (*syslog.Writer, error) {
	return syslog.New(syslog.LOG_INFO|syslog.LOG_DAEMON, "jsonmon")
}

func log(severity int, message string) {
	if syslogFlag == false {
		fmt.Fprint(os.Stderr, "<", severity, ">", message, "\n")
	} else {
		switch severity {
		case 2:
			logs.Crit(message)
		case 3:
			logs.Err(message)
		case 4:
			logs.Warning(message)
		case 5:
			logs.Notice(message)
		case 7:
			logs.Debug(message)
		}
	}
}
