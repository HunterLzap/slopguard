package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/HunterLzap/slopguard/internal/contract"
)

func TestEvaluateDiffRatchetAndImprovements(t *testing.T) {
	rules := []AdapterEscapeRule{
		{
			Extensions: []string{".ts"},
			Patterns: []contract.EscapePattern{
				{Name: "ts_ignore", Regex: `(?m)//\s*@ts-ignore`, Target: "comment"},
				{Name: "explicit_any", Regex: `(?m):\s*any\b`, Target: "code"},
			},
		},
	}

	// Case 1: adding 1 @ts-ignore triggers ratchet:ts_ignore.
	addDiff := `diff --git a/src/a.ts b/src/a.ts
index 1111111..2222222 100644
--- a/src/a.ts
+++ b/src/a.ts
@@ -4,0 +5,2 @@
+// @ts-ignore
+const s = "x: any"; // note: any
`
	filesAdd := ParseUnifiedDiff(addDiff)
	resAdd, err := EvaluateDiffRatchet(filesAdd, rules, NewBaselineFromMetrics(map[string]int{"ts_ignore": 2}, time.Time{}), nil)
	if err != nil {
		t.Fatalf("EvaluateDiffRatchet add error: %v", err)
	}
	if len(resAdd.Findings) != 1 || resAdd.Findings[0].Rule != "ratchet:ts_ignore" {
		t.Fatalf("findings = %+v, want single ratchet:ts_ignore (explicit_any must be 0)", resAdd.Findings)
	}

	// Case 2: removing 1 @ts-ignore produces Improvements and 0 regressions.
	delDiff := `diff --git a/src/a.ts b/src/a.ts
index 1111111..2222222 100644
--- a/src/a.ts
+++ b/src/a.ts
@@ -5,1 +4,0 @@
-// @ts-ignore
`
	filesDel := ParseUnifiedDiff(delDiff)
	resDel, err := EvaluateDiffRatchet(filesDel, rules, NewBaselineFromMetrics(map[string]int{"ts_ignore": 2}, time.Time{}), nil)
	if err != nil {
		t.Fatalf("EvaluateDiffRatchet del error: %v", err)
	}
	if len(resDel.Findings) != 0 {
		t.Fatalf("unexpected findings: %+v", resDel.Findings)
	}
	if len(resDel.Improvements) != 1 || resDel.Improvements[0].Name != "ts_ignore" || resDel.Improvements[0].Delta != -1 {
		t.Fatalf("improvements = %+v, want ts_ignore delta=-1", resDel.Improvements)
	}
}

func TestScanDiffProbesExactLinesAndActionHints(t *testing.T) {
	diffText := `diff --git a/src/handler.ts b/src/handler.ts
index 1111111..2222222 100644
--- a/src/handler.ts
+++ b/src/handler.ts
@@ -9,0 +10,5 @@
+const s = "do not remove workaround";
+// do not remove this block
+const x = 1;
+// workaround for bug
+// talk to @platform-team before changing
`
	files := ParseUnifiedDiff(diffText)
	findings := ScanDiffProbes(files, contract.SeverityError, nil, nil)
	if len(findings) != 3 {
		t.Fatalf("got %d findings, want 3: %+v", len(findings), findings)
	}

	if findings[0].File != "src/handler.ts" || findings[0].Line != 11 || findings[0].Rule != ProbeConstraintRuleID {
		t.Fatalf("finding[0] = %+v, want line 11 %s", findings[0], ProbeConstraintRuleID)
	}
	if !strings.Contains(findings[0].ActionHint, "type, runtime assertion, test, or lint rule") {
		t.Fatalf("finding[0].ActionHint = %q", findings[0].ActionHint)
	}

	if findings[1].File != "src/handler.ts" || findings[1].Line != 13 || findings[1].Rule != ProbeWorkaroundRuleID {
		t.Fatalf("finding[1] = %+v, want line 13 %s", findings[1], ProbeWorkaroundRuleID)
	}
	if !strings.Contains(findings[1].ActionHint, "minimal root-cause fix") {
		t.Fatalf("finding[1].ActionHint = %q", findings[1].ActionHint)
	}

	if findings[2].File != "src/handler.ts" || findings[2].Line != 14 || findings[2].Rule != ProbeConstraintRuleID {
		t.Fatalf("finding[2] = %+v, want line 14 %s", findings[2], ProbeConstraintRuleID)
	}
}
