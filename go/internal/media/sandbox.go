package media

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// ffmpeg Seatbelt 沙箱是 X1 收窄协议面之后的 OS 级第二道兜底：即便解码器漏洞被恶意
// 样本触发，被 sandbox-exec 关进 Seatbelt 的 ffmpeg/ffprobe 也无法联网、无法写对象库
// 以外的路径、无法读用户家目录里的私密文件。封装点与 X1 同在 process.go 的 RunCommand
// 与 RunFFmpegProgress，作为所有媒体子进程的单一收口。

const seatbeltSystemProfile = "/System/Library/Sandbox/Profiles/system.sb"

// 由 main 启动时按对象库布局注入的允许根（配置推导）。素材引用文件（reference 模式，
// 路径因用户而异）在每次调用时从参数推导补进只读集。
var (
	sandboxRootsMu    sync.RWMutex
	sandboxReadRoots  []string
	sandboxWriteRoots []string
)

// ConfigureFFmpegSandbox 注册对象库允许根。readRoots 一般是 objects/tmp/segments/cache，
// writeRoots 一般只有 tmp（所有 ffmpeg 输出都落在对象库临时目录）。传入路径会被解析为
// 内核真实路径（EvalSymlinks）——Seatbelt 按解析后的真实路径匹配（macOS 上 /tmp 是
// /private/tmp 的符号链接，不解析会导致 subpath 规则永不命中）。
func ConfigureFFmpegSandbox(readRoots, writeRoots []string) {
	sandboxRootsMu.Lock()
	defer sandboxRootsMu.Unlock()
	sandboxReadRoots = resolveSandboxPaths(readRoots)
	sandboxWriteRoots = resolveSandboxPaths(writeRoots)
}

// 可注入探测点，便于测试强制走降级分支而不依赖真实平台。
var (
	sandboxGOOS     = runtime.GOOS
	sandboxLookPath = exec.LookPath
)

var sandboxWarnOnce sync.Once

// ffmpegSandboxRequested 报告是否启用沙箱。RUSHES_FFMPEG_SANDBOX 默认开；显式设为
// 0/false/off/no 才关闭。
func ffmpegSandboxRequested() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("RUSHES_FFMPEG_SANDBOX"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

type sandboxPlan struct {
	name    string
	args    []string
	cleanup func()
}

func noopCleanup() {}

// planSandbox 决定是否用 Seatbelt 包住 ffmpeg/ffprobe 调用：
//   - 非 ffmpeg 家族命令：原样直跑；
//   - 显式关闭：原样直跑（用户选择，不记日志）；
//   - 开启但不可用（非 darwin / 缺 sandbox-exec / profile 落盘失败）：降级直跑并记一条
//     警告日志（非静默降级），失去 OS 级兜底但不影响功能。
func planSandbox(name string, args []string) sandboxPlan {
	passthrough := sandboxPlan{name: name, args: args, cleanup: noopCleanup}
	if !isFFmpegFamily(name) || !ffmpegSandboxRequested() {
		return passthrough
	}
	if sandboxGOOS != "darwin" {
		warnSandboxDegraded("当前平台非 macOS，Seatbelt 不可用")
		return passthrough
	}
	if _, err := sandboxLookPath("sandbox-exec"); err != nil {
		warnSandboxDegraded("未找到 sandbox-exec")
		return passthrough
	}
	sandboxRootsMu.RLock()
	configured := len(sandboxReadRoots) > 0 || len(sandboxWriteRoots) > 0
	sandboxRootsMu.RUnlock()
	if !configured {
		// 未注入对象库允许根（尚未 ConfigureFFmpegSandbox，或在不涉及对象库的测试里）：
		// 沙箱无从界定可写/可读范围，直跑而非用空 profile 拒死一切。生产由 main 注入根。
		return passthrough
	}
	execPath, ok := resolveSandboxExecutable(name)
	if !ok {
		warnSandboxDegraded("无法解析 " + name + " 的可执行文件路径（PATH 未找到）")
		return passthrough
	}
	profile := buildSeatbeltProfile(name, args)
	file, err := os.CreateTemp("", "rushes-ffmpeg-*.sb")
	if err != nil {
		warnSandboxDegraded("创建 Seatbelt profile 临时文件失败: " + err.Error())
		return passthrough
	}
	profilePath := file.Name()
	if _, err := file.WriteString(profile); err != nil {
		_ = file.Close()
		_ = os.Remove(profilePath)
		warnSandboxDegraded("写入 Seatbelt profile 失败: " + err.Error())
		return passthrough
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(profilePath)
		warnSandboxDegraded("关闭 Seatbelt profile 失败: " + err.Error())
		return passthrough
	}
	wrapped := make([]string, 0, len(args)+3)
	wrapped = append(wrapped, "-f", profilePath, execPath)
	wrapped = append(wrapped, args...)
	return sandboxPlan{name: "sandbox-exec", args: wrapped, cleanup: func() { _ = os.Remove(profilePath) }}
}

func warnSandboxDegraded(reason string) {
	sandboxWarnOnce.Do(func() {
		slog.Warn("ffmpeg Seatbelt 沙箱不可用，降级为直接执行（媒体解码将失去 OS 级隔离兜底）", "reason", reason)
	})
}

// buildSeatbeltProfile 按调用生成 SBPL profile，所有路径解析为内核真实路径。写默认全拒、
// 仅放行对象库临时目录与进程临时目录；读拒绝用户家目录（私密文件），仅放行对象库、素材
// 引用文件与 ffmpeg 二进制目录；network 全拒。
func buildSeatbeltProfile(name string, args []string) string {
	sandboxRootsMu.RLock()
	readRoots := append([]string(nil), sandboxReadRoots...)
	writeRoots := append([]string(nil), sandboxWriteRoots...)
	sandboxRootsMu.RUnlock()

	inputs := resolveSandboxPaths(sandboxInputPaths(name, args))
	var binDirs []string
	if dir, ok := resolveExecutableDir(name); ok {
		binDirs = append(binDirs, dir)
	}
	tmpDir := resolveSandboxPath(os.TempDir())
	home := resolveSandboxPath(sandboxHomeDir())

	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(import \"" + seatbeltSystemProfile + "\")\n")
	b.WriteString("(allow process-fork)\n")
	b.WriteString("(allow process-exec*)\n")
	b.WriteString("(deny network*)\n")
	b.WriteString("(deny file-write*)\n")
	b.WriteString("(allow file-write*\n")
	b.WriteString("  (literal \"/dev/null\") (literal \"/dev/stdout\") (literal \"/dev/stderr\")\n")
	writeSandboxSubpath(&b, tmpDir)
	for _, r := range writeRoots {
		writeSandboxSubpath(&b, r)
	}
	b.WriteString(")\n")
	if home != "" {
		b.WriteString("(deny file-read* (subpath " + sandboxQuote(home) + "))\n")
	}
	b.WriteString("(allow file-read*\n")
	for _, d := range binDirs {
		writeSandboxSubpath(&b, d)
	}
	// 常见包管理器前缀：动态链接的 ffmpeg（如 CI 的 brew ffmpeg-full）二进制与其依赖
	// dylib 都在这些前缀下，需可读方能启动。仅含已安装软件、无用户私密数据。
	for _, prefix := range []string{"/opt/homebrew", "/usr/local", "/opt/local"} {
		writeSandboxSubpath(&b, resolveSandboxPath(prefix))
	}
	for _, r := range readRoots {
		writeSandboxSubpath(&b, r)
	}
	for _, in := range inputs {
		b.WriteString("  (literal " + sandboxQuote(in) + ")\n")
	}
	b.WriteString(")\n")
	return b.String()
}

// sandboxInputPaths 从（已注入协议白名单的）参数里提取输入文件：每个 -i 的取值，以及
// ffprobe 的位置参数输入。管道/stdin（-、pipe:）不是文件，跳过。
func sandboxInputPaths(name string, args []string) []string {
	var inputs []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-i" && i+1 < len(args) {
			if candidate := args[i+1]; isSandboxFilePath(candidate) {
				inputs = append(inputs, candidate)
			}
			i++
		}
	}
	if sandboxBaseName(name) == "ffprobe" && len(args) > 0 {
		if last := args[len(args)-1]; isSandboxFilePath(last) {
			inputs = append(inputs, last)
		}
	}
	return inputs
}

func isSandboxFilePath(value string) bool {
	if value == "" || value == "-" {
		return false
	}
	if strings.HasPrefix(value, "-") || strings.HasPrefix(value, "pipe:") {
		return false
	}
	return true
}

func sandboxBaseName(name string) string {
	base := strings.ToLower(filepath.Base(name))
	return strings.TrimSuffix(base, ".exe")
}

func writeSandboxSubpath(b *strings.Builder, path string) {
	if path == "" {
		return
	}
	b.WriteString("  (subpath " + sandboxQuote(path) + ")\n")
}

// sandboxQuote 把路径编码为 SBPL 双引号字符串（转义反斜杠与引号）。
func sandboxQuote(path string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "\"", "\\\"")
	return "\"" + replacer.Replace(path) + "\""
}

func resolveExecutableDir(name string) (string, bool) {
	resolved, err := sandboxLookPath(name)
	if err != nil {
		return "", false
	}
	if real, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = real
	}
	dir := filepath.Dir(resolved)
	if dir == "" || dir == "." {
		return "", false
	}
	return dir, true
}

func resolveSandboxExecutable(name string) (string, bool) {
	resolved, err := sandboxLookPath(name)
	if err != nil {
		return "", false
	}
	if real, err := filepath.EvalSymlinks(resolved); err == nil {
		return real, true
	}
	if abs, err := filepath.Abs(resolved); err == nil {
		return abs, true
	}
	return resolved, true
}

func sandboxHomeDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}

func resolveSandboxPaths(paths []string) []string {
	resolved := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		clean := resolveSandboxPath(path)
		if clean == "" {
			continue
		}
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		resolved = append(resolved, clean)
	}
	return resolved
}

// resolveSandboxPath 解析为内核真实路径；路径尚不存在时退回 Abs+Clean。
func resolveSandboxPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}
