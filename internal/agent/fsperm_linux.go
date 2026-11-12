//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"syscall"
)

const selinuxXattr = "security.selinux"

func getSELinux(path string) []byte {
	buf := make([]byte, 256)
	n, err := syscall.Getxattr(path, selinuxXattr, buf)
	if err != nil || n <= 0 {
		return nil
	}
	return buf[:n]
}

// fixOwnership copies uid/gid/mode and SELinux label from ref (dir) and
// refFile (optional file) to newDir and its files.
func fixOwnership(ref, refFile, newDir string, names []string) error {
	var rst syscall.Stat_t
	if err := syscall.Stat(ref, &rst); err != nil {
		return err
	}
	dirMode := os.FileMode(rst.Mode & 0o7777)
	fileUID, fileGID := int(rst.Uid), int(rst.Gid)
	fileMode := os.FileMode(0o660)
	dirLabel := getSELinux(ref)
	fileLabel := dirLabel
	if refFile != "" {
		var fst syscall.Stat_t
		if err := syscall.Stat(refFile, &fst); err == nil {
			fileUID, fileGID = int(fst.Uid), int(fst.Gid)
			fileMode = os.FileMode(fst.Mode & 0o777)
			if l := getSELinux(refFile); l != nil {
				fileLabel = l
			}
		}
	}
	apply := func(p string, uid, gid int, mode os.FileMode, label []byte) error {
		if err := os.Lchown(p, uid, gid); err != nil {
			return err
		}
		if err := os.Chmod(p, mode); err != nil {
			return err
		}
		if label != nil {
			if err := syscall.Setxattr(p, selinuxXattr, label, 0); err != nil {
				return err
			}
		}
		return nil
	}
	if err := apply(newDir, int(rst.Uid), int(rst.Gid), dirMode, dirLabel); err != nil {
		return err
	}
	for _, n := range names {
		if err := apply(filepath.Join(newDir, n), fileUID, fileGID, fileMode, fileLabel); err != nil {
			return err
		}
	}
	return nil
}
