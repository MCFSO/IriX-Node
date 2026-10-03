//go:build !windows && !linux && !android && !freebsd

package main

func processIdentity(pid int) (string, error) {
	return "", errProcessIdentityUnsupported
}

func terminateRecordedProcess(record processRecord) error {
	return errProcessIdentityUnsupported
}
