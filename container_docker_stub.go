//go:build !linux

// Docker 桩：非 Linux 平台不支持 Docker 能力，所有操作返回 errContainerUnsupported。

package main

func dockerPS(all bool) ([]map[string]any, error) { return nil, errContainerUnsupported }

func dockerCreate(name, image, command, workDir string, ports, volumes []string, env map[string]string, restartPolicy string, memoryLimitMb int, cpus float64, diskLimitGb int) (map[string]any, error) {
	return nil, errContainerUnsupported
}

func dockerClone(id, name string) (map[string]any, error) { return nil, errContainerUnsupported }

func dockerExport(d *Daemon, id string) (map[string]any, error) {
	return nil, errContainerUnsupported
}

func dockerImageImport(d *Daemon, fileName, name string) error {
	return errContainerUnsupported
}

func dockerAction(id, action string) error { return errContainerUnsupported }

func dockerRemove(id string, force bool) error { return errContainerUnsupported }

func dockerLogs(id string, tail int) (string, error) { return "", errContainerUnsupported }

func dockerExec(id, command string) (string, error) { return "", errContainerUnsupported }

func dockerStats(id string) (map[string]any, error) { return nil, errContainerUnsupported }

func dockerImages() ([]map[string]any, error) { return nil, errContainerUnsupported }

func dockerPull(name string) error { return errContainerUnsupported }

func dockerImageRemove(name string) error { return errContainerUnsupported }

func dockerBuildStart(d *Daemon, dockerfile, name, tag string) (string, error) {
	return "", errContainerUnsupported
}

func dockerVolumeList() ([]map[string]any, error) { return nil, errContainerUnsupported }

func dockerVolumeRemove(name string) error { return errContainerUnsupported }

func dockerNetworkList() ([]map[string]any, error) { return nil, errContainerUnsupported }
