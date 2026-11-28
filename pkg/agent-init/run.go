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
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"
)

type child struct {
	process  *os.Process
	deadline time.Time
	killed   bool
}

type initResult struct {
	epoch  uint64
	serial uint64
	err    error
}

type supervisor struct {
	cfg                               Config
	argv                              []string
	cwd                               string
	env                               []string
	backend, business                 *child
	desired, gate, closing            bool
	generation, epoch, serial         uint64
	instance                          string
	businessError                     string
	current                           *InitRequest
	backendRestarts, businessRestarts int
	backendAt, businessAt, initAt     time.Time
	initCancel                        context.CancelFunc
	initWaiters, stopWaiters          []command
	results                           chan initResult
	ctx                               context.Context
}

// Run's event loop exclusively owns child starts, signals and reaping.
func Run(ctx context.Context, cfg Config, argv []string) (retErr error) {
	cfg, err := normalizeConfig(cfg, argv)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return errors.New("capture working directory")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return errors.New("generate instance identity")
	}
	listener, cleanup, err := listenControl(cfg.SocketPath)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, cleanup()) }()
	// Keep control service and init workers separate from the caller context,
	// so cancellation initiates bounded cleanup rather than aborting it.
	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()
	s := &supervisor{cfg: cfg, argv: append([]string(nil), argv...), cwd: cwd,
		env: environment(os.Environ(), nil), desired: true,
		instance: hex.EncodeToString(id[:]), results: make(chan initResult, 16), ctx: workerCtx}
	commands := make(chan command)
	done := make(chan struct{})
	defer close(done)
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	if err := s.startBackend(); err != nil {
		return err
	}
	server := &http.Server{Handler: controlHandler(commands, done), ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: controlTimeout, IdleTimeout: 5 * time.Second,
		MaxHeaderBytes: 8 << 10}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(listener) }()
	defer func() {
		if err := server.Close(); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	ctxDone := ctx.Done()
	var shutdownDeadline, killDeadline time.Time
	beginShutdown := func() {
		if s.closing {
			return
		}
		s.closing, s.desired, s.gate = true, false, false
		s.invalidateInit(errors.New("shutting down"))
		cancelWorkers()
		now := time.Now()
		shutdownDeadline = now.Add(cfg.StopTimeout)
		killDeadline = shutdownDeadline.Add(time.Second)
		retErr = errors.Join(retErr, s.stopChild(s.business, now), s.stopChild(s.backend, now), signalOrphans(syscall.SIGTERM))
		ctxDone = nil
	}
	for {
		select {
		case <-ctxDone:
			beginShutdown()
		case sig := <-signals:
			if sig == syscall.SIGTERM || sig == syscall.SIGINT {
				beginShutdown()
			}
		case cmd := <-commands:
			if s.closing {
				s.respond(cmd, errors.New("shutting down"))
			} else {
				s.handle(cmd)
			}
		case result := <-s.results:
			// Reap before accepting a network completion: a dead backend cannot
			// open its gate merely because its successful response arrived late.
			if err := s.reap(time.Now()); err != nil {
				retErr = errors.Join(retErr, err)
				beginShutdown()
			}
			s.completeInit(result)
		case err := <-serveErrors:
			if !errors.Is(err, http.ErrServerClosed) {
				retErr = errors.Join(retErr, errors.New("control server stopped"))
			}
			beginShutdown()
		case <-ticker.C:
		}
		now := time.Now()
		if err := s.reap(now); err != nil {
			retErr = errors.Join(retErr, err)
			beginShutdown()
		}
		if s.closing {
			if !now.Before(shutdownDeadline) {
				retErr = errors.Join(retErr, signalOrphans(syscall.SIGKILL))
				shutdownDeadline = now.Add(time.Hour)
			}
			if s.business == nil && s.backend == nil {
				// Do not leave TERM-ignoring adopted descendants behind when the
				// direct children exit before the grace deadline.
				if err := signalOrphans(syscall.SIGKILL); err != nil {
					retErr = errors.Join(retErr, err)
				}
				_, remaining, err := reapChildren(nil, now)
				if err != nil {
					retErr = errors.Join(retErr, err)
				}
				if !remaining || !now.Before(killDeadline) {
					return retErr
				}
			}
			if !now.Before(killDeadline) {
				return errors.Join(retErr, errors.New("bounded child shutdown timed out"))
			}
			continue
		}
		s.reconcile(now)
	}
}

func (s *supervisor) status() Status {
	st := Status{Initialized: s.gate, State: "stopped", Generation: s.generation, Error: s.businessError}
	if s.epoch != 0 {
		st.InstanceID = s.instance + "-" + hexUint(s.epoch)
	}
	if s.current != nil {
		st.Revision = s.current.Revision
	}
	if s.backend != nil {
		st.BackendPID = s.backend.process.Pid
	}
	if s.closing {
		st.State = "shutting-down"
	} else if s.business != nil && !s.business.deadline.IsZero() {
		st.State = "stopping"
	} else if s.business != nil {
		st.State = "running"
	} else if s.desired {
		if s.cfg.WaitForInit && !s.gate {
			st.State = "waiting-for-init"
		} else {
			st.State = "starting"
		}
	}
	if s.business != nil {
		st.PID = s.business.process.Pid
	}
	return st
}

func hexUint(value uint64) string {
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = byte(value)
		value >>= 8
	}
	return hex.EncodeToString(b[:])
}

func (s *supervisor) respond(cmd command, err error) { cmd.reply <- reply{s.status(), err} }

func (s *supervisor) handle(cmd command) {
	if cmd.ctx.Err() != nil {
		s.respond(cmd, cmd.ctx.Err())
		return
	}
	switch cmd.action {
	case "status":
		s.respond(cmd, nil)
	case "start":
		if s.business != nil && !s.business.deadline.IsZero() {
			s.respond(cmd, errors.New("stop in progress"))
			return
		}
		s.desired = true
		s.businessAt = time.Time{}
		err := s.startBusiness()
		s.respond(cmd, err)
	case "stop", "restart":
		if cmd.action == "restart" && s.business != nil && !s.business.deadline.IsZero() {
			s.respond(cmd, errors.New("stop in progress"))
			return
		}
		s.desired = cmd.action == "restart"
		s.businessAt = time.Time{}
		if err := s.stopChild(s.business, time.Now()); err != nil {
			s.respond(cmd, err)
			return
		}
		if s.business == nil {
			var err error
			if s.desired {
				err = s.startBusiness()
			}
			s.respond(cmd, err)
		} else {
			s.stopWaiters = append(s.stopWaiters, cmd)
		}
	case "init":
		init := *cmd.init
		if s.current != nil {
			if init.LifecycleID != s.current.LifecycleID || init.Revision < s.current.Revision ||
				(init.Revision == s.current.Revision && !reflect.DeepEqual(init, *s.current)) {
				s.respond(cmd, errors.New("conflicting init"))
				return
			}
			if reflect.DeepEqual(init, *s.current) {
				if s.gate {
					s.respond(cmd, nil)
					return
				}
				s.initWaiters = append(s.initWaiters, cmd)
				if s.initCancel == nil {
					s.beginInit()
				}
				return
			}
		}
		s.invalidateInit(errors.New("init superseded"))
		s.current = &init
		s.initWaiters = append(s.initWaiters, cmd)
		s.beginInit()
	}
}

func (s *supervisor) invalidateInit(err error) {
	s.gate = false
	s.serial++
	if s.initCancel != nil {
		s.initCancel()
		s.initCancel = nil
	}
	for _, cmd := range s.initWaiters {
		s.respond(cmd, err)
	}
	s.initWaiters = nil
}

func (s *supervisor) beginInit() {
	if s.closing || s.backend == nil || s.current == nil || s.initCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.initCancel = cancel
	s.serial++
	epoch, serial, init := s.epoch, s.serial, *s.current
	go func() {
		result := initResult{epoch, serial, backendInit(ctx, s.cfg, init)}
		select {
		case s.results <- result:
		case <-s.ctx.Done():
		}
	}()
}

func (s *supervisor) completeInit(result initResult) {
	if s.closing || s.backend == nil || result.epoch != s.epoch || result.serial != s.serial {
		return
	}
	if s.initCancel != nil {
		s.initCancel()
		s.initCancel = nil
	}
	if result.err == nil {
		s.gate = true
	} else {
		s.initAt = time.Now().Add(s.cfg.RestartBackoff)
	}
	var startErr error
	if s.gate {
		startErr = s.startBusiness()
	}
	for _, cmd := range s.initWaiters {
		s.respond(cmd, errors.Join(result.err, startErr))
	}
	s.initWaiters = nil
}

func (s *supervisor) startBackend() error {
	p, err := s.spawn(s.cfg.Backend, s.env)
	if err != nil {
		return errors.New("backend exec failed")
	}
	s.backend = p
	s.epoch++
	return nil
}

func (s *supervisor) startBusiness() error {
	if s.closing || !s.desired || (s.cfg.WaitForInit && !s.gate) || s.backend == nil || s.business != nil {
		return nil
	}
	p, err := s.spawn(s.argv, environment(s.env, s.current))
	if err != nil {
		s.desired = false
		s.businessError = "business exec failed"
		return errors.New(s.businessError)
	}
	s.businessError = ""
	s.business = p
	s.generation++
	return nil
}

func (s *supervisor) spawn(argv, env []string) (*child, error) {
	path, err := executable(argv[0], s.cwd, env)
	if err != nil {
		return nil, err
	}
	// Child streams are inherited; the supervisor itself never logs argv or
	// environment. Backend stdout/stderr remain the backend's responsibility.
	p, err := os.StartProcess(path, argv, &os.ProcAttr{Dir: s.cwd, Env: env,
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr}, Sys: &syscall.SysProcAttr{Setpgid: true}})
	if err != nil {
		return nil, errors.New("exec failed")
	}
	return &child{process: p}, nil
}

func executable(name, cwd string, env []string) (string, error) {
	if strings.ContainsRune(name, '/') {
		if !filepath.IsAbs(name) {
			name = filepath.Join(cwd, name)
		}
		return name, nil
	}
	path := ""
	for _, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			path = strings.TrimPrefix(entry, "PATH=")
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return candidate, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrPermission) {
			return "", errors.New("resolve executable")
		}
	}
	return "", errors.New("executable not found")
}

func signalGroup(p *child, sig syscall.Signal) error {
	if p == nil {
		return nil
	}
	// The direct child's PID remains reserved until this loop reaps it. Never
	// save a bare PGID and signal it later after releasing its leader PID.
	err := killGroup(p.process.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("signal child process group: %w", err)
	}
	return nil
}

func (s *supervisor) stopChild(p *child, now time.Time) error {
	if p == nil || !p.deadline.IsZero() {
		return nil
	}
	p.deadline = now.Add(s.cfg.StopTimeout)
	return signalGroup(p, syscall.SIGTERM)
}

func (s *supervisor) reap(now time.Time) error {
	children := []*child{s.business, s.backend}
	for _, p := range children {
		if p != nil && !p.deadline.IsZero() && !now.Before(p.deadline) && !p.killed {
			p.killed = true
			if err := signalGroup(p, syscall.SIGKILL); err != nil {
				return err
			}
		}
	}
	pids, _, err := reapChildren(children, now)
	if err != nil {
		return fmt.Errorf("reap child processes: %w", err)
	}
	for _, pid := range pids {
		isBackend := s.backend != nil && s.backend.process.Pid == pid
		p := s.business
		if isBackend {
			p = s.backend
		}
		if p == nil || p.process.Pid != pid {
			continue
		}
		if err := p.process.Release(); err != nil {
			return err
		}
		if isBackend {
			s.backend = nil
			s.invalidateInit(errors.New("backend instance exited"))
			if !s.closing {
				s.backendRestarts++
				s.backendAt = now.Add(backoff(s.cfg.RestartBackoff, s.backendRestarts))
			} else {
				s.backendAt = time.Time{}
			}
		} else {
			s.business = nil
			if p.deadline.IsZero() {
				if !s.closing && s.desired && s.cfg.AutoRestart && s.businessRestarts < s.cfg.MaxRestarts {
					s.businessRestarts++
					s.businessAt = now.Add(backoff(s.cfg.RestartBackoff, s.businessRestarts))
				} else {
					s.desired = false
				}
			}
		}
	}
	return nil
}

func (s *supervisor) reconcile(now time.Time) {
	if s.backend == nil && !s.backendAt.IsZero() && !now.Before(s.backendAt) {
		s.backendAt = time.Time{}
		if err := s.startBackend(); err != nil {
			s.backendRestarts++
			s.backendAt = now.Add(backoff(s.cfg.RestartBackoff, s.backendRestarts))
		} else if s.current != nil {
			s.beginInit()
		}
	}
	if !s.gate && s.current != nil && s.backend != nil && s.initCancel == nil && !now.Before(s.initAt) {
		s.beginInit()
	}
	var startErr error
	if s.business == nil && (s.businessAt.IsZero() || !now.Before(s.businessAt)) {
		startErr = s.startBusiness()
	}
	if s.business == nil || s.business.deadline.IsZero() {
		for _, cmd := range s.stopWaiters {
			err := startErr
			if cmd.action == "restart" && !s.desired {
				err = errors.New("restart canceled")
			}
			s.respond(cmd, err)
		}
		s.stopWaiters = nil
	}
}
