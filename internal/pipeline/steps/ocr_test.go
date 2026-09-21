package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const ocrCleanJSON = `{"status":"success","summary":{"files_reviewed":3,"comments":0,"total_tokens":1200},"comments":[]}`

const ocrFindingsJSON = `{
	"status":"success",
	"summary":{"files_reviewed":3,"comments":3,"total_tokens":3000},
	"comments":[
		{"path":"src/a.go","content":"Concurrent map access without a lock","start_line":42,"end_line":47,"category":"bug","severity":"high"},
		{"path":"src/a.go","content":"Missing timeout on the HTTP client","start_line":10,"end_line":10,"category":"security","severity":"medium"},
		{"path":"src/b.go","content":"Consider naming this constant more clearly","start_line":5,"end_line":5,"category":"maintainability","severity":"low"}
	]
}`

// fakeOcr injects a canned ocr invocation result into the step's runner seam.
func fakeOcr(output string, exitCode int, record *[]string) func(*pipeline.StepContext, string) (string, int, error) {
	return func(_ *pipeline.StepContext, cmdStr string) (string, int, error) {
		if record != nil {
			*record = append(*record, cmdStr)
		}
		return output, exitCode, nil
	}
}

func ocrTestContext(t *testing.T, dir, baseSHA, headSHA string, ocrCfg config.OCR) *pipeline.StepContext {
	t.Helper()
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.OCR = ocrCfg
	return sctx
}

func TestOCRStep_CleanRunCompletesWithoutApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	var calls []string
	step := &OCRStep{runCommand: fakeOcr(ocrCleanJSON, 0, &calls)}
	sctx := ocrTestContext(t, dir, baseSHA, headSHA, config.OCR{Enabled: true, Effort: "medium", TimeoutMin: 15, MinSeverity: "low"})

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval on a clean OpenCodeReview run")
	}
	if outcome.AutoFixable {
		t.Error("expected no auto-fix on a clean run")
	}
	parsed, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Items) != 0 {
		t.Errorf("expected no findings, got %d", len(parsed.Items))
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 ocr invocation, got %d", len(calls))
	}
}

func TestOCRStep_CommentsMapToFindingsAndBlock(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	step := &OCRStep{runCommand: fakeOcr(ocrFindingsJSON, 0, nil)}
	sctx := ocrTestContext(t, dir, baseSHA, headSHA, config.OCR{Enabled: true, Effort: "medium", TimeoutMin: 15, MinSeverity: "low"})

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected blocking findings to require approval")
	}
	if !outcome.AutoFixable {
		t.Error("expected findings to be auto-fixable")
	}
	parsed, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Items) != 3 {
		t.Fatalf("expected 3 findings, got %d", len(parsed.Items))
	}
	byFile := map[string]Finding{}
	for _, item := range parsed.Items {
		byFile[item.File+"#"+item.Severity] = item
	}
	high := byFile["src/a.go#error"]
	if high.Description == "" || !strings.HasPrefix(high.Description, "[bug]") {
		t.Errorf("expected high finding with bug category prefix, got %q", high.Description)
	}
	if high.Line != 42 {
		t.Errorf("expected line 42, got %d", high.Line)
	}
	if high.Action != types.ActionAutoFix {
		t.Errorf("expected auto-fix action for error severity, got %q", high.Action)
	}
	medium := byFile["src/a.go#warning"]
	if medium.Action != types.ActionAutoFix {
		t.Errorf("expected auto-fix action for warning severity, got %q", medium.Action)
	}
	low := byFile["src/b.go#info"]
	if low.Action != types.ActionNoOp {
		t.Errorf("expected no-op action for info severity, got %q", low.Action)
	}
	if !strings.HasPrefix(parsed.Summary, "OpenCodeReview:") {
		t.Errorf("expected OpenCodeReview summary prefix, got %q", parsed.Summary)
	}
}

func TestOCRStep_InfoOnlyDoesNotBlock(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	infoOnly := `{"status":"success","comments":[{"path":"src/b.go","content":"Cosmetic note","start_line":5,"category":"style","severity":"low"}]}`
	step := &OCRStep{runCommand: fakeOcr(infoOnly, 0, nil)}
	sctx := ocrTestContext(t, dir, baseSHA, headSHA, config.OCR{Enabled: true, MinSeverity: "low"})

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected info-only findings not to block")
	}
	parsed, _ := types.ParseFindingsJSON(outcome.Findings)
	if len(parsed.Items) != 1 || parsed.Items[0].Severity != types.FindingSeverityInfo {
		t.Fatalf("expected a single info finding, got %+v", parsed.Items)
	}
}

func TestOCRStep_MinSeverityDropsLowComments(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	step := &OCRStep{runCommand: fakeOcr(ocrFindingsJSON, 0, nil)}
	sctx := ocrTestContext(t, dir, baseSHA, headSHA, config.OCR{Enabled: true, MinSeverity: "medium"})

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := types.ParseFindingsJSON(outcome.Findings)
	if len(parsed.Items) != 2 {
		t.Fatalf("expected 2 findings above medium threshold, got %d: %+v", len(parsed.Items), parsed.Items)
	}
}

func TestOCRStep_ExitFailureFailsClosed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	step := &OCRStep{runCommand: fakeOcr("fatal: cannot resolve LLM endpoint\n", 1, nil)}
	sctx := ocrTestContext(t, dir, baseSHA, headSHA, config.OCR{Enabled: true})

	_, err := step.Execute(sctx)
	if err == nil {
		t.Fatal("expected an ocr failure to fail the step closed")
	}
	if !strings.Contains(err.Error(), "exit code 1") {
		t.Errorf("expected the exit code in the error, got %v", err)
	}
}

func TestOCRStep_CommandNotFoundGivesTargetedError(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	step := &OCRStep{runCommand: fakeOcr("sh: ocr: command not found\n", 127, nil)}
	sctx := ocrTestContext(t, dir, baseSHA, headSHA, config.OCR{Enabled: true})

	_, err := step.Execute(sctx)
	if err == nil {
		t.Fatal("expected a missing ocr binary to fail the step")
	}
	if !strings.Contains(err.Error(), "install") || !strings.Contains(err.Error(), "@alibaba-group/open-code-review") {
		t.Errorf("expected a targeted install hint, got %v", err)
	}
}

func TestOCRStep_UnparseableOutputFailsClosed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	step := &OCRStep{runCommand: fakeOcr("not json at all\n", 0, nil)}
	sctx := ocrTestContext(t, dir, baseSHA, headSHA, config.OCR{Enabled: true})

	if _, err := step.Execute(sctx); err == nil {
		t.Fatal("expected undecodable output to fail the step closed")
	}
}

func TestOCRStep_UnexpectedStatusFailsClosed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	step := &OCRStep{runCommand: fakeOcr(`{"status":"mystery","comments":[]}`, 0, nil)}
	sctx := ocrTestContext(t, dir, baseSHA, headSHA, config.OCR{Enabled: true})

	if _, err := step.Execute(sctx); err == nil || !strings.Contains(err.Error(), "mystery") {
		t.Fatalf("expected an unknown status to fail closed, got %v", err)
	}
}

func TestOCRStep_SkippedStatusApproves(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	skipped := `{"status":"skipped","message":"No supported files changed.","comments":[]}`
	step := &OCRStep{runCommand: fakeOcr(skipped, 0, nil)}
	sctx := ocrTestContext(t, dir, baseSHA, headSHA, config.OCR{Enabled: true})

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || outcome.AutoFixable {
		t.Errorf("expected a skipped envelope to approve cleanly, got needsApproval=%v autoFixable=%v", outcome.NeedsApproval, outcome.AutoFixable)
	}
	parsed, _ := types.ParseFindingsJSON(outcome.Findings)
	if len(parsed.Items) != 0 {
		t.Errorf("expected no findings for a skipped envelope, got %d", len(parsed.Items))
	}
}

func TestOCRStep_CompletedWithWarningsStillPasses(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	withWarnings := `{"status":"completed_with_warnings","warnings":[{"file":"src/c.go","error":"sub-agent failed"}],"comments":[{"path":"src/a.go","content":"One real finding","start_line":1,"severity":"high"}]}`
	step := &OCRStep{runCommand: fakeOcr(withWarnings, 0, nil)}
	sctx := ocrTestContext(t, dir, baseSHA, headSHA, config.OCR{Enabled: true})

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected the reported high finding to block despite sub-agent warnings")
	}
	parsed, _ := types.ParseFindingsJSON(outcome.Findings)
	if len(parsed.Items) != 1 {
		t.Fatalf("expected the one real finding, got %d", len(parsed.Items))
	}
}

func TestOCRStep_ReviewCommandCarriesConfiguredScope(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	var calls []string
	step := &OCRStep{runCommand: fakeOcr(ocrCleanJSON, 0, &calls)}
	sctx := ocrTestContext(t, dir, baseSHA, headSHA, config.OCR{
		Enabled: true, Effort: "high", Provider: "anthropic", Model: "claude-opus-4-6", TimeoutMin: 30, MinSeverity: "low", Background: true,
	})
	sctx.UserIntent = "make the payment retry idempotent"

	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 ocr invocation, got %d", len(calls))
	}
	cmd := calls[0]
	for _, want := range []string{
		"ocr review",
		"--repo " + dir,
		"--from " + baseSHA,
		"--to " + headSHA,
		"--format json",
		"--audience agent",
		"--effort high",
		"--timeout 30",
		"--provider anthropic",
		"--model claude-opus-4-6",
		"--background " + sctx.UserIntent,
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("ocr invocation %q missing %q", cmd, want)
		}
	}
}

func TestOCRStep_FixTurnFixesThenRechecks(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	// The fixer has already repaired the worktree in a previous round; the one
	// ocr invocation this Execute makes is the re-check over the repaired head,
	// which reports clean so the step completes.
	callCount := 0
	step := &OCRStep{runCommand: func(_ *pipeline.StepContext, cmdStr string) (string, int, error) {
		callCount++
		return ocrCleanJSON, 0, nil
	}}

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "ocr-fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix concurrent map access"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.OCR = config.OCR{Enabled: true, MinSeverity: "low"}
	sctx.Fixing = true
	sctx.PreviousFindings = `{"items":[{"id":"ocr-abc","severity":"error","file":"src/a.go","line":42,"description":"Concurrent map access","action":"auto-fix"}],"summary":"OpenCodeReview: 1 comment"}`

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected exactly one fixer agent call, got %d", len(ag.calls))
	}
	if !strings.Contains(ag.calls[0].Prompt, "Concurrent map access") ||
		!strings.Contains(ag.calls[0].Prompt, "Previous OpenCodeReview findings") {
		t.Error("expected the fixer prompt to carry the previous OpenCodeReview findings")
	}
	if callCount != 1 {
		t.Fatalf("expected ocr to re-run once after the fix, got %d invocations", callCount)
	}
	if outcome.NeedsApproval {
		t.Error("expected a clean re-check to complete without approval")
	}
	if outcome.FixSummary == "" {
		t.Error("expected the fix summary to flow to the outcome")
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree after fix commit, got %q", status)
	}
}

func TestOCRStep_FixTurnWithoutFindingsFails(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	step := &OCRStep{runCommand: fakeOcr(ocrCleanJSON, 0, nil)}
	sctx := ocrTestContext(t, dir, baseSHA, headSHA, config.OCR{Enabled: true})
	sctx.Fixing = true

	if _, err := step.Execute(sctx); err == nil {
		t.Fatal("expected a fix turn without previous findings to fail closed")
	}
}

func TestWithConfiguredSteps_InsertsOCROnlyWhenEnabled(t *testing.T) {
	core := []pipeline.Step{&IntentStep{}, &RebaseStep{}, &ReviewStep{}, &TestStep{}}
	seq := WithConfiguredSteps(core, nil, false)
	if len(seq) != 4 {
		t.Fatalf("expected the bare core sequence when ocr is disabled, got %d steps", len(seq))
	}

	seq = WithConfiguredSteps(core, nil, true)
	if len(seq) != 5 {
		t.Fatalf("expected ocr inserted when enabled, got %d steps", len(seq))
	}
	if seq[2].Name() != types.StepReview || seq[3].Name() != types.StepOCR || seq[4].Name() != types.StepTest {
		t.Fatalf("expected ocr immediately after review, got %s, %s, %s", seq[2].Name(), seq[3].Name(), seq[4].Name())
	}

	// A repo gate anchored after review still lands between review and ocr.
	gate := config.Gate{Name: "license", After: types.StepReview, Command: "exit 0"}
	seq = WithConfiguredSteps(core, []config.Gate{gate}, true)
	if len(seq) != 6 {
		t.Fatalf("expected gate + ocr after review, got %d steps", len(seq))
	}
	if seq[2].Name() != types.StepReview || seq[3].Name() != types.StepOCR || seq[4].Name() != types.CustomGateStepName(types.StepReview, "license") || seq[5].Name() != types.StepTest {
		t.Fatalf("unexpected order: %s, %s, %s, %s", seq[2].Name(), seq[3].Name(), seq[4].Name(), seq[5].Name())
	}
}

// TestOCRStep_ExecutesRealOcrBinaryFromPATH exercises the production shell
// path (no injected runner seam): a real `ocr` executable on PATH that emits
// the machine-readable review envelope, invoked through the shared step shell
// runner. This is the one test that would catch a wiring bug between the step
// and runStepShellCommand (wrong env, wrong cwd, lost output).
func TestOCRStep_ExecutesRealOcrBinaryFromPATH(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	binDir := t.TempDir()
	script := filepath.Join(binDir, "ocr")
	content := `#!/bin/sh
cat <<'JSON'
{"status":"success","comments":[{"path":"src/a.go","content":"real binary finding","start_line":7,"severity":"high"}]}
JSON
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// runCommand is nil on purpose: the step must reach the real runner.
	sctx := ocrTestContext(t, dir, baseSHA, headSHA, config.OCR{Enabled: true, MinSeverity: "low"})
	outcome, err := (&OCRStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected the real-binary finding to block")
	}
	parsed, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Items) != 1 || parsed.Items[0].File != "src/a.go" || parsed.Items[0].Line != 7 {
		t.Fatalf("unexpected findings from the real binary: %+v", parsed.Items)
	}
}

const delegatePreviewJSON = `{
  "schema_version": "1",
  "mode": "range",
  "repository": "/repo",
  "from": "BASE",
  "to": "HEAD",
  "merge_base": "MB",
  "total_files": 3,
  "reviewable_count": 2,
  "excluded_count": 1,
  "total_insertions": 40,
  "total_deletions": 5,
  "reviewable_files": [
    {"path": "src/a.go", "status": "modified", "insertions": 30, "deletions": 2},
    {"path": "src/b.go", "status": "added", "insertions": 10, "deletions": 0}
  ],
  "excluded_files": [
    {"path": "README.md", "status": "modified", "insertions": 0, "deletions": 3, "exclude_reason": "docs-ignore"}
  ]
}`

const delegateEmptyPreviewJSON = `{
  "schema_version": "1",
  "mode": "range",
  "repository": "/repo",
  "from": "BASE",
  "to": "HEAD",
  "merge_base": "MB",
  "total_files": 3,
  "reviewable_count": 0,
  "excluded_count": 3,
  "total_insertions": 0,
  "total_deletions": 0,
  "reviewable_files": [],
  "excluded_files": [
    {"path": "README.md", "status": "modified", "exclude_reason": "docs-ignore"}
  ]
}`

const delegateRulesJSON = `{
  "schema_version": "1",
  "groups": [
    {"group_id": 1, "source": "go.rules", "pattern": "**/*.go", "files": ["src/a.go", "src/b.go"], "rule": "Check: nil checks, goroutine safety, error wrapping."}
  ]
}`

// delegateOcrSeam answers ocr invocations in delegate mode: preview and rule
// commands get their canned JSON, everything else fails.
func delegateOcrSeam(output string) func(*pipeline.StepContext, string) (string, int, error) {
	return func(_ *pipeline.StepContext, cmdStr string) (string, int, error) {
		switch {
		case strings.Contains(cmdStr, "delegate preview"):
			return output, 0, nil
		case strings.Contains(cmdStr, "delegate rule"):
			return delegateRulesJSON, 0, nil
		default:
			return "unexpected command: " + cmdStr, 1, nil
		}
	}
}

func delegateReviewJSON(covered bool) string {
	reviewed := "[\"src/a.go\",\"src/b.go\"]"
	if !covered {
		reviewed = "[\"src/a.go\"]"
	}
	return `{"findings":[{"severity":"warning","file":"src/a.go","line":12,"description":"error not wrapped","action":"auto-fix","review_scope":"source"}],"reviewed_paths":` + reviewed + `,"risk_level":"medium","risk_rationale":"one warning worth a follow-up","risk_scope":"source-or-external"}`
}

func TestOCRStep_DelegateModeReviewViaPipelineAgent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "opencode",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(delegateReviewJSON(true))}, nil
		},
	}
	step := &OCRStep{runCommand: delegateOcrSeam(delegatePreviewJSON)}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.OCR = config.OCR{Enabled: true, Delegate: true, MinSeverity: "low"}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected the delegated review finding to block")
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected exactly one pipeline-agent review call, got %d", len(ag.calls))
	}
	prompt := ag.calls[0].Prompt
	for _, want := range []string{
		"OpenCodeReview delegation scaffold",
		"src/a.go [modified] +30/-2",
		"src/b.go [added] +10/-0",
		"README.md (docs-ignore)",
		"Group 1",
		"reviewed_paths",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("delegate prompt missing %q", want)
		}
	}
	parsed, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Items) != 1 || parsed.Items[0].File != "src/a.go" || parsed.Items[0].Severity != "warning" {
		t.Fatalf("unexpected delegated findings: %+v", parsed.Items)
	}
}

func TestOCRStep_DelegateModeCleanPassRequiresCoverage(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	// Clean findings (no items) but reviewed_paths only covers one of the two
	// files: the gate must park, never read the omission as a clean pass.
	ag := &mockAgent{
		name: "opencode",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"reviewed_paths":["src/a.go"],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`)}, nil
		},
	}
	step := &OCRStep{runCommand: delegateOcrSeam(delegatePreviewJSON)}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.OCR = config.OCR{Enabled: true, Delegate: true}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected partial coverage to park the clean verdict")
	}
}

func TestOCRStep_DelegateModeNoReviewableFilesApproves(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "opencode"}
	step := &OCRStep{runCommand: delegateOcrSeam(delegateEmptyPreviewJSON)}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.OCR = config.OCR{Enabled: true, Delegate: true}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || outcome.AutoFixable {
		t.Errorf("expected an empty delegate preview to approve, got needsApproval=%v autoFixable=%v", outcome.NeedsApproval, outcome.AutoFixable)
	}
	if len(ag.calls) != 0 {
		t.Errorf("expected no agent call when nothing is reviewable, got %d", len(ag.calls))
	}
}

func TestOCRStep_DelegateModeFailuresFailClosed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	tests := []struct {
		name    string
		seam    func(*pipeline.StepContext, string) (string, int, error)
		wantSub string
	}{
		{
			name: "preview_failure",
			seam: func(_ *pipeline.StepContext, cmdStr string) (string, int, error) {
				return "fatal: bad flags\n", 1, nil
			},
			wantSub: "exit code 1",
		},
		{
			name: "preview_undecodable",
			seam: func(_ *pipeline.StepContext, cmdStr string) (string, int, error) {
				return "not json\n", 0, nil
			},
			wantSub: "decode ocr delegate preview",
		},
		{
			name: "rules_failure",
			seam: func(_ *pipeline.StepContext, cmdStr string) (string, int, error) {
				if strings.Contains(cmdStr, "delegate preview") {
					return delegatePreviewJSON, 0, nil
				}
				return "fatal: rules exploded\n", 1, nil
			},
			wantSub: "exit code 1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sctx := ocrTestContext(t, dir, baseSHA, headSHA, config.OCR{Enabled: true, Delegate: true})
			_, err := (&OCRStep{runCommand: tt.seam}).Execute(sctx)
			if err == nil {
				t.Fatal("expected the delegate failure to fail the step closed")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantSub)
			}
		})
	}
}

func TestOCRStep_DelegateModeRejectedOutputRetriesOnce(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	attempts := 0
	ag := &mockAgent{
		name: "opencode",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			attempts++
			if attempts == 1 {
				return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"bogus"}]}`)}, nil
			}
			return &agent.Result{Output: json.RawMessage(delegateReviewJSON(true))}, nil
		},
	}
	step := &OCRStep{runCommand: delegateOcrSeam(delegatePreviewJSON)}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.OCR = config.OCR{Enabled: true, Delegate: true}

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("expected one rejected attempt then a rerun, got %d attempts", attempts)
	}
	if !outcome.NeedsApproval {
		t.Error("expected the corrected delegated review to still park on its finding")
	}
}

func TestOCRStep_DelegateModeCommandCarriesScope(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	var calls []string
	seam := func(_ *pipeline.StepContext, cmdStr string) (string, int, error) {
		calls = append(calls, cmdStr)
		switch {
		case strings.Contains(cmdStr, "delegate preview"):
			return delegatePreviewJSON, 0, nil
		case strings.Contains(cmdStr, "delegate rule"):
			return delegateRulesJSON, 0, nil
		default:
			return "", 1, nil
		}
	}
	ag := &mockAgent{
		name: "opencode",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(delegateReviewJSON(true))}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.OCR = config.OCR{Enabled: true, Delegate: true, Background: true}
	sctx.Config.IgnorePatterns = []string{"vendor/**"}
	sctx.UserIntent = "add rate limiting"

	if _, err := (&OCRStep{runCommand: seam}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected preview + rule invocations, got %d: %v", len(calls), calls)
	}
	preview := calls[0]
	for _, want := range []string{"ocr delegate preview", "--from " + baseSHA, "--to " + headSHA, "--format json", "--exclude vendor/**", "--background add rate limiting"} {
		if !strings.Contains(preview, want) {
			t.Errorf("preview command %q missing %q", preview, want)
		}
	}
	if !strings.Contains(calls[1], "ocr delegate rule") || !strings.Contains(calls[1], "src/a.go") || !strings.Contains(calls[1], "src/b.go") {
		t.Errorf("rule command %q must carry the reviewable paths", calls[1])
	}
}

func TestOCRStep_DelegateModeFixTurnReReviewsViaAgent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "opencode",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			// The fixer writes a change so executeFixMode can commit, then the
			// delegated review reports clean.
			os.WriteFile(filepath.Join(dir, "delegated-fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"wrap the error"}`)}, nil
		},
	}
	// The agent mock cannot distinguish fixer turn from review turn by itself:
	// count calls - the first agent call is the fixer (JSONSchema summary), the
	// second is the delegated review.
	reviewResult := `{"findings":[],"reviewed_paths":["src/a.go","src/b.go"],"risk_level":"low","risk_rationale":"clean after fix","risk_scope":"source-or-external"}`
	callCount := 0
	ag.runFn = func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		callCount++
		if callCount == 1 {
			os.WriteFile(filepath.Join(dir, "delegated-fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"wrap the error"}`)}, nil
		}
		return &agent.Result{Output: json.RawMessage(reviewResult)}, nil
	}

	step := &OCRStep{runCommand: delegateOcrSeam(delegatePreviewJSON)}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.OCR = config.OCR{Enabled: true, Delegate: true}
	sctx.Fixing = true
	sctx.PreviousFindings = `{"items":[{"id":"ocr-abc","severity":"warning","file":"src/a.go","line":12,"description":"error not wrapped","action":"auto-fix"}],"summary":"OpenCodeReview: 1 comment"}`

	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if callCount != 2 {
		t.Fatalf("expected fixer + delegate review agent calls, got %d", callCount)
	}
	if outcome.NeedsApproval {
		t.Error("expected the clean delegated re-review to complete the fix round")
	}
}
