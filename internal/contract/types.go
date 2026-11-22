package contract

import "time"

type VerifyStatus string

const (
	Pass        VerifyStatus = "PASS"
	Fail        VerifyStatus = "FAIL"
	MissingTool VerifyStatus = "MISSING_TOOL"
	Unsupported VerifyStatus = "UNSUPPORTED"
	NoChanges   VerifyStatus = "NO_CHANGES"
)

type VerifyScope string

const (
	ScopeChanged VerifyScope = "changed"
	ScopeAll     VerifyScope = "all"
)

type VerifyResult struct {
	Status            VerifyStatus      `json:"status"`
	ExitCode          int               `json:"exit_code"`
	ProjectPath       string            `json:"project_path"`
	Scope             VerifyScope       `json:"scope"`
	DetectedLanguages []string          `json:"detected_languages"`
	ExecutedCheckers  []string          `json:"executed_checkers"`
	MissingTools      []ToolRequirement `json:"missing_tools"`
	ScannedFilesCount int               `json:"scanned_files_count"`
	Diagnostics       []Diagnostic      `json:"diagnostics"`
	Warnings          []string          `json:"warnings"`
	DurationMs        int64             `json:"duration_ms"`
}

type ToolRequirement struct {
	Name           string `json:"name"`
	InstallCommand string `json:"install_command"`
}

type Diagnostic struct {
	Language    string `json:"language"`
	Checker     string `json:"checker"`
	RuleID      string `json:"rule_id"`
	Severity    string `json:"severity"`
	File        string `json:"file"`
	Line        int    `json:"line"`
	Column      int    `json:"column"`
	Message     string `json:"message"`
	CodeSnippet string `json:"code_snippet"`
}

type MistakeStatus string
type ProposalStatus string

const (
	MistakeCollecting    MistakeStatus = "COLLECTING"
	MistakeQueued        MistakeStatus = "QUEUED"
	MistakeReviewing     MistakeStatus = "REVIEWING"
	MistakeProposed      MistakeStatus = "PROPOSED"
	MistakeIgnoredByAI   MistakeStatus = "IGNORED_BY_AI"
	MistakeFailedSandbox MistakeStatus = "FAILED_SANDBOX"
	MistakeApproved      MistakeStatus = "APPROVED"
	MistakeDismissed     MistakeStatus = "DISMISSED"
	MistakeSnoozed       MistakeStatus = "SNOOZED"

	ProposalProposed      ProposalStatus = "PROPOSED"
	ProposalIgnoredByAI   ProposalStatus = "IGNORED_BY_AI"
	ProposalFailedSandbox ProposalStatus = "FAILED_SANDBOX"
	ProposalApproved      ProposalStatus = "APPROVED"
	ProposalDismissed     ProposalStatus = "DISMISSED"
	ProposalSnoozed       ProposalStatus = "SNOOZED"
)

type Decision string

const (
	Ignore    Decision = "IGNORE"
	HardRule  Decision = "HARD_RULE"
	GuardHint Decision = "GUARD_HINT"
)

type RuleProposal struct {
	ID                  string         `json:"id"`
	ProjectPath         string         `json:"project_path"`
	Fingerprint         string         `json:"fingerprint"`
	Decision            Decision       `json:"decision"`
	SummaryZH           string         `json:"summary_zh"`
	RuleID              string         `json:"rule_id"`
	SemgrepYAML         string         `json:"semgrep_yaml"`
	HintLine            string         `json:"hint_line"`
	SandboxProofHash    string         `json:"sandbox_proof_hash"`
	SandboxPositiveHits int            `json:"sandbox_positive_hits"`
	SandboxProjectHits  int            `json:"sandbox_project_hits"`
	SandboxDurationMs   int64          `json:"sandbox_duration_ms"`
	Status              ProposalStatus `json:"status"`
	AppliedPath         string         `json:"applied_path"`
	CreatedAt           time.Time      `json:"created_at"`
	UpdatedAt           time.Time      `json:"updated_at"`
}

type ReviewerDecision struct {
	Decision    Decision `json:"decision"`
	SummaryZH   string   `json:"summary_zh"`
	RuleID      string   `json:"rule_id"`
	SemgrepYAML string   `json:"semgrep_yaml"`
	HintLine    string   `json:"hint_line"`
}
