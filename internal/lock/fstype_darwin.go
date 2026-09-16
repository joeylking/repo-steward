package lock

import "syscall"

func fsTypeName(dir string) (string, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return "", err
	}
	b := make([]byte, 0, len(st.Fstypename))
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b), nil
}
