// 容器 / Bastille 参数校验（红队审计 高-4）。
//
// 容器与 jail 的创建、挂载、执行端点在本项目中**等价于宿主机 root**：
//   - 绑定挂载可把宿主机任意目录（/etc、/root、节点数据目录）暴露给容器
//     （典型攻击形态 `-v /:/host`）；
//   - 环境变量可注入动态链接器（LD_PRELOAD / LD_LIBRARY_PATH），劫持容器
//     进程加载的库；
//   - workdir 会被写进 jail.conf 的 `exec.start = 'cd <workdir> && …'`
//     单引号字符串，换行/单引号可注入任意 jail 配置；
//   - 镜像构建端点可在宿主机上以 root 执行任意 Dockerfile 指令，
//     并可用 BuildKit 的 RUN --mount 把构建器可见的文件带进构建容器。
//
// 因此所有参数在进入 docker / bastille 命令行或 jail.conf 之前做白名单校验：
// 路径必须为绝对路径、不得落在系统目录与节点数据目录内、不得经符号链接穿透。
// 权限目录中相关端点的描述统一带「等价宿主机 root」标注，且非管理员账户的
// 端点开关默认为全关（accounts.createAccount 写入 '{}'），故默认仅管理员可用。

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// macAddressPattern 合法 MAC 地址：6 组十六进制字节，冒号分隔。
var macAddressPattern = regexp.MustCompile(`^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$`)

// volumeNamePattern Docker 命名卷名（不含 /，因此不是绑定挂载）。
var volumeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// dockerfileMountRE 抓取 Dockerfile 中 `RUN --mount=type=xxx` 的挂载类型
// （等号形式与空格形式都覆盖）。
var dockerfileMountRE = regexp.MustCompile(`(?i)--mount\s*=?\s*[^\n;&|]*\btype\s*=\s*([a-z]+)`)

// dockerfileBlockedMountTypes 拒绝的 BuildKit RUN --mount 类型：
//   - bind   ：把构建上下文之外的文件带进构建容器（审计要求禁用的形态）；
//   - cache  ：跨构建持久化任意文件（数据外带/串味）；
//   - secret / ssh：构建期读取凭据（本节点不提供凭据，一律拒绝）。
var dockerfileBlockedMountTypes = map[string]bool{
	"bind": true, "cache": true, "secret": true, "ssh": true,
}

// dockerfileInsecureRE 匹配 BuildKit 的越权模式。
var dockerfileInsecureRE = regexp.MustCompile(`(?i)--security\s*=?\s*insecure`)

// dockerfileHostNetRE 匹配构建期使用宿主机网络：构建容器可访问节点自身
// 监听在 127.0.0.1 的 HTTP 端点（管理 API / 下载上传票据通道），
// 构成 SSRF / 本地提权跳板。
var dockerfileHostNetRE = regexp.MustCompile(`(?i)--network\s*=?\s*host`)

// ---------------------------------------------------------------------------
// 基础校验
// ---------------------------------------------------------------------------

// isPosixAbs 判断路径是否以 / 开头（POSIX 绝对路径）。
// 容器宿主路径（Linux docker / FreeBSD jail）统一按 POSIX 语义解释，
// 故不能直接用 filepath.IsAbs —— 后者在 Windows 上把 "/data" 判为相对路径，
// 而容器能力在 Windows 上由平台桩统一返回 501，不参与真实校验。
func isPosixAbs(p string) bool { return strings.HasPrefix(p, "/") }

// validateContainerEnv 校验容器环境变量名：
//   - 名称不能为空，不能含空白 / NUL / '='（会被 docker CLI 解析成别的键）；
//   - 拒绝 LD_*：LD_PRELOAD / LD_LIBRARY_PATH / LD_AUDIT 等动态链接器变量
//     可让进程加载任意库，是经典的提权与逃逸跳板（红队审计 高-4）。
func validateContainerEnv(env map[string]string) error {
	for k := range env {
		if strings.TrimSpace(k) == "" {
			return errors.New("env 变量名不能为空")
		}
		if strings.ContainsAny(k, " \t\r\n\x00=") {
			return fmt.Errorf("env 变量名含非法字符: %q", k)
		}
		if strings.HasPrefix(strings.ToUpper(k), "LD_") {
			return fmt.Errorf("env 不允许设置动态链接器变量 %s（LD_* 可劫持进程加载的库）", k)
		}
	}
	return nil
}

// validateMACAddress 校验静态 MAC 地址格式（bastille create -M / jail.conf）。
func validateMACAddress(mac string) error {
	if !macAddressPattern.MatchString(strings.TrimSpace(mac)) {
		return fmt.Errorf("mac 不是合法的 MAC 地址（应为 02:00:00:00:00:01 形式）: %s", mac)
	}
	return nil
}

// validateContainerWorkdir 校验容器 / jail 工作目录：
//   - 必须为 POSIX 绝对路径（以 / 开头）：相对路径按进程 cwd 解析，语义漂移；
//   - 拒绝 NUL / \r / \n / 单引号：该值会经 `docker -w` 或 jail.conf 的
//     `exec.start = 'cd <workdir> && …'` 单引号字符串落地，换行与引号可注入配置；
//   - 拒绝 .. 段：避免逃出预期根目录。
func validateContainerWorkdir(workdir string) error {
	if workdir == "" {
		return nil
	}
	if strings.ContainsAny(workdir, "\x00\r\n'") {
		return fmt.Errorf("workdir 不能包含换行符或单引号: %q", workdir)
	}
	if !isPosixAbs(workdir) {
		return fmt.Errorf("workdir 必须为绝对路径（以 / 开头）: %s", workdir)
	}
	for _, seg := range strings.Split(workdir, "/") {
		if seg == ".." {
			return fmt.Errorf("workdir 不能包含 .. : %s", workdir)
		}
	}
	return nil
}

// validateContainerPath 校验容器（jail）内路径：POSIX 绝对路径 + 无 .. 段。
func validateContainerPath(label, p string) error {
	if p == "" {
		return fmt.Errorf("%s 不能为空", label)
	}
	if strings.ContainsAny(p, "\x00\r\n") {
		return fmt.Errorf("%s 含控制字符: %q", label, p)
	}
	if !isPosixAbs(p) {
		return fmt.Errorf("%s 必须为绝对路径（以 / 开头）: %s", label, p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("%s 不能包含 .. : %s", label, p)
		}
	}
	return nil
}

// validateCLIPositional 校验作为**位置参数**传给 docker / bastille 的值：
// 以 '-' 开头会被 CLI 当作选项解析（如 image="--privileged" 直接提权），
// 控制字符则可能截断参数。选项值（-p/-v/-e/-w/--name 的取值）由前一个
// 选项消费，不受此影响。
func validateCLIPositional(label, v string) error {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	if strings.HasPrefix(strings.TrimSpace(v), "-") {
		return fmt.Errorf("%s 不能以 - 开头（会被解析为命令行选项）: %s", label, v)
	}
	if strings.ContainsAny(v, "\x00\r\n") {
		return fmt.Errorf("%s 含控制字符", label)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 宿主机挂载源路径
// ---------------------------------------------------------------------------

// validateHostMountSource 校验绑定挂载的宿主机源路径（红队审计 高-4）：
//   - 必须为 POSIX 绝对路径（相对路径按守护进程 cwd 解析，语义漂移且可指向任意位置）；
//   - 不得为文件系统根目录（/）或 Windows 磁盘根；
//   - 不得位于系统目录内（forbiddenCwdDirs：/etc /proc /sys /root …）；
//   - 不得位于节点数据目录内（accounts.db 账户库 / 保险库 / instances.json）；
//   - 符号链接必须解析后仍满足以上约束（拒绝 /data/link → /etc 的穿透）。
//
// 关于审计建议「仅允许 {data} 下路径」：本项目实例工作目录被
// validateInstanceCwd 强制排除在数据目录之外，而数据目录本身存放凭据，
// 故采用等价的「拒绝根目录 + 拒绝系统目录 + 拒绝数据目录 + 拒绝符号链接穿透」，
// 既封堵宿主机 root 暴露，又不误伤「把实例目录挂进容器」的正常用法。
func (d *Daemon) validateHostMountSource(src string) error {
	src = strings.TrimSpace(src)
	if src == "" {
		return errors.New("绑定挂载必须提供宿主机源路径")
	}
	if strings.ContainsAny(src, "\x00\r\n") {
		return fmt.Errorf("宿主机源路径含控制字符: %q", src)
	}
	if !isPosixAbs(src) {
		return fmt.Errorf("绑定挂载的宿主机源路径必须为绝对路径: %s", src)
	}
	if err := d.checkHostPathAllowed(src); err != nil {
		return err
	}
	// 符号链接穿透检查：目标若含软链，解析后必须仍满足同一组约束。
	resolved, err := resolveExistingPrefix(src)
	if err != nil {
		return err
	}
	if resolved != src {
		if err := d.checkHostPathAllowed(resolved); err != nil {
			return fmt.Errorf("宿主机源路径经符号链接解析后越界（%s → %s）", src, resolved)
		}
	}
	return nil
}

// checkHostPathAllowed 校验宿主机绝对路径不落在根目录 / 系统目录 / 节点数据目录内。
func (d *Daemon) checkHostPathAllowed(p string) error {
	cleaned := filepath.Clean(p)
	if cleaned == string(filepath.Separator) || cleaned == "/" || cleaned == `\` {
		return fmt.Errorf("不允许挂载文件系统根目录: %s", p)
	}
	if isDriveRoot(cleaned) {
		return fmt.Errorf("不允许挂载磁盘根目录: %s", p)
	}
	abs := cleaned
	if !filepath.IsAbs(abs) {
		if a, err := filepath.Abs(abs); err == nil {
			abs = a
		}
	}
	for _, bad := range forbiddenCwdDirs() {
		base := bad
		if runtime.GOOS == "windows" {
			vol := filepath.VolumeName(abs)
			if vol == "" {
				continue
			}
			base = filepath.Clean(vol + bad)
		}
		if pathWithin(base, abs) {
			// 与 validateCwd 一致：Windows 的 \Users 整树拒绝，但豁免系统临时
			// 目录（通常为 <用户>\AppData\Local\Temp）——部署与测试依赖它。
			if runtime.GOOS == "windows" && bad == `\Users` && pathWithin(os.TempDir(), abs) {
				continue
			}
			return fmt.Errorf("不允许挂载系统目录: %s", p)
		}
	}
	if d != nil && d.DataDir != "" {
		if dd, err := filepath.Abs(d.DataDir); err == nil {
			if pathWithin(filepath.Clean(dd), abs) {
				return fmt.Errorf("不允许挂载节点数据目录（账户库/保险库/实例表所在）: %s", p)
			}
		}
	}
	return nil
}

// validateContainerVolume 校验 docker `-v` 形式的卷规格 "源:容器路径[:ro|rw]"。
// 源为绝对路径 → 绑定挂载（走宿主机路径校验）；否则必须是合法命名卷。
func (d *Daemon) validateContainerVolume(spec string) error {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil // 空条目由 dockerCreate 忽略
	}
	parts := strings.Split(spec, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return fmt.Errorf("volumes 条目格式错误（应为 宿主路径:容器路径[:ro|rw]）: %s", spec)
	}
	src := strings.TrimSpace(parts[0])
	dest := strings.TrimSpace(parts[1])
	if src == "" {
		return fmt.Errorf("volumes 条目缺少宿主机源路径或卷名: %s", spec)
	}
	if isPosixAbs(src) {
		if err := d.validateHostMountSource(src); err != nil {
			return err
		}
	} else if !volumeNamePattern.MatchString(src) {
		return fmt.Errorf("volumes 宿主机源必须是绝对路径或合法卷名（%s）: %s", volumeNamePattern.String(), src)
	}
	if err := validateContainerPath("容器内目标路径", dest); err != nil {
		return err
	}
	if len(parts) == 3 {
		switch strings.TrimSpace(parts[2]) {
		case "", "ro", "rw":
		default:
			return fmt.Errorf("volumes 模式仅支持 ro/rw: %s", spec)
		}
	}
	return nil
}

// validateContainerVolumes 逐条校验 docker 卷规格列表。
func (d *Daemon) validateContainerVolumes(specs []string) error {
	for _, s := range specs {
		if err := d.validateContainerVolume(s); err != nil {
			return err
		}
	}
	return nil
}

// validateBastilleVolume 校验 bastille 挂载对 "宿主机路径:jail内路径"。
// jail 侧不支持命名卷，源必须是绝对宿主路径。
func (d *Daemon) validateBastilleVolume(src, dest string) error {
	if err := d.validateHostMountSource(src); err != nil {
		return err
	}
	return validateContainerPath("jail 内目标路径", dest)
}

// ---------------------------------------------------------------------------
// 镜像构建
// ---------------------------------------------------------------------------

// validateDockerfileSafety 拒绝 Dockerfile 中突破构建隔离的 BuildKit 指令
// （红队审计 高-4：镜像构建端点等价宿主机 root，需禁用 BuildKit 绑定挂载）。
// 该检查是纵深防御：构建端点同时已收归管理员（requireAdmin）。
func validateDockerfileSafety(dockerfile string) error {
	for _, m := range dockerfileMountRE.FindAllStringSubmatch(dockerfile, -1) {
		typ := strings.ToLower(m[1])
		if dockerfileBlockedMountTypes[typ] {
			return fmt.Errorf("Dockerfile 不允许使用 BuildKit 的 RUN --mount=type=%s（可越过构建隔离取用宿主/凭据）", typ)
		}
	}
	if m := dockerfileInsecureRE.FindString(dockerfile); m != "" {
		return fmt.Errorf("Dockerfile 不允许使用 --security=insecure（%s）", strings.TrimSpace(m))
	}
	if m := dockerfileHostNetRE.FindString(dockerfile); m != "" {
		return fmt.Errorf("Dockerfile 不允许使用 --network=host（构建期可访问节点本机服务，%s）", strings.TrimSpace(m))
	}
	return nil
}
