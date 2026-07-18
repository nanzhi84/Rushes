package media

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type CommandResult struct {
	Stdout []byte
	Stderr []byte
}

type Progress struct {
	OutTime time.Duration
	Done    bool
}

type CommandError struct {
	Name   string
	Stderr string
	Err    error
}

func (err *CommandError) Error() string {
	if err.Stderr == "" {
		return fmt.Sprintf("%s 执行失败: %v", err.Name, err.Err)
	}
	return fmt.Sprintf("%s 执行失败: %v: %s", err.Name, err.Err, err.Stderr)
}

func (err *CommandError) Unwrap() error { return err.Err }

// ffmpegProtocolWhitelist 收窄 ffmpeg/ffprobe 可访问的协议面。恶意媒体（如引用
// http:// 分片的 m3u8/播放列表、拼接协议）能让底层进程发起网络请求或读取任意本地
// 文件——攻击者只需控制文件内容，无需控制参数。仓库内所有 ffmpeg/ffprobe 的输入
// 都只是本地文件（-i 或位置参数）加 stdin/stdout 管道（-progress pipe:1、-f null -、
// 滤镜 file=-），因此 file,pipe 足以覆盖全部合法用途，其余协议一律拒绝。
const ffmpegProtocolWhitelist = "file,pipe"

// isFFmpegFamily 按可执行文件基名判断是否为 ffmpeg 或 ffprobe，容忍绝对路径与
// Windows 的 .exe 后缀。fc-scan、aubiotrack、aubioonset 等其它外部命令不在此列，
// 不会被注入协议白名单。
func isFFmpegFamily(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	base = strings.TrimSuffix(base, ".exe")
	return base == "ffmpeg" || base == "ffprobe"
}

// injectProtocolWhitelist 在 ffmpeg/ffprobe 参数中统一注入 -protocol_whitelist，作为
// 所有媒体子进程的单一收口，避免各调用点散落遗漏。-protocol_whitelist 是 per-input
// 选项，必须位于对应输入之前才对该输入生效，且会被上一个 -i 消费，因此在每个 -i 前
// 各注入一次；ffprobe 以位置参数作为输入，则在参数最前面注入一次即可覆盖。非
// ffmpeg/ffprobe 命令原样返回；调用方若已显式指定 -protocol_whitelist，则尊重其设置、
// 不再注入。
// 前提：本函数按裸 -i token 判定输入位；若某个选项的取值恰好是字符串 "-i"
// （形如 [flag, "-i"] 的三段式用法），会被误判为输入位而多注入一次白名单——
// 当前全部调用点均无此形态。
func injectProtocolWhitelist(name string, args []string) []string {
	if !isFFmpegFamily(name) {
		return args
	}
	inputs := 0
	for _, arg := range args {
		switch arg {
		case "-protocol_whitelist":
			return args
		case "-i":
			inputs++
		}
	}
	guard := []string{"-protocol_whitelist", ffmpegProtocolWhitelist}
	if inputs == 0 {
		return append(append(make([]string, 0, len(args)+len(guard)), guard...), args...)
	}
	result := make([]string, 0, len(args)+len(guard)*inputs)
	for _, arg := range args {
		if arg == "-i" {
			result = append(result, guard...)
		}
		result = append(result, arg)
	}
	return result
}

func RunCommand(ctx context.Context, name string, args ...string) (CommandResult, error) {
	plan := planSandbox(name, injectProtocolWhitelist(name, args))
	defer plan.cleanup()
	command := exec.CommandContext(ctx, plan.name, plan.args...)
	configureProcess(command)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return CommandResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, &CommandError{
			Name: name, Stderr: stderrSummary(stderr.String()), Err: err,
		}
	}
	return CommandResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, nil
}

// RunFFmpegProgress consumes stdout completely before Wait and parses ffmpeg's
// machine-readable -progress blocks. Callers must include no -progress option.
func RunFFmpegProgress(
	ctx context.Context,
	ffmpeg string,
	args []string,
	onProgress func(Progress),
) error {
	progressArgs := append([]string{"-progress", "pipe:1", "-nostats", "-loglevel", "error"}, args...)
	plan := planSandbox(ffmpeg, injectProtocolWhitelist(ffmpeg, progressArgs))
	defer plan.cleanup()
	command := exec.CommandContext(ctx, plan.name, plan.args...)
	configureProcess(command)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return err
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	current := Progress{}
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if !ok {
			continue
		}
		switch key {
		case "out_time_us":
			microseconds, parseErr := strconv.ParseInt(value, 10, 64)
			if parseErr == nil {
				current.OutTime = time.Duration(microseconds) * time.Microsecond
			}
		case "progress":
			current.Done = value == "end"
			if onProgress != nil {
				onProgress(current)
			}
		}
	}
	scanErr := scanner.Err()
	waitErr := command.Wait()
	if scanErr != nil {
		return scanErr
	}
	if waitErr != nil {
		return &CommandError{
			Name: ffmpeg, Stderr: stderrSummary(stderr.String()), Err: waitErr,
		}
	}
	return nil
}

func stderrSummary(raw string) string {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	return strings.Join(lines, "\n")
}
