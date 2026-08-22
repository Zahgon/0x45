package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// Prefork replaces fasthttp's SO_REUSEPORT process forking, which Fiber exposed
// through fiber.Config{Prefork}. net/http has no built-in equivalent, so the two
// halves fasthttp provided are reproduced here: a master that re-executes this
// binary once per core, and children that each bind the same address with
// SO_REUSEPORT so the kernel load-balances accepts between them.
//
// The environment key mirrors Fiber's FIBER_PREFORK_CHILD contract.
const (
	preforkChildKey = "0X_PREFORK_CHILD"
	preforkChildVal = "1"
)

// IsPreforkChild reports whether this process is a prefork child.
func IsPreforkChild() bool {
	return os.Getenv(preforkChildKey) == preforkChildVal
}

// listenPrefork serves addr in prefork mode. In a child it binds with
// SO_REUSEPORT and serves; in the master it supervises the children and returns
// as soon as any one of them exits, killing the rest — the same failure
// semantics fasthttp's prefork master had.
func (s *Server) listenPrefork(addr string) error {
	if IsPreforkChild() {
		// One core per child, as fasthttp did.
		runtime.GOMAXPROCS(1)

		ln, err := reusePortListen("tcp", addr)
		if err != nil {
			return fmt.Errorf("prefork: %w", err)
		}

		s.logger.Info("prefork child listening",
			zap.String("address", addr),
			zap.Int("pid", os.Getpid()))

		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}

	return s.preforkMaster(addr)
}

type preforkChild struct {
	pid int
	err error
}

func (s *Server) preforkMaster(addr string) error {
	max := runtime.GOMAXPROCS(0)
	children := make(map[int]*exec.Cmd, max)
	exited := make(chan preforkChild, max)

	// Kill every surviving child when the master returns.
	defer func() {
		for _, proc := range children {
			if proc.Process == nil {
				continue
			}
			if err := proc.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				s.logger.Error("prefork: failed to kill child", zap.Error(err))
			}
		}
	}()

	pids := make([]string, 0, max)
	for i := 0; i < max; i++ {
		cmd := exec.Command(os.Args[0], os.Args[1:]...) //nolint:gosec // re-executes this same binary
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Env = append(os.Environ(), preforkChildKey+"="+preforkChildVal)

		if err := cmd.Start(); err != nil {
			return fmt.Errorf("prefork: failed to start child process: %w", err)
		}

		pid := cmd.Process.Pid
		children[pid] = cmd
		pids = append(pids, strconv.Itoa(pid))

		go func(c *exec.Cmd, pid int) {
			exited <- preforkChild{pid: pid, err: c.Wait()}
		}(cmd, pid)
	}

	s.logger.Info("prefork master started",
		zap.String("address", addr),
		zap.Int("children", len(children)),
		zap.String("pids", strings.Join(pids, ",")))

	// Any child exiting takes the whole server down, as it did under fasthttp.
	dead := <-exited
	return fmt.Errorf("prefork: child %d exited: %w", dead.pid, dead.err)
}
