package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunObjectRendersAttributionRecord(t *testing.T) {
	snapshot := `{"schema_version":1,"status":"partial","phase":"snapshot","run_id":"run-1","repo_id":"repo-1","worker":{"tool":"unknown","model":"unknown","provider":"unknown"},"bugs":[],"non_bugs":[],"counts":{"original_worker":0,"pipeline":0,"pre_existing":0,"unknown":1,"non_bugs":0,"fixed_before_shipping":0,"escaped":0},"evidence_gaps":["submitted head SHA is missing; git location is unknown"]}`
	final := `{"schema_version":1,"status":"complete","phase":"final","run_id":"run-1","repo_id":"repo-1","submitted_head_sha":"abcdef1234567890","worker":{"tool":"grok","model":"grok-4.6","provider":"xai"},"bug_fix":{"originating_run_id":"run-0","confirmed":false,"confidence":"unknown","evidence":["typed fixes_run_id signal","run failed; repair did not ship; repair is unconfirmed"]},"bugs":[{"id":"review-1","fingerprint":"fp","file":"svc.go","line":10,"description":"nil deref","source_step":"review","attribution":"original_worker","outcome":"fixed_before_shipping","confidence":"high"}],"non_bugs":[],"counts":{"original_worker":1,"pipeline":0,"pre_existing":0,"unknown":0,"non_bugs":2,"fixed_before_shipping":1,"escaped":0},"limits":["A changed line is not a bug; only confirmed findings and fix-round evidence are attributed."]}`

	rv := runViewFromIPC(&ipc.RunInfo{
		ID: "run-1", Branch: "feature/x", Status: types.RunFailed, HeadSHA: "abcdef1234567890",
		AttributionSnapshotJSON: &snapshot, AttributionJSON: &final,
	})
	out := axiDoc(runObjectField(rv))
	for _, want := range []string{
		"  attribution:\n",
		"    status: complete\n",
		"    phase: final\n",
		"    original_worker: 1\n",
		"    pipeline: 0\n",
		"    pre_existing: 0\n",
		"    unknown: 0\n",
		"    non_bugs: 2\n",
		"    worker_tool: grok\n",
		"    worker_model: grok-4.6\n",
		"    fixes_run: run-0\n",
		"    bug_fix_confirmed: false\n",
		"    submitted_head: abcdef1234567890\n",
		"A changed line is not a bug",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("run object missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "phase: snapshot") {
		t.Fatalf("final record must win over the snapshot in:\n%s", out)
	}

	// Before finalization only the snapshot exists, and it is what renders.
	rv = runViewFromIPC(&ipc.RunInfo{ID: "run-1", Branch: "feature/x", Status: types.RunRunning, HeadSHA: "abcdef1234567890", AttributionSnapshotJSON: &snapshot})
	out = axiDoc(runObjectField(rv))
	for _, want := range []string{"    status: partial\n", "    phase: snapshot\n", "    unknown: 1\n", "git location is unknown"} {
		if !strings.Contains(out, want) {
			t.Errorf("snapshot render missing %q in:\n%s", want, out)
		}
	}

	// A legacy run without any record renders no attribution block at all.
	rv = runViewFromIPC(&ipc.RunInfo{ID: "run-1", Branch: "feature/x", Status: types.RunCompleted, HeadSHA: "abcdef1234567890"})
	if out := axiDoc(runObjectField(rv)); strings.Contains(out, "attribution") {
		t.Fatalf("legacy run rendered an attribution block:\n%s", out)
	}

	// An unparseable record is surfaced as unavailable, never dropped as clean.
	broken := "{not json"
	rv = runViewFromIPC(&ipc.RunInfo{ID: "run-1", Branch: "feature/x", Status: types.RunCompleted, HeadSHA: "abcdef1234567890", AttributionJSON: &broken})
	out = axiDoc(runObjectField(rv))
	if !strings.Contains(out, "    status: unavailable\n") || !strings.Contains(out, "not a clean score") {
		t.Fatalf("broken record not surfaced in:\n%s", out)
	}
}
