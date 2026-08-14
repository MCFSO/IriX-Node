//go:build !freebsd

// Bastille 桩：非 FreeBSD 平台不支持 Bastille 能力，所有操作返回 errContainerUnsupported。

package main

func bastilleReleases() ([]map[string]any, error) { return nil, errContainerUnsupported }

func bastilleBootstrap(release string) (string, error) { return "", errContainerUnsupported }

func bastilleJails() ([]map[string]any, error) { return nil, errContainerUnsupported }

func bastilleCreate(name, release, ip, jtype, vnetMode, iface string,
	volumes []bastilleVolume, workdir string, memoryLimitMb, cpus, diskLimitMb int) (map[string]any, error) {
	return nil, errContainerUnsupported
}

func bastilleAction(name, action string, force bool) error { return errContainerUnsupported }

func bastilleLogs(name string, tail int) (string, error) { return "", errContainerUnsupported }

func bastilleCmd(name, command string) (string, error) { return "", errContainerUnsupported }

func bastilleConfig(name string) (string, error) { return "", errContainerUnsupported }

func bastilleMounts(name string) ([]string, error) { return nil, errContainerUnsupported }

func bastilleMount(name, source, dest string) error { return errContainerUnsupported }

func bastilleUmount(name, dest string) error { return errContainerUnsupported }

func bastilleApplyLimits(name string, memoryLimitMb, cpus, diskLimitMb int) error {
	return errContainerUnsupported
}

func bastilleClone(name, newName, newIP string) error { return errContainerUnsupported }

func bastilleExport(d *Daemon, name string) (map[string]any, error) {
	return nil, errContainerUnsupported
}

func bastilleImport(d *Daemon, file, release string, force bool) error {
	return errContainerUnsupported
}

func bastilleSetupMode(mode, extIf, tunIf, addr string) (map[string]any, error) {
	return nil, errContainerUnsupported
}

func bastilleRdrList(jail string) ([]rdrRule, error) { return nil, errContainerUnsupported }

func bastilleRdrAdd(jail, proto string, hostPort, jailPort int) error {
	return errContainerUnsupported
}

func bastilleRdrDelete(jail, proto string, hostPort, jailPort int) error {
	return errContainerUnsupported
}

func bastilleTemplates() ([]string, error) { return nil, errContainerUnsupported }

func bastilleApply(jail, template string, args map[string]string) (string, error) {
	return "", errContainerUnsupported
}
