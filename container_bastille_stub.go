//go:build !freebsd

// Bastille 桩：非 FreeBSD 平台不支持 Bastille 能力，所有操作返回 errContainerUnsupported。

package main

func bastilleReleases() ([]map[string]any, error) { return nil, errContainerUnsupported }

func bastilleBootstrap(release string) (string, error) { return "", errContainerUnsupported }

func bastilleJails() ([]map[string]any, error) { return nil, errContainerUnsupported }

func bastilleCreate(name, release, ip, jtype string, vnet, bridge bool, mac string) error {
	return errContainerUnsupported
}

func bastilleAction(name, action string) error { return errContainerUnsupported }

func bastilleLogs(name string, tail int) (string, error) { return "", errContainerUnsupported }

func bastilleCmd(name, command string) (string, error) { return "", errContainerUnsupported }

func bastilleConfig(name string) (string, error) { return "", errContainerUnsupported }

func bastilleMounts(name string) ([]string, error) { return nil, errContainerUnsupported }

func bastilleTemplates() ([]string, error) { return nil, errContainerUnsupported }

func bastilleApply(jail, template string, args map[string]string) (string, error) {
	return "", errContainerUnsupported
}

func bastilleRdr(jail, proto string, hostPort, jailPort int, add bool) error {
	return errContainerUnsupported
}
