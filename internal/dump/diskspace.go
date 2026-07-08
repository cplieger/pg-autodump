package dump

import "syscall"

// statfsFreeKB returns the free space (in KB) available to an unprivileged user
// on the filesystem backing dir, via statfs. It is the Orchestrator's default
// disk-space probe (held behind the o.freeSpace seam so the low-space decision
// can be exercised at exact thresholds without depending on the live
// filesystem, whose free space drifts between reads).
//
// Linux-only via syscall.Statfs, consistent with the rest of the module
// (atomicfile is Linux-only by design).
func statfsFreeKB(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	//nolint:gosec // G115: filesystem block counts fit int64 (largest pool << 2^63).
	return int64(st.Bavail) * st.Bsize / 1024, nil
}

// checkDiskSpace logs a warning when the free space on the dump directory is
// below freeKBWarn at the start of a run. It is advisory only: a low-space
// warning never blocks a dump (the dump fails loudly on its own if the write
// cannot complete). A zero or negative threshold disables the check, skipping
// the probe entirely.
//
// The guard is strict (freeKB < freeKBWarn): free space exactly at the
// threshold is not "low" and stays silent.
func (o *Orchestrator) checkDiskSpace() {
	if o.freeKBWarn <= 0 {
		return
	}
	freeKB, err := o.freeSpace(o.dumpDir)
	if err != nil {
		o.log.Warn("cannot check free disk space", "dir", o.dumpDir, "err", err)
		return
	}
	if freeKB < o.freeKBWarn {
		o.log.Warn("low free disk space for dumps",
			"dir", o.dumpDir, "free_kb", freeKB, "warn_below_kb", o.freeKBWarn)
	}
}
