package guestbin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/YukinaMochizuki/tower/internal/guest"
)

// stopGrace is how long a container's processes have to end once init is
// told to stop, before it exits and Docker kills what is left.
const stopGrace = 5 * time.Second

// tailscaledRestart is how long init waits to start tailscaled again once
// it ended.
const tailscaledRestart = 2 * time.Second

// initMain is tower-guest init [--tailnet [--as USER] [--as-home]]
// [-- COMMAND...], a container's first process: it runs the image's own
// entrypoint and command, if any, reaps every orphan, as processes started
// apart from any call become, and stays up when the image's command ends,
// so the container runs until it is stopped. Stopped (SIGTERM or SIGINT),
// it passes the signal to every process and exits once they end, or after
// stopGrace.
//
// With --tailnet, in a container started as root that joins a tailnet, it
// first starts the tailnet (startTailnet), keeps tailscaled running, and
// runs the image's command without NET_ADMIN as USER, the image's, with
// HOME set from its passwd entry given --as-home (tower-guest dropped).
func initMain(args []string) int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tower-guest init: %v\n", err)
		return 1
	}
	_, leaveErr := os.Lstat(filepath.Join(guest.TailnetControlDir, guest.TailnetLeaveFile))
	tailnet, leave, args, err := initPlan(self, args, leaveErr == nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tower-guest init: %v\n", err)
		return 2
	}
	if leave {
		return leaveOnly(context.Background(), defaultTailnetPaths, os.Stderr)
	}
	if os.Getpid() != 1 {
		// Orphans reach the reaper only as the namespace's first process.
		fmt.Fprintln(os.Stderr, "tower-guest init: not the first process; orphans are not reaped")
	}
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	var tsd *tailscaledProc
	restart := make(chan struct{}, 1)
	if tailnet {
		if tsd, err = startTailnet(context.Background(), defaultTailnetPaths); err != nil {
			fmt.Fprintf(os.Stderr, "tower-guest init: the tailnet: %v; the container runs without it\n", err)
		}
	}
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
	ended := func(pid int) {
		if tsd != nil && pid == tsd.pid {
			fmt.Fprintf(os.Stderr, "tower-guest init: tailscaled ended; it starts again in %s\n", tailscaledRestart)
			time.AfterFunc(tailscaledRestart, func() { restart <- struct{}{} })
		}
	}
	for {
		select {
		case <-restart:
			if err := tsd.start(); err != nil {
				fmt.Fprintf(os.Stderr, "tower-guest init: tailscaled: %v\n", err)
			}
			continue
		case sig := <-sigs:
			switch sig {
			case syscall.SIGCHLD:
				reap(main, ended)
			case syscall.SIGHUP:
				if main > 0 {
					_ = syscall.Kill(main, syscall.SIGHUP)
				}
			default:
				_ = syscall.Kill(-1, sig.(syscall.Signal))
				deadline := time.Now().Add(stopGrace)
				for time.Now().Before(deadline) {
					reap(main, nil)
					if !anyChild() {
						return 0
					}
					time.Sleep(50 * time.Millisecond)
				}
				return 0
			}
		}
	}
}

// initPlan reads init's arguments: whether the container joins a
// tailnet, whether it is only to leave it, as its driver asks with
// leaveAsked, and the image's command as init runs it, none for none or to
// leave. In a container that joins a tailnet, it runs through self's
// dropped, which takes NET_ADMIN from it and runs it as the image's user.
func initPlan(self string, args []string, leaveAsked bool) (tailnet, leave bool, command []string, err error) {
	user, asHome := "", false
	for len(args) > 0 && args[0] != "--" {
		switch {
		case args[0] == "--tailnet":
			tailnet, args = true, args[1:]
		case args[0] == "--as" && len(args) > 1:
			user, args = args[1], args[2:]
		case args[0] == "--as-home":
			asHome, args = true, args[1:]
		default:
			return false, false, nil, fmt.Errorf("unknown argument %q", args[0])
		}
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if tailnet && leaveAsked {
		return true, true, nil, nil
	}
	if !tailnet || len(args) == 0 {
		return tailnet, false, args, nil
	}
	dropped := []string{self, "dropped", "--as", user}
	if asHome {
		dropped = append(dropped, "--as-home")
	}
	return tailnet, false, append(append(dropped, "--"), args...), nil
}

// reap waits for every child that has ended, telling ended of each.
func reap(main int, ended func(pid int)) {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
		if pid == main {
			fmt.Fprintf(os.Stderr, "tower-guest init: the image's command ended (%v); the container runs on\n", ws)
		}
		if ended != nil {
			ended(pid)
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
