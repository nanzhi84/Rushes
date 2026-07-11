package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nanzhi84/Rushes/go/internal/contracts"
	"github.com/nanzhi84/Rushes/go/internal/reducer"
	"github.com/nanzhi84/Rushes/go/internal/storage"
)

type RunnerConfig struct {
	Database          *storage.DB
	Registry          *Registry
	WorkerID          string
	Concurrency       int
	PollInterval      time.Duration
	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration
	Logger            *slog.Logger
	Now               func() time.Time
}

type Runner struct {
	database          *storage.DB
	registry          *Registry
	workerID          string
	concurrency       int
	pollInterval      time.Duration
	heartbeatInterval time.Duration
	heartbeatTimeout  time.Duration
	logger            *slog.Logger
	now               func() time.Time
}

func NewRunner(config RunnerConfig) (*Runner, error) {
	if config.Database == nil || config.Registry == nil {
		return nil, errors.New("worker 缺少数据库或 registry")
	}
	if config.WorkerID == "" {
		config.WorkerID = fmt.Sprintf("worker_%d", time.Now().UnixNano())
	}
	if config.Concurrency <= 0 {
		config.Concurrency = 2
	}
	if config.PollInterval <= 0 {
		config.PollInterval = 250 * time.Millisecond
	}
	if config.HeartbeatInterval <= 0 {
		config.HeartbeatInterval = 5 * time.Second
	}
	if config.HeartbeatTimeout <= 0 {
		config.HeartbeatTimeout = 60 * time.Second
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Runner{
		database: config.Database, registry: config.Registry, workerID: config.WorkerID,
		concurrency: config.Concurrency, pollInterval: config.PollInterval,
		heartbeatInterval: config.HeartbeatInterval, heartbeatTimeout: config.HeartbeatTimeout,
		logger: config.Logger, now: config.Now,
	}, nil
}

func (runner *Runner) Run(ctx context.Context) error {
	if _, err := RecoverStale(ctx, runner.database, runner.now(), runner.heartbeatTimeout); err != nil {
		return fmt.Errorf("恢复超时 job: %w", err)
	}
	var group sync.WaitGroup
	for range runner.concurrency {
		group.Add(1)
		go func() {
			defer group.Done()
			runner.loop(ctx)
		}()
	}
	<-ctx.Done()
	group.Wait()
	return nil
}

func (runner *Runner) loop(ctx context.Context) {
	ticker := time.NewTicker(runner.pollInterval)
	defer ticker.Stop()
	for {
		worked, err := runner.RunOnce(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			runner.logger.Error("worker job 执行失败", "error", err)
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (runner *Runner) RunOnce(ctx context.Context) (bool, error) {
	job, err := Claim(ctx, runner.database, runner.workerID, runner.now())
	if err != nil || job == nil {
		return false, err
	}
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	heartbeatDone := make(chan struct{})
	go runner.heartbeat(jobCtx, cancel, *job, heartbeatDone)

	handler, handlerErr := runner.registry.Require(job.Kind)
	var result map[string]any
	if handlerErr == nil {
		result, handlerErr = handler(jobCtx, *job, runner.reportProgress)
	}
	cancel()
	<-heartbeatDone
	stored, statusErr := GetJob(ctx, runner.database, job.ID)
	if statusErr != nil {
		return true, statusErr
	}
	if stored.Status != "running" {
		return true, nil
	}
	if handlerErr != nil {
		failure := failureJSON(handlerErr)
		if !errors.Is(handlerErr, context.Canceled) && job.Attempts+1 <= job.MaxRetries {
			scheduled, retryErr := ScheduleRetry(ctx, runner.database, *job, runner.now(), failure)
			if retryErr != nil {
				return true, retryErr
			}
			if scheduled {
				return true, nil
			}
		}
		terminalErr := runner.emitTerminal(ctx, *job, "JobFailed", nil, failure)
		if errors.Is(terminalErr, reducer.ErrJobCancelled) {
			return true, nil
		}
		return true, terminalErr
	}
	terminalErr := runner.emitTerminal(ctx, *job, "JobSucceeded", result, nil)
	if errors.Is(terminalErr, reducer.ErrJobCancelled) {
		return true, nil
	}
	return true, terminalErr
}

func (runner *Runner) heartbeat(
	ctx context.Context,
	cancel context.CancelFunc,
	job Job,
	done chan<- struct{},
) {
	defer close(done)
	ticker := time.NewTicker(runner.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok, err := Heartbeat(ctx, runner.database, job.ID, runner.workerID, runner.now())
			if err != nil || !ok {
				cancel()
				return
			}
		}
	}
}

func (runner *Runner) reportProgress(ctx context.Context, job Job, progress float64) error {
	progress = max(0, min(progress, 1))
	_, err := reducer.Apply(ctx, runner.database, []contracts.Event{{
		Type: "JobProgress", DraftID: value(job.DraftID),
		Payload: map[string]any{
			"job_id": job.ID, "kind": job.Kind, "asset_id": value(job.AssetID),
			"requested_by_draft_id": value(job.RequestedByDraftID), "progress": progress,
		},
	}}, reducer.Options{Actor: contracts.ActorJob})
	return err
}

func (runner *Runner) emitTerminal(
	ctx context.Context,
	job Job,
	eventType string,
	result map[string]any,
	failure map[string]any,
) error {
	applyResult, err := reducer.Apply(ctx, runner.database, []contracts.Event{{
		Type: eventType, DraftID: value(job.DraftID),
		Payload: map[string]any{
			"job_id": job.ID, "kind": job.Kind, "asset_id": value(job.AssetID),
			"requested_by_draft_id": value(job.RequestedByDraftID),
			"progress":              1.0, "result": result, "error": failure,
		},
	}}, reducer.Options{Actor: contracts.ActorJob})
	if err != nil {
		return err
	}
	if applyResult.Status != reducer.StatusApplied {
		return fmt.Errorf("job 终态写入失败: %s", applyResult.Status)
	}
	return nil
}

func failureJSON(err error) map[string]any {
	return map[string]any{
		"error_code": "job_handler_failed", "message": err.Error(),
		"retryable": !errors.Is(err, context.Canceled),
	}
}

func value(pointer *string) string {
	if pointer == nil {
		return ""
	}
	return *pointer
}
