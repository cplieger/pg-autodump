package dump

import "syscall"

// checkDiskSpace logs a warning when the free space on the dump directory is
// below FreeKBWarn at the start of a run. It is advisory only: a low-space
// warning never blocks a dump (the dump fails loudly on its own if the write
// cannot complete). A zero or negative threshold disables the check.
//
// Linux-only via syscall.Statfs, consistent with the rest of the module
// (atomicfile is Linux-only by design).
func (o *Orchestrator) checkDiskSpace() {
	if o.freeKBWarn <= 0 {
		return
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(o.dumpDir, &st); err != nil {
		o.log.Warn("cannot check free disk space", "dir", o.dumpDir, "err", err)
		return
	}
	//nolint:gosec // G115: filesystem block counts fit int64 (largest pool << 2^63) and freeKBWarn is non-negative.
	freeKB := int64(st.Bavail) * st.Bsize / 1024
	if freeKB < o.freeKBWarn {
		o.log.Warn("low free disk space for dumps",
			"dir", o.dumpDir, "free_kb", freeKB, "warn_below_kb", o.freeKBWarn)
	}
}
