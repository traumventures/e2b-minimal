package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/traumventures/e2b-minimal/internal/store"
)

// Memory safety with MEMORY_MODE=regular. Sandboxes take host memory on
// demand up to their template's size; the parent cgroup's memory.max caps all
// of them together, so the host never runs short. Inside that cap:
//
//   - every running sandbox is promised MemoryMinMB. A create or resume that
//     cannot be promised it, because the promises would exceed memory.max or
//     because less than that is free right now, is refused with 503;
//   - the promises are memory.min on each sandbox cgroup and their sum on the
//     parent, so host memory pressure never reclaims below them;
//   - beyond its promise a sandbox uses what is free. When all sandboxes
//     together come within the headroom of memory.max, the watchdog kills the
//     one furthest above its promise through the orchestrator. memory.high,
//     set halfway into the headroom, slows allocations so it gets there
//     first. The kernel OOM killer at memory.max is only the backstop: guest
//     memory is a memfd the orchestrator holds, so the kernel cannot free it
//     when it kills Firecracker, and it would go on to kill innocent sandboxes.

var errNoMemory = errors.New("not enough memory for another sandbox")

func (s *Server) memoryGuarded() bool { return s.cfg.CgroupRoot != "" && s.cfg.MemoryMinMB > 0 }

func readCgroupInt(path string) (int64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v := strings.TrimSpace(string(raw))
	if v == "max" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)

	return n, err == nil
}

// admit checks that one more sandbox can be promised MemoryMinMB. The caller
// holds no lock; the sandbox being admitted is already in the store as Busy.
func (s *Server) admit(sandboxID string) error {
	if !s.memoryGuarded() {
		return nil
	}
	limit, limited := readCgroupInt(filepath.Join(s.cfg.CgroupRoot, "memory.max"))
	if !limited {
		return nil
	}
	used, _ := readCgroupInt(filepath.Join(s.cfg.CgroupRoot, "memory.current"))
	minBytes := s.cfg.MemoryMinMB << 20
	promised := int64(0)
	s.store.View(func(tx store.Tx) {
		for _, sb := range tx.Sandboxes() {
			if sb.ID != sandboxID && sb.State == store.SandboxRunning {
				promised += minBytes
			}
		}
	})
	if promised+minBytes > limit-s.headroom(limit) {
		return fmt.Errorf("%w: %d MiB promised to running sandboxes, %d MiB more would exceed SANDBOX_MEMORY_MAX (%d MiB)",
			errNoMemory, promised>>20, s.cfg.MemoryMinMB, limit>>20)
	}
	if used+minBytes > limit-s.headroom(limit) {
		return fmt.Errorf("%w: sandboxes use %d of %d MiB, %d MiB are not free",
			errNoMemory, used>>20, limit>>20, s.cfg.MemoryMinMB)
	}

	return nil
}

// applyMemoryPromises writes memory.min for every running sandbox and their
// sum on the parent, whose own protection bounds what the children get.
func (s *Server) applyMemoryPromises() {
	if !s.memoryGuarded() {
		return
	}
	var running []string
	s.store.View(func(tx store.Tx) {
		for _, sb := range tx.Sandboxes() {
			if sb.State == store.SandboxRunning {
				running = append(running, sb.ID)
			}
		}
	})
	minBytes := s.cfg.MemoryMinMB << 20
	total := int64(0)
	for _, id := range running {
		dirs, _ := filepath.Glob(filepath.Join(s.cfg.CgroupRoot, "sbx-"+id+"-*"))
		for _, d := range dirs {
			if err := os.WriteFile(filepath.Join(d, "memory.min"), []byte(strconv.FormatInt(minBytes, 10)), 0); err != nil {
				slog.Warn("set sandbox memory.min", "sandbox", id, "err", err)
			}
		}
		if len(dirs) > 0 {
			total += minBytes
		}
	}
	if err := os.WriteFile(filepath.Join(s.cfg.CgroupRoot, "memory.min"), []byte(strconv.FormatInt(total, 10)), 0); err != nil {
		slog.Warn("set memory.min for all sandboxes", "err", err)
	}
}

// headroom is the part of memory.max kept free of promises, where the
// watchdog acts: 5%, at least 256 MiB.
func (s *Server) headroom(limit int64) int64 {
	return max(limit/20, 256<<20)
}

// RunMemoryWatchdog keeps the sandboxes together below memory.max.
func (s *Server) RunMemoryWatchdog(ctx context.Context) {
	if !s.memoryGuarded() {
		return
	}
	limit, limited := readCgroupInt(filepath.Join(s.cfg.CgroupRoot, "memory.max"))
	if !limited {
		return
	}
	high := limit - s.headroom(limit)/2
	if err := os.WriteFile(filepath.Join(s.cfg.CgroupRoot, "memory.high"), []byte(strconv.FormatInt(high, 10)), 0); err != nil {
		slog.Warn("set memory.high for all sandboxes", "err", err)
	}
	threshold := limit - s.headroom(limit)
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if used, _ := readCgroupInt(filepath.Join(s.cfg.CgroupRoot, "memory.current")); used < threshold {
			continue
		}
		freed := s.killLargest(ctx, threshold)
		// The victim's memory is released a moment after the kill returns;
		// wait for it before judging again, or a second sandbox dies for the
		// same overload.
		s.waitBelow(ctx, threshold, freed)
	}
}

// killLargest kills the running sandbox that uses the most memory above its
// promise and returns how much memory it used.
func (s *Server) killLargest(ctx context.Context, threshold int64) int64 {
	var ids []string
	s.store.View(func(tx store.Tx) {
		for _, sb := range tx.Sandboxes() {
			if sb.State == store.SandboxRunning && !sb.Busy {
				ids = append(ids, sb.ID)
			}
		}
	})
	victim, worst := "", int64(0)
	for _, id := range ids {
		dirs, _ := filepath.Glob(filepath.Join(s.cfg.CgroupRoot, "sbx-"+id+"-*"))
		for _, d := range dirs {
			if cur, ok := readCgroupInt(filepath.Join(d, "memory.current")); ok && cur > worst {
				victim, worst = id, cur
			}
		}
	}
	if victim == "" || worst <= s.cfg.MemoryMinMB<<20 {
		return 0
	}
	sb, _, err := s.claim(victim)
	if err != nil {
		return 0
	}
	slog.Warn("all sandboxes together reached the memory limit, killing the largest",
		"sandbox", victim, "used_mib", worst>>20, "promised_mib", s.cfg.MemoryMinMB, "threshold_mib", threshold>>20)
	kctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.kill(kctx, sb); err != nil {
		slog.Error("kill sandbox over the memory limit", "sandbox", victim, "err", err)
		s.release(victim)

		return 0
	}

	return worst
}

// waitBelow waits up to five seconds for the freed memory to show, or for
// usage to drop below the threshold.
func (s *Server) waitBelow(ctx context.Context, threshold, freed int64) {
	if freed == 0 {
		return
	}
	start, _ := readCgroupInt(filepath.Join(s.cfg.CgroupRoot, "memory.current"))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
		cur, _ := readCgroupInt(filepath.Join(s.cfg.CgroupRoot, "memory.current"))
		if cur < threshold || cur <= start-freed/2 {
			return
		}
	}
}
