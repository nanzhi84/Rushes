package media

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
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

func RunCommand(ctx context.Context, name string, args ...string) (CommandResult, error) {
	command := exec.CommandContext(ctx, name, args...)
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
	command := exec.CommandContext(ctx, ffmpeg, progressArgs...)
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
