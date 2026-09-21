//go:build !e2e

package daemon

import (
	"context"
	"os"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ocrPinFixture builds a daemon root whose repository's trusted default branch
// carries defaultBranchYAML, plus a run parked at an approval gate whose step
// rows are exactly the sequence ocrPinned produces (the core pipeline plus the
// OpenCodeReview gate after review). The run's ocr_enabled pin is recorded to
// true.
func ocrPinFixture(t *testing.T, defaultBranchYAML string) (*RunManager, *db.Run, []types.StepName) {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mockClaude := writeMockClaude(t, t.TempDir())
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  claude: "+mockClaude+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	repo, _ := setupTestGitRepo(t, p, d, "repo1")
	headSHA := commitDefaultBranchConfig(t, repo.WorkingPath, defaultBranchYAML)

	run, err := d.InsertRun(repo.ID, "feature", headSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, p.RepoDir(repo.ID), "worktree", "add", "--detach", p.WorktreeDir(repo.ID, run.ID), headSHA)

	sequence := steps.WithConfiguredSteps(steps.AllSteps(), nil, true)
	recorded := parkRunAtReviewGate(t, d, run.ID, sequence)
	if !ocrAfterReview(recorded) {
		t.Fatalf("fixture sequence missing ocr after review: %v", recorded)
	}
	if err := d.SetRunOCREnabled(run.ID, true); err != nil {
		t.Fatal(err)
	}
	stored, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return NewRunManager(d, p, nil), stored, recorded
}

// TestPrepareRecoveredRun_ResumesWithTheOCRStepTheRunPinned is the
// crash-recovery half of pinning the OpenCodeReview gate's presence: the
// trusted default branch drops ocr.enabled while a run is parked, so
// re-resolving the flag at recovery time would rebuild a shorter step sequence
// than the one the run recorded (the ocr gateway it actually executed), and
// drop an otherwise healthy parked run as a crash.
func TestPrepareRecoveredRun_ResumesWithTheOCRStepTheRunPinned(t *testing.T) {
	ocrOnYAML := `auto_fix:
  lint: 0
  test: 0
  review: 0
ocr:
  enabled: true
`
	m, run, recorded := ocrPinFixture(t, ocrOnYAML)

	repo, err := m.db.GetRepo(run.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	// The maintainer disables the gate on the default branch after the run
	// parked. The run's own head, and its recorded steps, are untouched.
	commitDefaultBranchConfig(t, repo.WorkingPath, `auto_fix:
  lint: 0
  test: 0
  review: 0
ocr:
  enabled: false
`)

	plan, err := m.prepareRecoveredRun(context.Background(), run)
	if err != nil {
		t.Fatalf("parked run must still recover after the default branch disabled ocr: %v", err)
	}
	if got := planStepNames(plan); !stepNamesEqual(got, recorded) {
		t.Errorf("recovered step sequence = %v, want the sequence the run recorded %v", got, recorded)
	}
	foundOCR := false
	for _, name := range planStepNames(plan) {
		if name == types.StepOCR {
			foundOCR = true
		}
	}
	if !foundOCR {
		t.Errorf("recovered sequence must keep the pinned ocr step: %v", planStepNames(plan))
	}
}

// TestPrepareRecoveredRun_UnpinnedRunRecoversWithoutOCR covers the run written
// before the ocr_enabled pin existed: an absent pin means the gate was off,
// which is the only sequence such a run can have had.
func TestPrepareRecoveredRun_UnpinnedRunRecoversWithoutOCR(t *testing.T) {
	m, run, recorded := gatePinFixture(t, oneGateYAML, nil)
	if enabled, err := m.db.GetRunOCREnabled(run.ID); err != nil || enabled {
		t.Fatalf("fixture pinned ocr_enabled=%v (err %v), want the pre-upgrade absent pin (false)", enabled, err)
	}

	plan, err := m.prepareRecoveredRun(context.Background(), run)
	if err != nil {
		t.Fatalf("unpinned parked run must recover: %v", err)
	}
	if got := planStepNames(plan); !stepNamesEqual(got, recorded) {
		t.Errorf("recovered step sequence = %v, want the core sequence the run recorded %v", got, recorded)
	}
	for _, name := range planStepNames(plan) {
		if name == types.StepOCR {
			t.Errorf("unpinned run recovered with the ocr step from the live default branch")
		}
	}
}

// ocrAfterReview reports whether the OpenCodeReview gate comes immediately
// after the review step in a recorded sequence.
func ocrAfterReview(names []types.StepName) bool {
	for i, name := range names {
		if name == types.StepReview {
			return i+1 < len(names) && names[i+1] == types.StepOCR
		}
	}
	return false
}
