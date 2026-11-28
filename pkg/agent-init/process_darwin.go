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
	"net"
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
		cred, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		sockErr = err
		if err == nil {
			uid = cred.Uid
		}
	})
	return uid, errors.Join(err, sockErr)
}

func killGroup(pid int, sig syscall.Signal) error {
	err := syscall.Kill(-pid, sig)
	// Darwin reports EPERM for a group containing only an exited leader.
	// Only normalize it when the still-unreaped leader has no live PGID;
	// never mask a permission failure for a live process group.
	if errors.Is(err, syscall.EPERM) {
		_, lookupErr := syscall.Getpgid(pid)
		if errors.Is(lookupErr, syscall.ESRCH) {
			return nil
		}
	}
	return err
}

// Non-Linux tests own only registered children, never process-global wait4.
func signalOrphans(_ syscall.Signal) error { return nil }

func reapChildren(children []*child, now time.Time) ([]int, bool, error) {
	var pids []int
	for _, p := range children {
		if p == nil || (!p.deadline.IsZero() && now.Before(p.deadline)) {
			continue
		}
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(p.process.Pid, &status, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return pids, false, err
		}
		if pid > 0 {
			pids = append(pids, pid)
		}
	}
	// Darwin has no WNOWAIT peek here; never signal a group after consuming
	// its leader. Natural-exit descendant cleanup is therefore best effort.
	return pids, false, nil
}
