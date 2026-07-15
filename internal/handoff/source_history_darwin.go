//go:build darwin

package handoff

import "golang.org/x/sys/unix"

func renameSourceHistoryNoReplace(rootFD int, from, to string) error {
	return unix.RenameatxNp(rootFD, from, rootFD, to, unix.RENAME_EXCL)
}

func sourceHistoryMtimeNsec(stat unix.Stat_t) int64 {
	return stat.Mtim.Sec*1_000_000_000 + stat.Mtim.Nsec
}
