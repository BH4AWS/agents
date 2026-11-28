/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agentinit

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func peerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid uint32
	var sockErr error
	err = raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		sockErr = err
		if err == nil {
			uid = cred.Uid
		}
	})
	return uid, errors.Join(err, sockErr)
}

func killGroup(pid int, sig syscall.Signal) error { return syscall.Kill(-pid, sig) }

func signalOrphans(sig syscall.Signal) error {
	if os.Getpid() != 1 {
		return nil
	}
	err := syscall.Kill(-1, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// Reserve leaders until group cleanup; orphan draining must skip registered PIDs.
func reapChildren(children []*child, now time.Time) ([]int, bool, error) {
	var pids []int
	registered := make(map[int]struct{}, len(children))
	deferOrphans := false
	for _, p := range children {
		if p == nil {
			continue
		}
		registered[p.process.Pid] = struct{}{}
		if !p.deadline.IsZero() && now.Before(p.deadline) {
			deferOrphans = true
			continue
		}
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, p.process.Pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return pids, true, err
		}
		if info.Signo == 0 {
			continue
		}
		if err := signalGroup(p, syscall.SIGKILL); err != nil {
			return pids, true, err
		}
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(p.process.Pid, &status, syscall.WNOHANG, nil)
		if err != nil {
			return pids, true, err
		}
		if pid > 0 {
			pids = append(pids, pid)
		}
	}
	if os.Getpid() != 1 || deferOrphans {
		return pids, false, nil
	}

	// Go can spawn children on any OS thread, not just the thread-group leader.
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return pids, true, err
	}
	orphans := make(map[int]struct{})
	remaining := false
	for _, task := range tasks {
		path := filepath.Join("/proc/self/task", task.Name(), "children")
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			// Its children may have moved to an already scanned thread.
			remaining = true
			continue
		}
		if err != nil {
			return pids, true, err
		}
		for _, field := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(field)
			if err != nil {
				return pids, true, fmt.Errorf("parse child PID in %s: %w", path, err)
			}
			if pid <= 0 {
				return pids, true, fmt.Errorf("invalid child PID %q in %s", field, path)
			}
			// Rescan next tick even after reaping: descendants may be newly adopted.
			remaining = true
			if _, ok := registered[pid]; !ok {
				orphans[pid] = struct{}{}
			}
		}
	}
	for pid := range orphans {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
		if errors.Is(err, syscall.EINTR) || errors.Is(err, syscall.ECHILD) {
			continue
		}
		if err != nil {
			return pids, true, err
		}
		if info.Signo == 0 {
			continue
		}
		var status syscall.WaitStatus
		reaped, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) || errors.Is(err, syscall.ECHILD) {
			continue
		}
		if err != nil {
			return pids, true, err
		}
		if reaped > 0 {
			pids = append(pids, reaped)
		}
	}
	return pids, remaining, nil
}
