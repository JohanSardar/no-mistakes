package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestConflictingActiveRunLaunchAttribution(t *testing.T) {
	t.Parallel()
	storedFixes := "run-earlier"
	storedWorker := `{"tool":"codex","model":"gpt-5"}`
	recorded := &ipc.RunInfo{ID: "run-1", FixesRunID: &storedFixes, WorkerProvenanceJSON: &storedWorker}
	bare := &ipc.RunInfo{ID: "run-1"}
	for _, tc := range []struct {
		name       string
		run        *ipc.RunInfo
		provenance string
		fixesRun   string
		wantErr    string
	}{
		{name: "omitted_flags_reattach", run: recorded},
		{name: "omitted_flags_reattach_bare_run", run: bare},
		{name: "matching_values_reattach", run: recorded, provenance: storedWorker, fixesRun: "run-earlier"},
		{name: "equivalent_provenance_reattaches", run: recorded, provenance: ` {"model":"gpt-5", "tool":"codex", "provider":" "} `},
		{name: "empty_provenance_object_is_no_flag", run: bare, provenance: `{}`},
		{name: "fixes_run_not_recorded", run: bare, fixesRun: "run-earlier", wantErr: "active run run-1 is already in progress without --fixes-run run-earlier"},
		{name: "fixes_run_differs", run: recorded, fixesRun: "run-other", wantErr: "active run run-1 is already linked to --fixes-run run-earlier, not run-other"},
		{name: "provenance_not_recorded", run: bare, provenance: storedWorker, wantErr: "active run run-1 is already in progress without --worker-provenance"},
		{name: "provenance_differs", run: recorded, provenance: `{"tool":"claude-code"}`, wantErr: "active run run-1 already records a different --worker-provenance"},
		{name: "malformed_provenance", run: recorded, provenance: `{"tool":`, wantErr: "parse worker provenance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := conflictingActiveRunLaunchAttribution(tc.run, tc.provenance, tc.fixesRun)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected reattachment, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// Reattaching to the branch's active run must not silently drop entry
// provenance the launch did not record: it is written once at launch, so a
// --fixes-run or --worker-provenance the run does not carry would never reach
// its attribution record. The refusal is structured, like --base-branch.
func TestAxiRunRefusesReattachmentThatWouldDropLaunchAttribution(t *testing.T) {
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{})
	fx.setGetActive(func(context.Context) (*ipc.RunInfo, error) { return fx.running(), nil })

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "fixes_run", args: []string{"--fixes-run", "run-earlier"}, want: "without --fixes-run run-earlier"},
		{name: "worker_provenance", args: []string{"--worker-provenance", `{"tool":"codex"}`}, want: "without --worker-provenance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := executeCmd(append([]string{"axi", "run", "--wait", "3s"}, tc.args...)...)
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != 2 {
				t.Fatalf("err = %v, want exit 2\n%s", err, out)
			}
			if !strings.Contains(out, "active run run-timeout is already in progress "+tc.want) {
				t.Fatalf("missing structured refusal:\n%s", out)
			}
			if !strings.Contains(out, "Omit --worker-provenance and --fixes-run to reattach") {
				t.Fatalf("missing reattach help:\n%s", out)
			}
		})
	}

	fixes := "run-earlier"
	worker := `{"tool":"codex"}`
	fx.setGetActive(func(context.Context) (*ipc.RunInfo, error) {
		run := fx.running()
		run.FixesRunID = &fixes
		run.WorkerProvenanceJSON = &worker
		return run, nil
	})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) { return fx.completed(), nil })
	out, err := executeCmd("axi", "run", "--wait", "3s", "--fixes-run", "run-earlier", "--worker-provenance", ` {"tool": "codex"} `)
	if err != nil {
		t.Fatalf("matching provenance should reattach: %v\n%s", err, out)
	}
	if !strings.Contains(out, "outcome: passed") {
		t.Fatalf("expected reattached completed run:\n%s", out)
	}
}
