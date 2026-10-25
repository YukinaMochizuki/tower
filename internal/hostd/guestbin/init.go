package guestbin

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// stopGrace is how long a container's processes have to end once init is
// told to stop, before it exits and Docker kills what is left.
const stopGrace = 5 * time.Second

// initMain is tower-guest init [-- COMMAND...], a container's first
// process: it runs the image's own entrypoint and command, if any, reaps
// every orphan, as processes started apart from any call become, and
// stays up when the image's command ends, so the container runs until it
// is stopped. Stopped (SIGTERM or SIGINT), it passes the signal to every
// process and exits once they end, or after stopGrace.
func initMain(args []string) int {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if os.Getpid() != 1 {
		// Orphans reach the reaper only as the namespace's first process.
		fmt.Fprintln(os.Stderr, "tower-guest init: not the first process; orphans are not reaped")
	}
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	main := -1
	if len(args) > 0 {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "tower-guest init: the image's command: %v\n", err)
		} else {
			main = cmd.Process.Pid
		}
	}
	for sig := range sigs {
		switch sig {
		case syscall.SIGCHLD:
			reap(main)
		case syscall.SIGHUP:
			if main > 0 {
				_ = syscall.Kill(main, syscall.SIGHUP)
			}
		default:
			_ = syscall.Kill(-1, sig.(syscall.Signal))
			deadline := time.Now().Add(stopGrace)
			for time.Now().Before(deadline) {
				reap(main)
				if !anyChild() {
					return 0
				}
				time.Sleep(50 * time.Millisecond)
			}
			return 0
		}
	}
	return 0
}

// reap waits for every child that has ended.
func reap(main int) {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
		if pid == main {
			fmt.Fprintf(os.Stderr, "tower-guest init: the image's command ended (%v); the container runs on\n", ws)
		}
	}
}

// anyChild reports whether init may have a child left: one running, or
// one it has just reaped, after which there may be more.
func anyChild() bool {
	var ws syscall.WaitStatus
	_, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
	return err == nil
}
