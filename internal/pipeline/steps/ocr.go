package steps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// OCRStep runs the OpenCodeReview CLI (github.com/alibaba/open-code-review,
// the `ocr` binary) as an extra review gate immediately after the review step.
//
// It reviews the same branch diff the review step examined, but through OCR's
// own deterministic filter pipeline and LLM sub-agents, and surfaces OCR's
// line-level comments as pipeline findings. The gate passes when a fresh `ocr
// review` over the current head reports no error- or warning-severity
// comments; anything else parks for the operator or, within the auto_fix.ocr
// budget, is handed to the pipeline's fixer agent - the executor re-runs this
// step with Fixing set, the fixer repairs the worktree, and the step re-runs
// OCR on the repaired head, exactly like the review step's fix rounds.
//
// Two execution modes exist. The default mode runs `ocr review --format json`
// and needs an LLM endpoint configured on the OCR side (ocr config / OCR_LLM_*).
// Delegation mode (ocr.delegate) instead runs `ocr delegate preview` and
// `ocr delegate rule` for the deterministic engineering - mode/ref metadata,
// the reviewable file list, and the resolved rule groups - and then hands the
// scaffold to the pipeline's OWN review agent (the configured `agent`, e.g.
// opencode) with the review step's output contract. That mode needs no OCR-side
// LLM configuration: the agent's existing subscription powers the review.
//
// Findings map OCR severities onto the pipeline vocabulary: critical/high
// become severity-error (blocking), medium becomes warning (blocking), and low
// becomes info (non-blocking, listed on the PR only). Error and warning
// findings carry action auto-fix so the fix loop can act on them; info
// findings are no-op. The trusted ocr.min_severity threshold can drop
// low-grade comments entirely. In delegation mode the agent itself assigns
// severities and actions under the review step's contract, and the clean
// verdict additionally requires every OCR-selected file to appear in
// reviewed_paths.
//
// The step is opt-in via the trusted ocr.enabled config and is present in a
// run's step sequence only then, so `ocr` is a real step name but never a
// custom-gate anchor and never eligible for pushed-branch skip options beyond
// the per-run --skip control every core step has.
type OCRStep struct {
	// runCommand is the ocr execution seam. Nil uses the shared step shell
	// runner; tests inject a fake that returns canned output per call.
	runCommand func(sctx *pipeline.StepContext, cmdStr string) (string, int, error)
}

func (s *OCRStep) Name() types.StepName { return types.StepOCR }

func (s *OCRStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		return nil, err
	}
	baseSHA := resolveBranchBaseSHA(sctx.Ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)

	var fixSummary string
	if sctx.Fixing && !sctx.SkipFixExecution {
		summary, err := s.runFixTurn(sctx, baseSHA)
		if err != nil {
			return nil, err
		}
		fixSummary = summary
	}
	return s.runReview(sctx, baseSHA, fixSummary)
}

// runFixTurn repairs the worktree when the gate's park (or the auto-fix loop)
// selected OCR findings for a fix, and returns the agent's commit summary. The
// caller then re-runs OCR over the repaired worktree, so a re-parked verdict
// describes the repaired head rather than the unchanged one that produced the
// previous comments. This is the same fix protocol Review and Lint use.
func (s *OCRStep) runFixTurn(sctx *pipeline.StepContext, baseSHA string) (string, error) {
	historySection := executionContextPromptSection(sctx.WorkDir) + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)
	prompt := fmt.Sprintf(
		`Investigate previous OpenCodeReview findings and address legitimate ones.

Examine the relevant code yourself and apply fixes directly.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- default branch: %s

This gate passes when a fresh OpenCodeReview run over the changed area reports no error- or warning-severity findings. The pipeline's review and test steps cover the other gates; you only need to make the OpenCodeReview findings go away without weakening the code.

Rules:
- Always start with double checking whether the findings are legitimate.
- Before changing code, identify whether each finding is a local defect or a symptom of a deeper design, abstraction, validation, ownership, or test-coverage flaw. Prefer the smallest correct root-cause fix within the changed area over patching only the reported line.
- Fix the invariant at every place in the changed area where it must hold: every sibling call path, command, action, and state transition; every consumer of the same input, field, or record. A fix that closes only the reported site and leaves a sibling site reachable is incomplete; the next OpenCodeReview run will report the sibling.
- Do not grow the fix into machinery: closing sibling sites with the same small edit, or moving a check to one shared boundary, is the fix; adding handling, state, fallbacks, retries, or a subsystem to manage symptoms is not.
- Do not resolve a finding by removing or reverting intentional code the change's stated intent requires. If the original change introduced something the intent requires, fix it forward (e.g. add validation, handle edge cases, tighten logic) rather than deleting it. When in doubt about whether the intent requires the code, leave it and report the finding as unresolved.
- Do not add code comments explaining your fixes.
- Apply all the fixes you intend to make first; do not run any verification in between individual fixes.
- After applying the fixes, re-trace for each finding the concrete failing sequence it describes through the code as it now is, and trace the ordinary successful path through every function you changed. Remove any alias, branch, parameter, or helper your fix made unreachable.
- After all fixes are applied, run one focused verification limited to the changed area (the specific package, file, or test you touched) at the end of the fix round to confirm the fixes hold.
- Do NOT run the complete repository test suite or lint suite during this fix round. The pipeline has dedicated test and lint steps after review that are the authoritative test and lint gates.
- Return JSON with a single "summary" field when you are done.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.%s

Previous OpenCodeReview findings to address:
%s`,
		sctx.Run.Branch,
		baseSHA,
		sctx.Run.HeadSHA,
		sctx.Repo.DefaultBranch,
		historySection,
		sanitizedPreviousFindingsForPrompt(sctx.PreviousFindings),
	)
	return executeFixMode(sctx, s.Name(), fixExecutionOptions{
		RequirePreviousFindings: true,
		MissingFindingsError:    "ocr fix requires previous OpenCodeReview findings",
		LogMessage:              "asking agent to fix identified issues...",
		Prompt:                  prompt,
		ErrorPrefix:             "agent fix",
		FallbackSummary:         "address OpenCodeReview findings",
		Purpose:                 "ocr-fix",
	})
}

// runReview invokes `ocr review` over the run's diff, converts its comments
// into findings, and returns the step outcome. Every invocation is a fresh
// process: the previous fix round's changes were already committed by the
// fixer, so the run below always reviews the current worktree head. In
// delegation mode (ocr.delegate), the LLM work moves to the pipeline's own
// review agent and no OCR-side LLM configuration is needed.
func (s *OCRStep) runReview(sctx *pipeline.StepContext, baseSHA, fixSummary string) (*pipeline.StepOutcome, error) {
	headSHA, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("resolve head before %s step: %w", s.Name(), err)
	}
	if sctx.Config.OCR.Delegate {
		return s.runDelegateReview(sctx, baseSHA, headSHA, fixSummary)
	}

	cmdStr := s.reviewCommand(sctx, baseSHA, headSHA)
	sctx.Log("running OpenCodeReview on the changed diff...")
	output, exitCode, err := s.executeOcr(sctx, cmdStr)
	if err != nil {
		return nil, err
	}
	if exitCode != 0 {
		return nil, s.commandFailure(sctx, cmdStr, output, exitCode)
	}

	parsed, err := parseOcrReviewOutput(output)
	if err != nil {
		return nil, err
	}

	// OCR's "skipped" envelope means no eligible files changed: the pipeline's
	// review step treats that same case as an honest empty pass, and so does
	// this gate. An unreadable or failing run above already failed closed.
	if parsed.Status == "skipped" {
		message := strings.TrimSpace(parsed.Message)
		if message == "" {
			message = "no files eligible for OpenCodeReview"
		}
		sctx.Log(message)
		findings := Findings{Summary: "OpenCodeReview: " + message}
		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{
			Findings:   string(findingsJSON),
			FixSummary: fixSummary,
		}, nil
	}
	switch parsed.Status {
	case "success", "completed_with_warnings", "completed_with_errors":
	default:
		return nil, fmt.Errorf("ocr review returned unexpected status %q", parsed.Status)
	}
	for _, warning := range parsed.Warnings {
		sctx.Log(fmt.Sprintf("ocr sub-agent warning: file %s: %s", warning.File, warning.Error))
	}

	minSeverity := ocrSeverityRank(minOcrSeverity(sctx))
	items := make([]Finding, 0, len(parsed.Comments))
	seen := make(map[string]bool, len(parsed.Comments))
	for _, comment := range parsed.Comments {
		severity := normalizeOcrSeverity(comment.Severity)
		if ocrSeverityRank(severity) < minSeverity {
			continue
		}
		item := Finding{
			ID:          ocrFindingID(comment),
			File:        strings.TrimSpace(comment.Path),
			Description: strings.TrimSpace(comment.Content),
			Source:      "opencode-review",
		}
		if item.File == "" || item.Description == "" {
			continue
		}
		switch severity {
		case "critical", "high":
			item.Severity = types.FindingSeverityError
			item.Action = types.ActionAutoFix
		case "medium":
			item.Severity = types.FindingSeverityWarning
			item.Action = types.ActionAutoFix
		default:
			item.Severity = types.FindingSeverityInfo
			item.Action = types.ActionNoOp
		}
		if comment.StartLine > 0 {
			item.Line = comment.StartLine
		}
		if category := strings.TrimSpace(comment.Category); category != "" {
			item.Description = fmt.Sprintf("[%s] %s", category, item.Description)
		}
		key := item.File + "#" + item.ID
		if seen[key] {
			continue
		}
		seen[key] = true
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].File != items[j].File {
			return items[i].File < items[j].File
		}
		return items[i].Line < items[j].Line
	})

	summary := fmt.Sprintf("OpenCodeReview: %d comment(s) across the changed diff", len(items))
	findings := Findings{
		Items:   items,
		Summary: summary,
	}
	findingsJSON, _ := json.Marshal(findings)

	// NeedsApproval mirrors the review step: error- and warning-severity
	// findings block; info findings are listed on the PR but never park.
	needsApproval := hasBlockingFindings(items)
	return &pipeline.StepOutcome{
		NeedsApproval: needsApproval,
		// Auto-fixable whenever anything was reported, exactly like the review
		// step. The executor only ever auto-fixes the auto-fix-action subset
		// (bounded by auto_fix.ocr), and an unfixable finding parks after the
		// budget or when the operator answers the gate - never silently.
		AutoFixable: len(items) > 0,
		Findings:    string(findingsJSON),
		ExitCode:    0,
		FixSummary:  fixSummary,
	}, nil
}

// reviewCommand assembles the ocr invocation. The diff is always expressed as
// a from/to range over resolved SHAs so a fix round reviews exactly the
// current worktree head without depending on branch refs.
func (s *OCRStep) reviewCommand(sctx *pipeline.StepContext, baseSHA, headSHA string) string {
	cfg := sctx.Config.OCR
	args := []string{
		"review",
		"--repo", sctx.WorkDir,
		"--from", baseSHA,
		"--to", headSHA,
		"--format", "json",
		"--audience", "agent",
		"--effort", cfg.Effort,
		"--timeout", fmt.Sprintf("%d", cfg.TimeoutMin),
	}
	if provider := strings.TrimSpace(cfg.Provider); provider != "" {
		args = append(args, "--provider", provider)
	}
	if model := strings.TrimSpace(cfg.Model); model != "" {
		args = append(args, "--model", model)
	}
	if cfg.Background {
		if intent := strings.TrimSpace(sctx.UserIntent); intent != "" {
			args = append(args, "--background", intent)
		}
	}
	return "ocr " + strings.Join(args, " ")
}

// executeOcr runs the ocr command through the shared shell runner, or the
// test-injected seam.
func (s *OCRStep) executeOcr(sctx *pipeline.StepContext, cmdStr string) (string, int, error) {
	if s != nil && s.runCommand != nil {
		return s.runCommand(sctx, cmdStr)
	}
	return runStepShellCommand(sctx, cmdStr)
}

// commandFailure renders an ocr failure closed: a nonzero exit means the
// review did not complete (bad flags, unresolvable LLM endpoint, all
// sub-agents failed), which must never read as an approving empty pass. A
// missing binary gets a targeted message because it is the most common
// misconfiguration and its cause is invisible in the shell output otherwise.
func (s *OCRStep) commandFailure(sctx *pipeline.StepContext, cmdStr, output string, exitCode int) error {
	if strings.Contains(output, "command not found") || strings.Contains(output, "not recognized") {
		return fmt.Errorf("ocr: OpenCodeReview CLI not found on PATH - install it (npm install -g @alibaba-group/open-code-review) or disable ocr.enabled")
	}
	detail := strings.TrimSpace(output)
	if len(detail) > 2000 {
		detail = detail[:2000] + "..."
	}
	if detail == "" {
		return fmt.Errorf("open code review step failed with exit code %d", exitCode)
	}
	return fmt.Errorf("open code review step failed with exit code %d: %s", exitCode, detail)
}

type ocrReviewOutput struct {
	Status   string       `json:"status"`
	Message  string       `json:"message"`
	Comments []ocrComment `json:"comments"`
	Warnings []ocrWarning `json:"warnings"`
}

type ocrComment struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Category  string `json:"category"`
	Severity  string `json:"severity"`
}

type ocrWarning struct {
	File  string `json:"file"`
	Error string `json:"error"`
}

// parseOcrReviewOutput decodes ocr's machine-readable review envelope. The
// envelope is a single JSON document on stdout with --format json, and a
// review that produced no document cannot certify the head: issues #703-style
// fail-closed, so an undecodable or empty payload is a step error, never an
// approving empty pass.
func parseOcrReviewOutput(output string) (*ocrReviewOutput, error) {
	parsed := &ocrReviewOutput{}
	if err := json.Unmarshal([]byte(output), parsed); err != nil {
		return nil, fmt.Errorf("decode ocr review output: %w", err)
	}
	if strings.TrimSpace(parsed.Status) == "" {
		return nil, fmt.Errorf("ocr review output missing status field")
	}
	return parsed, nil
}

// ocrSeverityRank ranks ocr severities so min_severity filtering and the
// severity mapping share one ordering. Unknown and empty severities rank as 0.
func ocrSeverityRank(severity string) int {
	switch severity {
	case "critical":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

// normalizeOcrSeverity resolves an ocr comment's severity field. An absent
// severity is treated as medium (a comment that mattered enough to be emitted
// is at least a warning); an unrecognized value is treated as low so it stays
// visible without ever blocking.
func normalizeOcrSeverity(severity string) string {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical", "high", "medium", "low":
		return strings.ToLower(strings.TrimSpace(severity))
	case "":
		return "medium"
	default:
		return "low"
	}
}

func minOcrSeverity(sctx *pipeline.StepContext) string {
	if sctx != nil && sctx.Config != nil {
		return sctx.Config.OCR.MinSeverity
	}
	return ""
}

// ocrFindingID derives a stable content identity for an OCR comment so finding
// selections and carry-forward can reference it across fix rounds even though
// OCR itself emits no per-comment ID.
func ocrFindingID(comment ocrComment) string {
	sum := sha256.Sum256([]byte(comment.Path + "\x00" + comment.Content + "\x00" + fmt.Sprintf("%d", comment.StartLine)))
	return "ocr-" + hex.EncodeToString(sum[:8])
}

// ------- Delegation mode (ocr.delegate) -------

// runDelegateReview is OCR's delegation mode: `ocr delegate preview` and
// `ocr delegate rule` provide the deterministic engineering (mode/ref
// metadata, the reviewable file list, the resolved rule groups), and the
// pipeline's own review agent - the configured `agent`, e.g. opencode - does
// the LLM review with its existing subscription. No OCR-side LLM endpoint is
// configured or consulted, so a machine that runs the pipeline's agents but
// has no separate OCR API key can still power this gate.
//
// The agent produces review-shaped structured output (reviewFindingsSchema),
// so findings, fix rounds, carry-forward, and PR rendering behave exactly as
// they do for the review step. The coverage record is the ocr delegate file
// list, and a clean verdict still requires every selected file to be covered.
func (s *OCRStep) runDelegateReview(sctx *pipeline.StepContext, baseSHA, headSHA, fixSummary string) (*pipeline.StepOutcome, error) {
	preview, err := s.delegatePreview(sctx, baseSHA, headSHA)
	if err != nil {
		return nil, err
	}
	if len(preview.ReviewableFiles) == 0 {
		sctx.Log("ocr delegate: no files eligible for review")
		findings := Findings{Summary: "OpenCodeReview: no files eligible for review"}
		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{
			Findings:   string(findingsJSON),
			FixSummary: fixSummary,
		}, nil
	}

	reviewable := make([]string, 0, len(preview.ReviewableFiles))
	for _, file := range preview.ReviewableFiles {
		reviewable = append(reviewable, file.Path)
	}
	sort.Strings(reviewable)

	rules, err := s.delegateRules(sctx, reviewable)
	if err != nil {
		return nil, err
	}

	prompt := s.delegatePrompt(sctx, preview, rules, baseSHA, headSHA)
	sctx.Log("asking the pipeline review agent to review the ocr-delegated diff...")

	opts := agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		Env:        sctx.Env,
		JSONSchema: reviewFindingsSchema,
		OnChunk:    sctx.LogChunk,
		Purpose:    "ocr-delegate-review",
	}
	var findings Findings
	for attempt := 1; ; attempt++ {
		result, err := s.runDelegateAgent(sctx, opts)
		if err == nil {
			findings, err = parseReviewAnalyzerOutput(result)
			if err == nil {
				break
			}
		} else if !agent.IsStructuredOutputRejected(err) || sctx.Ctx.Err() != nil || errors.Is(err, errReviewAgentTimeout) {
			return nil, err
		}
		if attempt == reviewAnalyzerMaxAttempts {
			return nil, fmt.Errorf("validate ocr delegate findings after %d attempts: %w", reviewAnalyzerMaxAttempts, err)
		}
		sctx.Log(fmt.Sprintf("ocr delegate findings rejected (%s); rerunning the review (attempt %d of %d)", strings.ReplaceAll(err.Error(), "\n", "; "), attempt+1, reviewAnalyzerMaxAttempts))
		opts.Prompt = prompt + reviewRetryNote(err)
	}

	if stripped, n := stripDeferredPipelineOwnedDeliveryFindings(findings); n > 0 {
		sctx.Log(fmt.Sprintf("dropped %d deferred pipeline-owned delivery finding(s) (owned by later push/PR/CI steps)", n))
		findings = stripped
	}

	needsApproval := hasBlockingFindings(findings.Items)
	if !needsApproval && !reviewedPathsCoverReviewable(findings.ReviewedPaths, reviewable) {
		// A clean delegated review certifies the OCR-selected file set, so it
		// is held to the same positive coverage record the review step demands
		// (VISION.md R4): an omitted reviewed_paths is never a legacy pass.
		sctx.Log(uncoveredReviewMessage(findings.ReviewedPaths, reviewable))
		needsApproval = true
	}
	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: needsApproval,
		AutoFixable:   len(findings.Items) > 0,
		Findings:      string(findingsJSON),
		ExitCode:      0,
		FixSummary:    fixSummary,
	}, nil
}

// delegatePreview runs `ocr delegate preview --format json` over the run's
// diff. The command needs no LLM: it only resolves the diff, applies the
// file filter (including the repository's ignore patterns passed through
// --exclude), and lists what a review would cover.
func (s *OCRStep) delegatePreview(sctx *pipeline.StepContext, baseSHA, headSHA string) (*ocrDelegatePreview, error) {
	cmd := "ocr delegate preview --repo " + sctx.WorkDir + " --from " + baseSHA + " --to " + headSHA + " --format json"
	if len(sctx.Config.IgnorePatterns) > 0 {
		cmd += " --exclude " + strings.Join(sctx.Config.IgnorePatterns, ",")
	}
	if sctx.Config.OCR.Background {
		if intent := strings.TrimSpace(sctx.UserIntent); intent != "" {
			cmd += " --background " + intent
		}
	}
	output, exitCode, err := s.executeOcr(sctx, cmd)
	if err != nil {
		return nil, err
	}
	if exitCode != 0 {
		return nil, s.commandFailure(sctx, cmd, output, exitCode)
	}
	var preview ocrDelegatePreview
	if err := json.Unmarshal([]byte(output), &preview); err != nil {
		return nil, fmt.Errorf("decode ocr delegate preview output: %w", err)
	}
	if preview.SchemaVersion == "" {
		return nil, fmt.Errorf("ocr delegate preview output is not a delegation preview document")
	}
	return &preview, nil
}

// delegateRules runs `ocr delegate rule --format json` over the reviewable
// file paths and returns the resolved rule groups. The rule command is a pure
// function of the file paths and OCR's rule.json; it makes no LLM calls.
func (s *OCRStep) delegateRules(sctx *pipeline.StepContext, paths []string) ([]ocrDelegateRuleGroup, error) {
	cmd := "ocr delegate rule --repo " + sctx.WorkDir + " --format json " + strings.Join(paths, " ")
	output, exitCode, err := s.executeOcr(sctx, cmd)
	if err != nil {
		return nil, err
	}
	if exitCode != 0 {
		return nil, s.commandFailure(sctx, cmd, output, exitCode)
	}
	var rules ocrDelegateRules
	if err := json.Unmarshal([]byte(output), &rules); err != nil {
		return nil, fmt.Errorf("decode ocr delegate rule output: %w", err)
	}
	if rules.SchemaVersion == "" {
		return nil, fmt.Errorf("ocr delegate rule output is not a delegation rules document")
	}
	return rules.Groups, nil
}

// delegatePrompt assembles the review prompt handed to the pipeline agent: the
// fixed context and the finding contract (same vocabulary as the review step),
// plus the deterministic OCR delegation scaffold - refs, the reviewable file
// list with its diff stats, the excluded files and reasons, and the rule
// groups. The scaffold is the whole point: the agent's review is scoped to
// exactly what OCR's filter selected and guided by OCR's resolved rules.
func (s *OCRStep) delegatePrompt(sctx *pipeline.StepContext, preview *ocrDelegatePreview, rules []ocrDelegateRuleGroup, baseSHA, headSHA string) string {
	var scaffold strings.Builder
	fmt.Fprintf(&scaffold, "- mode: %s\n", preview.Mode)
	if preview.MergeBase != "" {
		fmt.Fprintf(&scaffold, "- merge base: %s\n", preview.MergeBase)
	}
	fmt.Fprintf(&scaffold, "- reviewable files (%d, +%d/-%d):\n", len(preview.ReviewableFiles), preview.TotalInsertions, preview.TotalDeletions)
	for _, file := range preview.ReviewableFiles {
		fmt.Fprintf(&scaffold, "  - %s [%s] +%d/-%d\n", file.Path, file.Status, file.Insertions, file.Deletions)
	}
	if len(preview.ExcludedFiles) > 0 {
		fmt.Fprintf(&scaffold, "- excluded files (%d):\n", len(preview.ExcludedFiles))
		for _, file := range preview.ExcludedFiles {
			fmt.Fprintf(&scaffold, "  - %s (%s)\n", file.Path, file.ExcludeReason)
		}
	}
	fmt.Fprintf(&scaffold, "\nReview rules resolved for the files (from OpenCodeReview rule.json; follow them as checklists):\n")
	for _, group := range rules {
		fmt.Fprintf(&scaffold, "\nGroup %d - %s (pattern %q) applies to: %s\n%s\n", group.GroupID, sourceOrUnknown(group.Source), group.Pattern, strings.Join(group.Files, ", "), strings.TrimSpace(group.Rule))
	}

	historySection := executionContextPromptSection(sctx.WorkDir) + roundHistoryPromptSection(sctx) + uncertifiedRoundHistoryPromptSection(sctx) + fixRoundProvenanceClause(sctx) + userIntentPromptSection(sctx) + pipelineDeliveryPhaseClause()
	return fmt.Sprintf(
		`Review the changed files selected by OpenCodeReview's deterministic filter and rules, and return structured findings with a risk assessment.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- review scope: branch changes between %s and %s
- default branch: %s

OpenCodeReview delegation scaffold (deterministic, from the ocr delegate commands; no OCR-side LLM was consulted):
%s
Task:
- Read the diff between the base commit and the target commit yourself, using the merge base above when given.
- Review exactly the reviewable files listed in the scaffold. For each file, follow its rule group's checklist and inspect surrounding code, call sites, shared helpers, tests, and invariants when needed to understand root cause.
- Focus findings on risks introduced by the changed code.

Rules:
- Anchor every finding to a specific file and one-indexed line number in the changed code when possible.
- When you report a defect, enumerate in that same finding every other place in the changed code where the same invariant is violated or must hold (a sibling call path, command, action, or state transition; another consumer of the same input, field, or record), each as file:line with a few words, so one fix round can close the class.
- Use severity "error" for problems that should absolutely not get merged, "warning" for things that are worth addressing but can be done in a follow up, and "info" for things that are nice to have.
- Be concise and actionable. No generic advice like "add more tests". Only comment on things that genuinely matter. Do NOT report styling, formatting, linting, compilation, or type-checking issues.
- If the change is clean, return an empty findings array.
- For each finding, set the action field to one of:
  - "ask-user": the finding is about functional requirements or product behavior, or otherwise challenges the author's deliberate intent. When in doubt, default to "ask-user".
  - "auto-fix": the finding is a non-functional, non user-visible issue (correctness, error handling, security, performance, mechanical code quality) that can be safely fixed without any discussion about the author's intent.
  - "no-op": the finding is informational and does not require any action.
- Classify by the remedy, not only by the topic: if the smallest honest remedy would EXTEND the change beyond its stated intent (new durable state, a schema change, new background/retry/persistence machinery, a new subsystem), the action must be "ask-user".
- For each finding, set review_scope to exactly one of "source", "pipeline-owned-delivery", or "external-delivery" (only a finding whose sole claim is that this run's remote branch, push, PR, or CI output is not present yet is pipeline-owned-delivery).
- Report a finding only when you can construct a concrete sequence that occurs during the change's intended usage, including rare but real sequences those callers actually perform. Do not report a finding whose only supporting path is a hypothetical unused execution.
- Do not infer a systemic flaw from code shape, duplication, or architectural preference alone. Do not demand a shared abstraction or broad redesign without a concrete reachable path, violated invariant, or immediately competing semantic owner.
- Do not block explicitly authorized honest containment merely because a later durable fix is possible.
- Do NOT run tests during review. The pipeline has a dedicated test step after review.
- Report reviewed_paths as the exact set of opencoded reviewable files you actually read and judged in this pass. It is a coverage record, not a summary: list a reviewable file only if your findings verdict for it is current, and never list a file you did not examine or a file outside the reviewable list. A reviewable file you omit is treated as unreviewed by the pipeline, never as clean.

Risk assessment (after listing all findings):
- Set risk_level to "low" if the change is well-bounded, mostly cosmetic, or straightforward with little ambiguity; "medium" if it has room to improve but is safe to merge first with concerns addressed as follow-ups; "high" if it should not be merged without explicit human approval.
- Provide a one-sentence risk_rationale explaining why you chose that risk level.
- Set risk_scope to "source-or-external" when the assessment reflects source risk or enforceable external state, and to "pipeline-owned-delivery" only when it is based solely on a deferred outcome this run owns.%s`,
		sctx.Run.Branch,
		baseSHA,
		headSHA,
		baseSHA,
		headSHA,
		sctx.Repo.DefaultBranch,
		scaffold.String(),
		historySection,
	)
}

// sourceOrUnknown renders a rule group's source, falling back to the built-in
// ruleset label when the OCR CLI did not name one.
func sourceOrUnknown(source string) string {
	if strings.TrimSpace(source) == "" {
		return "built-in rules"
	}
	return source
}

// runDelegateAgent runs one review turn through the pipeline's own agent with
// the same fresh wall-clock deadline as the review step's turns, so a stalled
// agent is bounded without charging the next turn.
func (s *OCRStep) runDelegateAgent(sctx *pipeline.StepContext, opts agent.RunOpts) (*agent.Result, error) {
	ctx, cancel, timeout := ocrAgentContext(sctx.Ctx, sctx.Config)
	defer cancel()
	result, err := sctx.RunAgentSessionContext(ctx, "", opts)
	if err != nil {
		err = reviewAgentError(ctx, timeout, "agent review", err)
	}
	return result, err
}

// ocrAgentContext assembles the absolute wall-clock deadline for one delegate
// review turn, mirroring the review step's own deadline derivation.
func ocrAgentContext(parent context.Context, cfg *config.Config) (context.Context, context.CancelFunc, time.Duration) {
	timeout := config.DefaultReviewAgentTimeout
	if cfg != nil && cfg.ReviewAgentTimeout > 0 {
		timeout = cfg.ReviewAgentTimeout
	}
	ctx, cancel := context.WithDeadlineCause(parent, time.Now().Add(timeout), errReviewAgentTimeout)
	return ctx, cancel, timeout
}

// ocrDelegatePreview is the JSON document `ocr delegate preview --format json`
// emits (schema_version 1). Field names track the OCR CLI's delegate output.
type ocrDelegatePreview struct {
	SchemaVersion   string                   `json:"schema_version"`
	Mode            string                   `json:"mode"`
	Repository      string                   `json:"repository"`
	From            string                   `json:"from,omitempty"`
	To              string                   `json:"to,omitempty"`
	Commit          string                   `json:"commit,omitempty"`
	MergeBase       string                   `json:"merge_base,omitempty"`
	Background      string                   `json:"background,omitempty"`
	TotalFiles      int                      `json:"total_files"`
	ReviewableCount int                      `json:"reviewable_count"`
	ExcludedCount   int                      `json:"excluded_count"`
	TotalInsertions int64                    `json:"total_insertions"`
	TotalDeletions  int64                    `json:"total_deletions"`
	ReviewableFiles []ocrDelegatePreviewFile `json:"reviewable_files"`
	ExcludedFiles   []ocrDelegatePreviewFile `json:"excluded_files"`
}

type ocrDelegatePreviewFile struct {
	Path          string `json:"path"`
	Status        string `json:"status"`
	Insertions    int64  `json:"insertions"`
	Deletions     int64  `json:"deletions"`
	ExcludeReason string `json:"exclude_reason,omitempty"`
}

// ocrDelegateRules is the JSON document `ocr delegate rule --format json`
// emits: rule groups, each naming the files it applies to and the rule text.
type ocrDelegateRules struct {
	SchemaVersion string                 `json:"schema_version"`
	Groups        []ocrDelegateRuleGroup `json:"groups"`
}

type ocrDelegateRuleGroup struct {
	GroupID int      `json:"group_id"`
	Source  string   `json:"source"`
	Pattern string   `json:"pattern"`
	Files   []string `json:"files"`
	Rule    string   `json:"rule"`
}
