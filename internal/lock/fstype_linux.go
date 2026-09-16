package lock

import "syscall"

// Magic numbers from linux/magic.h for network filesystems.
const (
	nfsMagic  = 0x6969
	smb2Magic = 0xFE534D42
	cifsMagic = 0xFF534D42
)

func fsTypeName(dir string) (string, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return "", err
	}
	switch uint32(st.Type) {
	case nfsMagic:
		return "nfs", nil
	case smb2Magic:
		return "smb2", nil
	case cifsMagic:
		return "cifs", nil
	}
	return "local", nil
}
