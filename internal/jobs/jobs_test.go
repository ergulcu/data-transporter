// Copyright 2025 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

package jobs

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ergulcu/data-transporter/internal/transfer"
)

// fake writes a row every millisecond until it has written rows or is cancelled.
type fake struct {
	rows int64
	err  error
}

func (f fake) Migrate(ctx context.Context, _ transfer.Request, progress func(int64)) (transfer.Result, error) {
	if f.err != nil {
		return transfer.Result{}, f.err
	}
	for i := int64(1); i <= f.rows; i++ {
		select {
		case <-ctx.Done():
			return transfer.Result{Success: false, Message: "Migration was cancelled", TransferredRows: i - 1}, nil
		case <-time.After(time.Millisecond):
		}
		progress(i)
	}
	return transfer.Result{Success: true, Message: "Migration completed", TransferredRows: f.rows, CommittedRows: f.rows}, nil
}

func wait(t *testing.T, j *Jobs, id string, status Status) View {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if v, _ := j.Find(id); v.Status == status {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	v, _ := j.Find(id)
	t.Fatalf("job is %s, want %s", v.Status, status)
	return v
}

var req = transfer.Request{SourceCode: "a", TargetCode: "b"}

func TestAJobRunsToTheEnd(t *testing.T) {
	j := New(context.Background(), fake{rows: 20})
	v := j.Start(req)
	if v.Status != Running || v.ID == "" || v.SourceCode != "a" || v.FinishedAt != nil {
		t.Errorf("started = %+v", v)
	}
	done := wait(t, j, v.ID, Succeeded)
	if done.TransferredRows != 20 || done.FinishedAt == nil || done.Result == nil || !done.Result.Success {
		t.Errorf("done = %+v", done)
	}
}

func TestCancel(t *testing.T) {
	j := New(context.Background(), fake{rows: 1_000_000})
	v := j.Start(req)
	time.Sleep(20 * time.Millisecond)
	if !j.Cancel(v.ID) {
		t.Fatal("cancel returned false")
	}
	done := wait(t, j, v.ID, Cancelled)
	if done.Result.Message != "Migration was cancelled" || done.TransferredRows == 0 {
		t.Errorf("done = %+v", done)
	}
	if j.Cancel("unknown") {
		t.Error("unknown ids must return false")
	}
	if !j.Cancel(v.ID) {
		t.Error("cancelling a finished job is not an error")
	}
}

func TestFailure(t *testing.T) {
	j := New(context.Background(), fake{err: errors.New("No database configuration with code 'b'")})
	v := wait(t, j, j.Start(req).ID, Failed)
	if v.Result.Message != "No database configuration with code 'b'" {
		t.Errorf("result = %+v", v.Result)
	}
}

func TestStoppingTheContextStopsTheJobs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	j := New(ctx, fake{rows: 1_000_000})
	id := j.Start(req).ID
	cancel()
	j.Wait()
	if v, _ := j.Find(id); v.Status != Cancelled {
		t.Errorf("status = %s", v.Status)
	}
}

func TestListAndTrim(t *testing.T) {
	j := New(context.Background(), fake{})
	now := time.Date(2026, 3, 10, 10, 0, 0, 0, time.UTC)
	j.now = func() time.Time { now = now.Add(time.Second); return now }
	var ids []string
	for range MaxFinished + 5 {
		id := j.Start(req).ID
		wait(t, j, id, Succeeded)
		ids = append(ids, id)
	}
	j.Start(req) // trims before it is added
	list := j.List()
	if len(list) != MaxFinished+1 {
		t.Errorf("%d jobs are kept, want %d", len(list), MaxFinished+1)
	}
	if _, ok := j.Find(ids[0]); ok {
		t.Error("the oldest jobs must be dropped")
	}
	for i := 1; i < len(list); i++ {
		if list[i].StartedAt.After(list[i-1].StartedAt) {
			t.Fatal(fmt.Sprint("the list must start with the latest job: ", list[i-1].StartedAt, list[i].StartedAt))
		}
	}
}
