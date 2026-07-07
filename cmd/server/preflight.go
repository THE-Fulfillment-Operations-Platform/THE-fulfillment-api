package main

import (
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// freePortForDev kills any stale process still listening on the given TCP port.
//
// This is a dev-only convenience. `go run ./cmd/server` compiles and runs the
// server as a child process; if the parent is killed abruptly (closing the
// terminal, an editor stopping the task) the child binary can be orphaned and
// keep holding the port, causing "bind: address already in use" on the next
// start. Freeing the port here makes a restart work no matter how the previous
// run ended.
//
// It is a no-op in production (single-container deploys never contend for the
// port, and we must never kill neighbouring processes there), and degrades
// silently if `lsof` is unavailable.
func freePortForDev(port string) {
	out, err := exec.Command("lsof", "-ti", "tcp:"+port, "-sTCP:LISTEN").Output()
	if err != nil {
		return // lsof missing, or nothing is listening — nothing to free
	}
	self := os.Getpid()
	killed := false
	for _, field := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(field)
		if err != nil || pid == self {
			continue
		}
		if proc, err := os.FindProcess(pid); err == nil {
			if err := proc.Signal(syscall.SIGKILL); err == nil {
				log.Printf("preflight: freed port %s (killed stale pid=%d)", port, pid)
				killed = true
			}
		}
	}
	if killed {
		// Give the kernel a moment to release the socket before we bind.
		time.Sleep(200 * time.Millisecond)
	}
}
