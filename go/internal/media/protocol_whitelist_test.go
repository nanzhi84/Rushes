package media

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	whitelistFlag  = "-protocol_whitelist"
	whitelistValue = "file,pipe"
)

func countFlag(args []string, flag string) int {
	n := 0
	for _, a := range args {
		if a == flag {
			n++
		}
	}
	return n
}

func TestInjectProtocolWhitelistGuardsEachInput(t *testing.T) {
	assertGuardedInputs := func(t *testing.T, args []string, wantInputs int) {
		t.Helper()
		got := 0
		for i, a := range args {
			if a != "-i" {
				continue
			}
			got++
			if i < 2 || args[i-2] != whitelistFlag || args[i-1] != whitelistValue {
				t.Fatalf("每个 -i 前应紧邻 %s %s，实际 args=%v（第 %d 项）", whitelistFlag, whitelistValue, args, i)
			}
		}
		if got != wantInputs {
			t.Fatalf("应有 %d 个 -i，实际 %d：%v", wantInputs, got, args)
		}
	}

	single := injectProtocolWhitelist("ffmpeg", []string{"-y", "-i", "in.mp4", "-frames:v", "1", "out.jpg"})
	assertGuardedInputs(t, single, 1)

	multi := injectProtocolWhitelist("ffmpeg", []string{
		"-loop", "1", "-t", "3", "-i", "a.png",
		"-ss", "0", "-t", "5", "-i", "b.mp4",
		"-i", "c.wav", "-filter_complex", "concat=n=2:v=1:a=0", "out.mp4",
	})
	assertGuardedInputs(t, multi, 3)
	if n := countFlag(multi, whitelistFlag); n != 3 {
		t.Fatalf("三个 -i 应注入三次白名单，实际 %d 次：%v", n, multi)
	}

	prog := injectProtocolWhitelist("ffmpeg", []string{"-progress", "pipe:1", "-nostats", "-loglevel", "error", "-i", "in.mp4", "out.mp4"})
	assertGuardedInputs(t, prog, 1)
	if prog[0] != "-progress" {
		t.Fatalf("不应改动 -progress 等前置全局参数：%v", prog)
	}
}

func TestInjectProtocolWhitelistProbePositional(t *testing.T) {
	probe := injectProtocolWhitelist("ffprobe", []string{"-v", "error", "-show_format", "-show_streams", "clip.mp4"})
	if len(probe) < 2 || probe[0] != whitelistFlag || probe[1] != whitelistValue {
		t.Fatalf("ffprobe 无 -i 时白名单应置于参数最前，实际：%v", probe)
	}
	if n := countFlag(probe, whitelistFlag); n != 1 {
		t.Fatalf("ffprobe 应只注入一次，实际 %d 次：%v", n, probe)
	}
	if probe[len(probe)-1] != "clip.mp4" {
		t.Fatalf("输入位置参数不应被移动：%v", probe)
	}
}

func TestInjectProtocolWhitelistBasename(t *testing.T) {
	for _, name := range []string{"/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffprobe", "ffmpeg.exe", "FFprobe.EXE"} {
		got := injectProtocolWhitelist(name, []string{"-i", "x", "y"})
		if len(got) < 3 || got[0] != whitelistFlag || got[1] != whitelistValue || got[2] != "-i" {
			t.Fatalf("%s 应被识别为 ffmpeg 家族并在 -i 前注入白名单，实际：%v", name, got)
		}
	}
}

func TestInjectProtocolWhitelistSkipsNonFFmpeg(t *testing.T) {
	for _, name := range []string{"fc-scan", "aubiotrack", "aubioonset", "ffmpegx", "myffprobe", "ffprobe-wrapper"} {
		in := []string{"-i", "in.wav", "-O", "specflux"}
		got := injectProtocolWhitelist(name, in)
		if countFlag(got, whitelistFlag) != 0 {
			t.Fatalf("%s 不应被注入协议白名单，实际：%v", name, got)
		}
		if len(got) != len(in) {
			t.Fatalf("%s 参数应原样返回，实际：%v", name, got)
		}
	}
}

func TestInjectProtocolWhitelistRespectsExplicit(t *testing.T) {
	explicit := []string{"-protocol_whitelist", "file,crypto,data", "-i", "in.m3u8", "out.mp4"}
	got := injectProtocolWhitelist("ffmpeg", explicit)
	if countFlag(got, whitelistFlag) != 1 {
		t.Fatalf("已显式指定白名单时不应重复注入：%v", got)
	}
	if got[1] != "file,crypto,data" {
		t.Fatalf("应保留调用方的显式白名单值，实际：%v", got)
	}
}

func requireFFmpegBinary(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("本机缺少 %s，跳过真实协议白名单拒绝用例", name)
	}
}

func maliciousPlaylistPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", "malicious_http.m3u8"))
	if err != nil {
		t.Fatalf("解析恶意 fixture 路径失败: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("恶意 fixture 缺失: %v", err)
	}
	return path
}

func assertDiagnosableRejection(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("引用 http:// 分片的恶意播放列表应被协议白名单拒绝，实际无错误")
	}
	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		t.Fatalf("错误应为 *CommandError 以携带可诊断的 stderr，实际 %T: %v", err, err)
	}
	summary := strings.TrimSpace(commandErr.Stderr)
	if summary == "" {
		t.Fatal("stderr 摘要为空，拒绝原因不可诊断")
	}
	lower := strings.ToLower(summary)
	if !strings.Contains(lower, "http") && !strings.Contains(lower, "whitelist") && !strings.Contains(lower, "protocol") {
		t.Fatalf("stderr 摘要未提及协议/白名单，可诊断性存疑：%s", summary)
	}
}

func TestProbeFileRejectsHTTPPlaylist(t *testing.T) {
	requireFFmpegBinary(t, "ffprobe")
	_, err := ProbeFile(context.Background(), maliciousPlaylistPath(t))
	assertDiagnosableRejection(t, err)
}

func TestRunFFmpegProgressRejectsHTTPPlaylist(t *testing.T) {
	requireFFmpegBinary(t, "ffmpeg")
	out := filepath.Join(t.TempDir(), "thumb.jpg")
	err := RunFFmpegProgress(context.Background(), "ffmpeg",
		[]string{"-y", "-i", maliciousPlaylistPath(t), "-frames:v", "1", out}, nil)
	assertDiagnosableRejection(t, err)
}

func TestMediaExecConfinedToProcessWrapper(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读取 media 包目录失败: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") || name == "process.go" {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		if bytes.Contains(data, []byte("exec.Command")) {
			t.Errorf("%s 直接调用 exec.Command，绕过了 process.go 的协议白名单封装；请改用 RunCommand/RunFFmpegProgress", name)
		}
	}
}

func TestNoRawFFmpegExecInRepo(t *testing.T) {
	goRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("定位 go 模块根失败: %v", err)
	}
	walkErr := filepath.WalkDir(goRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "testdata", "node_modules", ".git":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !bytes.Contains(data, []byte("exec.Command")) {
			return nil
		}
		if !bytes.Contains(data, []byte("ffmpeg")) && !bytes.Contains(data, []byte("ffprobe")) {
			return nil
		}
		if filepath.Base(path) == "process.go" && filepath.Base(filepath.Dir(path)) == "media" {
			return nil
		}
		t.Errorf("%s 直接 exec 执行 ffmpeg/ffprobe，绕过了 media/process.go 的协议白名单封装", path)
		return nil
	})
	if walkErr != nil {
		t.Fatalf("遍历 go 模块源码失败: %v", walkErr)
	}
}
