package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A --fixes-run that names no run in this repository is refused as a plain
// request error before the launch mutates anything: the branch's healthy
// active run keeps running and no run row is created.
func TestPushReceivedRefusesBadFixesRunBeforeSupersedingOrCreatingARun(t *testing.T) {
	started := make(chan struct{})
	step := &mockSlowStep{name: types.StepReview, started: started}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{step} })
	repo, head := setupTestGitRepo(t, p, d, "fixes-run-refusal-repo")
	other, err := d.InsertRepoWithID("other-repo", t.TempDir(), "https://github.com/test/other", "main")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := d.InsertRun(other.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var first ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID), Ref: "refs/heads/main", New: head, Old: head, Intent: "keep the active run alive",
	}, &first); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("original run did not start")
	}

	for _, tc := range []struct{ name, fixesRun string }{
		{"unknown_run", "no-such-run"},
		{"run_in_another_repository", foreign.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var result ipc.PushReceivedResult
			err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
				Gate: p.RepoDir(repo.ID), Ref: "refs/heads/main", New: head, Old: head,
				Intent: "repair", LaunchNonce: "nonce-" + tc.name, ValidationGeneration: "gen-1", FixesRunID: tc.fixesRun,
			}, &result)
			if err == nil || !strings.Contains(err.Error(), "not a run in this repository") {
				t.Fatalf("expected fixes-run refusal, got result=%+v err=%v", result, err)
			}
			active, err := d.GetActiveRun(repo.ID, "main")
			if err != nil || active == nil || active.ID != first.RunID || active.Status != types.RunRunning {
				t.Fatalf("original run was superseded: active=%+v err=%v", active, err)
			}
			runs, err := d.GetRunsByRepo(repo.ID)
			if err != nil || len(runs) != 1 {
				t.Fatalf("refusal created a run: runs=%d err=%v", len(runs), err)
			}
			if bound, err := d.GetRunByLaunchNonce(repo.ID, "main", "nonce-"+tc.name); err != nil || bound != nil {
				t.Fatalf("refusal bound the launch nonce: run=%+v err=%v", bound, err)
			}
		})
	}
}

// Entry provenance supplied on a push is recorded on the run. A rerun that
// supplies none inherits only the typed bug-fix link: its submitted head is
// the gate head with the earlier run's pipeline commits in it, so the worker
// identity stays unknown unless the caller supplies it again.
func TestLaunchAttributionIsRecordedAndInheritedByRerun(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&mockPassStep{name: types.StepReview}} })
	repo, head := setupTestGitRepo(t, p, d, "launch-attribution-repo")
	original, err := d.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatusWithVerifiedHead(original.ID, types.RunCompleted, head); err != nil {
		t.Fatal(err)
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var pushed ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir(repo.ID), Ref: "refs/heads/main", New: head, Old: head, Intent: "fix the earlier bug",
		WorkerProvenance: json.RawMessage(`{"tool":"grok","model":"grok-4.6","provider":"xai","task_id":"task-7"}`),
		FixesRunID:       original.ID,
	}, &pushed); err != nil {
		t.Fatal(err)
	}
	fixRun := waitForRunTerminalState(t, d, pushed.RunID)
	if fixRun.FixesRunID == nil || *fixRun.FixesRunID != original.ID {
		t.Fatalf("fixes_run_id = %v, want %s", fixRun.FixesRunID, original.ID)
	}
	worker, err := types.ParseWorkerProvenance(derefString(fixRun.WorkerProvenanceJSON))
	if err != nil || worker == nil || worker.Tool != "grok" || worker.Model != "grok-4.6" || worker.TaskID != "task-7" {
		t.Fatalf("worker provenance = %+v (%v) from %v", worker, err, fixRun.WorkerProvenanceJSON)
	}
	untouched, err := d.GetRun(original.ID)
	if err != nil || untouched.FixesRunID != nil || untouched.WorkerProvenanceJSON != nil {
		t.Fatalf("originating run was rewritten: %+v err=%v", untouched, err)
	}

	var rerun ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "main"}, &rerun); err != nil {
		t.Fatal(err)
	}
	inherited := waitForRunTerminalState(t, d, rerun.RunID)
	if inherited.ID == fixRun.ID {
		t.Fatal("rerun reused the previous run")
	}
	if inherited.FixesRunID == nil || *inherited.FixesRunID != original.ID {
		t.Fatalf("rerun lost fixes_run_id: %v", inherited.FixesRunID)
	}
	if inherited.WorkerProvenanceJSON != nil {
		t.Fatalf("rerun re-asserted the earlier worker identity over the gate head: %v", *inherited.WorkerProvenanceJSON)
	}
	if fixRun.Rerun || !inherited.Rerun {
		t.Fatalf("rerun marker: pushed run %v, rerun %v", fixRun.Rerun, inherited.Rerun)
	}

	// An explicit value on the rerun is recorded as supplied.
	var override ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{
		RepoID: repo.ID, Branch: "main", WorkerProvenance: json.RawMessage(`{"tool":"codex"}`),
	}, &override); err != nil {
		t.Fatal(err)
	}
	overridden := waitForRunTerminalState(t, d, override.RunID)
	if worker, err := types.ParseWorkerProvenance(derefString(overridden.WorkerProvenanceJSON)); err != nil || worker == nil || worker.Tool != "codex" {
		t.Fatalf("explicit rerun provenance = %+v (%v)", worker, err)
	}
	if overridden.FixesRunID == nil || *overridden.FixesRunID != original.ID {
		t.Fatalf("explicit worker override dropped the inherited fixes_run_id: %v", overridden.FixesRunID)
	}
	if !overridden.Rerun {
		t.Fatal("an explicit worker override cleared the rerun marker")
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
