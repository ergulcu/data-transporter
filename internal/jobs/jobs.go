// Copyright 2025 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

// Package jobs runs migrations in the background, so that a long transfer does not hold an HTTP request open.
// Jobs are kept in memory; the most recent MaxFinished finished jobs stay visible.
package jobs

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ergulcu/data-transporter/internal/transfer"
)

// MaxFinished is how many finished jobs are kept.
const MaxFinished = 200

// Status of a job.
type Status string

const (
	Running   Status = "RUNNING"
	Succeeded Status = "SUCCEEDED"
	Failed    Status = "FAILED"
	Cancelled Status = "CANCELLED"
)

// View is what the API shows of a job.
type View struct {
	ID              string           `json:"id"`
	Status          Status           `json:"status"`
	SourceCode      string           `json:"sourceCode"`
	TargetCode      string           `json:"targetCode"`
	TransferredRows int64            `json:"transferredRows"`
	StartedAt       time.Time        `json:"startedAt"`
	FinishedAt      *time.Time       `json:"finishedAt"`
	Result          *transfer.Result `json:"result"`
}

// Migrator is what runs the migrations; *transfer.Transporter is one.
type Migrator interface {
	Migrate(ctx context.Context, req transfer.Request, progress func(rows int64)) (transfer.Result, error)
}

type job struct {
	view            View
	cancel          context.CancelFunc
	cancelRequested bool
}

// Jobs is safe for concurrent use.
type Jobs struct {
	migrator Migrator
	ctx      context.Context
	now      func() time.Time

	mu   sync.Mutex
	jobs map[string]*job
	wg   sync.WaitGroup
}

// New runs the jobs under ctx: cancelling it stops them all.
func New(ctx context.Context, m Migrator) *Jobs {
	return &Jobs{migrator: m, ctx: ctx, now: time.Now, jobs: map[string]*job{}}
}

// Start begins a migration and returns at once.
func (j *Jobs) Start(req transfer.Request) View {
	ctx, cancel := context.WithCancel(j.ctx)
	jb := &job{cancel: cancel, view: View{ID: uuid.NewString(), Status: Running, SourceCode: req.SourceCode,
		TargetCode: req.TargetCode, StartedAt: j.now().UTC()}}
	j.mu.Lock()
	j.jobs[jb.view.ID] = jb
	j.trim()
	view := jb.view
	j.mu.Unlock()

	j.wg.Go(func() {
		defer cancel()
		res, err := j.migrator.Migrate(ctx, req, func(rows int64) {
			j.mu.Lock()
			jb.view.TransferredRows = rows
			j.mu.Unlock()
		})
		if err != nil {
			res = transfer.Result{Success: false, Message: err.Error()}
		}
		j.mu.Lock()
		defer j.mu.Unlock()
		finished := j.now().UTC()
		jb.view.FinishedAt, jb.view.Result = &finished, &res
		jb.view.TransferredRows = res.TransferredRows
		switch {
		case res.Success:
			jb.view.Status = Succeeded
		case jb.cancelRequested || j.ctx.Err() != nil: // cancelled, or the service is stopping
			jb.view.Status = Cancelled
		default:
			jb.view.Status = Failed
		}
	})
	return view
}

// Find returns a job.
func (j *Jobs) Find(id string) (View, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	jb, ok := j.jobs[id]
	if !ok {
		return View{}, false
	}
	return jb.view, true
}

// List returns the jobs, the latest first.
func (j *Jobs) List() []View {
	j.mu.Lock()
	defer j.mu.Unlock()
	views := make([]View, 0, len(j.jobs))
	for _, jb := range j.jobs {
		views = append(views, jb.view)
	}
	slices.SortFunc(views, func(a, b View) int { return b.StartedAt.Compare(a.StartedAt) })
	return views
}

// Cancel stops a running job; it rolls back what it has not committed. It returns false for unknown ids.
func (j *Jobs) Cancel(id string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	jb, ok := j.jobs[id]
	if !ok {
		return false
	}
	if jb.view.Status == Running {
		jb.cancelRequested = true
		jb.cancel()
	}
	return true
}

// Wait waits for the running jobs, for example after cancelling the context given to New.
func (j *Jobs) Wait() { j.wg.Wait() }

// trim drops the oldest finished jobs beyond MaxFinished; j.mu is held.
func (j *Jobs) trim() {
	var finished []*job
	for _, jb := range j.jobs {
		if jb.view.FinishedAt != nil {
			finished = append(finished, jb)
		}
	}
	if len(finished) <= MaxFinished {
		return
	}
	slices.SortFunc(finished, func(a, b *job) int { return a.view.FinishedAt.Compare(*b.view.FinishedAt) })
	for _, jb := range finished[:len(finished)-MaxFinished] {
		delete(j.jobs, jb.view.ID)
	}
}
