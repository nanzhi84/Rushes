package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
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
	if config.HeartbeatInterval > config.HeartbeatTimeout/2 {
		return nil, errors.New("worker heartbeat interval 必须不超过 timeout 的一半")
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
	group.Add(1)
	go func() {
		defer group.Done()
		runner.recoverLoop(ctx)
	}()
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

func (runner *Runner) recoverLoop(ctx context.Context) {
	ticker := time.NewTicker(runner.heartbeatTimeout)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := RecoverStale(ctx, runner.database, runner.now(), runner.heartbeatTimeout); err != nil &&
				!errors.Is(err, context.Canceled) {
				runner.logger.Error("周期恢复超时 job 失败", "error", err)
			}
		}
	}
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
		result, handlerErr = invokeHandler(handler, jobCtx, *job, runner.newProgressReporter())
	}
	cancel()
	<-heartbeatDone
	stored, statusErr := GetJob(ctx, runner.database, job.ID)
	if statusErr != nil {
		return true, statusErr
	}
	if stored.Status != "running" || !sameClaim(stored, *job) {
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
		if errors.Is(terminalErr, reducer.ErrJobCancelled) || errors.Is(terminalErr, reducer.ErrJobClaimLost) {
			return true, nil
		}
		return true, terminalErr
	}
	terminalErr := runner.emitTerminal(ctx, *job, "JobSucceeded", result, nil)
	if errors.Is(terminalErr, reducer.ErrJobCancelled) || errors.Is(terminalErr, reducer.ErrJobClaimLost) {
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
			ok, err := Heartbeat(ctx, runner.database, job, runner.now())
			if err != nil || !ok {
				cancel()
				return
			}
		}
	}
}

func (runner *Runner) newProgressReporter() ProgressReporter {
	var sent bool
	var sequence int64
	var lastAt time.Time
	var lastProgress float64
	var lastMetadata ProgressUpdate
	return func(ctx context.Context, job Job, update ProgressUpdate) error {
		update.Progress = max(0, min(update.Progress, 1))
		now := runner.now()
		metadataChanged := progressHasMetadata(update) && !progressMetadataEqual(update, lastMetadata)
		if sent && update.Progress != 1 && now.Sub(lastAt) < time.Second &&
			math.Abs(update.Progress-lastProgress) < 0.01 &&
			!metadataChanged {
			return nil
		}
		payload := map[string]any{
			"job_id": job.ID, "kind": job.Kind, "asset_id": value(job.AssetID),
			"requested_by_draft_id": value(job.RequestedByDraftID), "progress": update.Progress,
			"worker_id": value(job.WorkerID), "started_at": value(job.StartedAt),
			"update_id": fmt.Sprintf("%s:%s:%d", job.ID, value(job.StartedAt), sequence+1),
		}
		if update.CurrentAssetID != "" {
			payload["current_asset_id"] = update.CurrentAssetID
		}
		if update.Total > 0 {
			payload["done"] = update.Done
			payload["total"] = update.Total
		}
		if update.Stage != "" {
			payload["stage"] = update.Stage
		}
		if update.Detail != "" {
			payload["detail"] = update.Detail
		}
		_, err := reducer.Apply(ctx, runner.database, []contracts.Event{{
			Type: "JobProgress", DraftID: value(job.DraftID), Payload: payload,
		}}, reducer.Options{Actor: contracts.ActorJob})
		if err == nil {
			sequence++
			sent = true
			lastAt = now
			lastProgress = update.Progress
			if progressHasMetadata(update) {
				lastMetadata = update
			}
		}
		return err
	}
}

func progressMetadataEqual(left, right ProgressUpdate) bool {
	return left.CurrentAssetID == right.CurrentAssetID && left.Done == right.Done &&
		left.Total == right.Total && left.Stage == right.Stage && left.Detail == right.Detail
}

func progressHasMetadata(update ProgressUpdate) bool {
	return update.CurrentAssetID != "" || update.Done != 0 || update.Total != 0 ||
		update.Stage != "" || update.Detail != ""
}

func invokeHandler(
	handler Handler,
	ctx context.Context,
	job Job,
	report ProgressReporter,
) (result map[string]any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("job handler panic: %v", recovered)
			result = nil
		}
	}()
	return handler(ctx, job, report)
}

func claimedJobOptions(job Job, options reducer.Options) reducer.Options {
	options.Actor = contracts.ActorJob
	if job.WorkerID != nil && job.StartedAt != nil {
		options.JobClaim = &reducer.JobClaim{
			JobID: job.ID, WorkerID: *job.WorkerID, StartedAt: *job.StartedAt,
		}
	}
	return options
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
			"worker_id":             value(job.WorkerID), "started_at": value(job.StartedAt),
			"progress": 1.0, "result": result, "error": failure,
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

func sameClaim(left, right Job) bool {
	return left.WorkerID != nil && right.WorkerID != nil &&
		left.StartedAt != nil && right.StartedAt != nil &&
		*left.WorkerID == *right.WorkerID && *left.StartedAt == *right.StartedAt
}
