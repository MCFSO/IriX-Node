//go:build !windows && !linux && !android && !freebsd

package sysinfo

func ProcessIdentity(pid int) (string, error) {
	return "", ErrProcessIdentityUnsupported
}

func TerminateRecordedProcess(pid int, identity string) error {
	return ErrProcessIdentityUnsupported
}
