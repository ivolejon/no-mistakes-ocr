package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
	"gopkg.in/yaml.v3"
)

func TestMerge_OCRDefaultsOff(t *testing.T) {
	merged := Merge(DefaultGlobalConfig(), &RepoConfig{})
	if merged.OCR.Enabled {
		t.Fatal("ocr.enabled must default to false: the gate is opt-in")
	}
	if merged.OCR.Effort != "medium" {
		t.Fatalf("ocr.effort default = %q, want medium", merged.OCR.Effort)
	}
	if merged.OCR.MinSeverity != "low" {
		t.Fatalf("ocr.min_severity default = %q, want low", merged.OCR.MinSeverity)
	}
	if merged.OCR.TimeoutMin != 15 {
		t.Fatalf("ocr.timeout_minutes default = %d, want 15", merged.OCR.TimeoutMin)
	}
	if merged.OCR.Delegate {
		t.Fatal("ocr.delegate must default to false: full `ocr review` needs an OCR-side LLM, so delegation is an explicit opt-in")
	}
	if !merged.OCR.Background {
		t.Fatal("ocr.background must default to true: the run's intent steers the review")
	}
}

func TestLoadRepo_OCRParsesAndValidates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".no-mistakes.yaml")
	if err := os.WriteFile(path, []byte(
		"ocr:\n"+
			"  enabled: true\n"+
			"  effort: high\n"+
			"  provider: anthropic\n"+
			"  model: claude-opus-4-6\n"+
			"  timeout_minutes: 45\n"+
			"  min_severity: medium\n"+
			"  delegate: true\n"+
			"  background: false\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}

	repo, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("LoadRepo: %v", err)
	}
	if repo.OCR.Enabled == nil || !*repo.OCR.Enabled {
		t.Fatal("ocr.enabled = false, want true")
	}
	if repo.OCR.Effort != "high" || repo.OCR.MinSeverity != "medium" || repo.OCR.TimeoutMin != 45 || !repo.OCR.Delegate {
		t.Fatalf("unexpected ocr block: %+v", repo.OCR)
	}
	merged := Merge(DefaultGlobalConfig(), repo)
	if !merged.OCR.Enabled || merged.OCR.Effort != "high" || merged.OCR.Provider != "anthropic" ||
		merged.OCR.Model != "claude-opus-4-6" || merged.OCR.TimeoutMin != 45 || merged.OCR.MinSeverity != "medium" || !merged.OCR.Delegate || merged.OCR.Background {
		t.Fatalf("unexpected merged ocr settings: %+v", merged.OCR)
	}
}

func TestLoadRepo_OCRRawValidationFailsClosed(t *testing.T) {
	tests := []struct {
		name, yaml string
	}{
		{"effort", "ocr:\n  effort: ultra\n"},
		{"min_severity", "ocr:\n  min_severity: severe\n"},
		{"negative_timeout", "ocr:\n  timeout_minutes: -1\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseRepoConfig([]byte(tt.yaml)); err == nil {
				t.Fatalf("expected %q to fail the config closed", tt.yaml)
			}
		})
	}
}

// The whole ocr block is trusted-only: it gates the branch that validates a
// run, so a pushed branch must not be able to switch itself out of it or
// change what it runs, regardless of allow_repo_commands.
func TestEffectiveRepoConfig_OCRBlockTrustedOnly(t *testing.T) {
	pushed := &RepoConfig{OCR: OCRRaw{Enabled: boolPtr(false), Effort: "low", MinSeverity: "high", Delegate: true}}
	trusted := &RepoConfig{OCR: OCRRaw{Enabled: boolPtr(true), Effort: "medium", MinSeverity: "low"}}

	effective := EffectiveRepoConfig(pushed, trusted, false)
	if effective.OCR.Enabled == nil || !*effective.OCR.Enabled {
		t.Fatal("ocr.enabled = false, want the trusted copy's enabled=true")
	}
	if effective.OCR.Effort != "medium" || effective.OCR.MinSeverity != "low" || effective.OCR.Delegate {
		t.Fatalf("pushed ocr values leaked through: %+v", effective.OCR)
	}

	// Disabling the gate on the pushed branch must not survive either.
	pushed = &RepoConfig{OCR: OCRRaw{Enabled: boolPtr(false)}}
	effective = EffectiveRepoConfig(pushed, trusted, true)
	if effective.OCR.Enabled == nil || !*effective.OCR.Enabled {
		t.Fatal("a pushed branch must not disable the ocr gate even under allow_repo_commands")
	}

	// Without a trusted copy the gate is off entirely: the built-in default.
	effective = EffectiveRepoConfig(pushed, nil, false)
	if effective.OCR.Enabled != nil && *effective.OCR.Enabled {
		t.Fatal("ocr.enabled must resolve off when there is no trusted copy")
	}
}

func TestMerge_AutoFixOCRDefaultsToThreeRounds(t *testing.T) {
	merged := Merge(DefaultGlobalConfig(), &RepoConfig{})
	if got := merged.AutoFixLimit(types.StepOCR); got != 3 {
		t.Fatalf("auto_fix.ocr default = %d, want 3 (the gate fixes what it finds)", got)
	}
	if merged.AutoFixLimit(types.StepReview) != 0 {
		t.Fatal("review must keep its auto-fix-off default")
	}
}

func TestLoadRepo_AutoFixOCRRespectsExplicitDisable(t *testing.T) {
	repo := &RepoConfig{}
	if err := yaml.Unmarshal([]byte("auto_fix:\n  ocr: 0\n"), repo); err != nil {
		t.Fatal(err)
	}
	merged := Merge(DefaultGlobalConfig(), repo)
	if merged.AutoFixLimit(types.StepOCR) != 0 {
		t.Fatal("explicit auto_fix.ocr: 0 must disable the auto-fix loop")
	}
}
