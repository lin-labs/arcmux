//go:build linux

package handoff

import "golang.org/x/sys/unix"

func renameSourceHistoryNoReplace(rootFD int, from, to string) error {
	return unix.Renameat2(rootFD, from, rootFD, to, unix.RENAME_NOREPLACE)
}

func sourceHistoryMtimeNsec(stat unix.Stat_t) int64 {
	return stat.Mtim.Sec*1_000_000_000 + stat.Mtim.Nsec
}
