package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestRunRadarJSON(t *testing.T) {
	root := t.TempDir()
	fr := &fakeRadar{result: radarResult{
		Hotspots: []radarHotspot{
			{File: "src/a.ts", RiskScore: 6, References: 1, Complexity: 5, Escapes: 1, Rework: 0, DeadCode: false, Duplicate: false},
			{File: "src/b.ts", RiskScore: 2, References: 1, Complexity: 1, Escapes: 1, Rework: 0, DeadCode: true, Duplicate: false},
		},
		Skipped: []radarSkipped{{Dimension: "duplicate", Tool: "jscpd", Reason: "jscpd not found"}},
	}}
	withDependencies(t, dependencies{engine: &fakeEngine{}, radar: fr})

	var stdout, stderr bytes.Buffer
	code := runRadar([]string{"--root", root, "--json", "--top", "2"}, &stdout, &stderr)
	if code != ExitOK {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if fr.lastTop != 2 {
		t.Fatalf("top = %d, want 2", fr.lastTop)
	}
	var got radarResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode radar JSON: %v\n%s", err, stdout.String())
	}
	if len(got.Hotspots) != 2 || got.Hotspots[0].File != "src/a.ts" || got.Hotspots[0].RiskScore != 6 {
		t.Fatalf("hotspots = %+v", got.Hotspots)
	}
	if len(got.Skipped) != 1 || got.Skipped[0].Dimension != "duplicate" {
		t.Fatalf("skipped = %+v", got.Skipped)
	}
}

func TestRunRadarHumanAndDefaultTop(t *testing.T) {
	root := t.TempDir()
	fr := &fakeRadar{result: radarResult{
		Hotspots: []radarHotspot{{File: "src/a.ts", RiskScore: 6, References: 1, Complexity: 5, Escapes: 1}},
		Skipped:  []radarSkipped{{Dimension: "dead_code", Tool: "knip", Reason: "knip not found"}},
	}}
	withDependencies(t, dependencies{engine: &fakeEngine{}, radar: fr})

	var stdout, stderr bytes.Buffer
	if code := runRadar([]string{"--root", root}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	if fr.lastTop != defaultRadarTop {
		t.Fatalf("top = %d, want %d", fr.lastTop, defaultRadarTop)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("FILE")) || !bytes.Contains(stdout.Bytes(), []byte("src/a.ts")) {
		t.Fatalf("human output:\n%s", stdout.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("skipped dead_code (knip)")) {
		t.Fatalf("skipped output missing:\n%s", stdout.String())
	}
}

func TestRunRadarErrors(t *testing.T) {
	withDependencies(t, dependencies{engine: &fakeEngine{}})
	var stdout, stderr bytes.Buffer
	if code := runRadar([]string{"--root", t.TempDir()}, &stdout, &stderr); code != ExitError {
		t.Fatalf("nil radar code = %d, want 2", code)
	}

	fr := &fakeRadar{err: errors.New("boom")}
	withDependencies(t, dependencies{engine: &fakeEngine{}, radar: fr})
	if code := runRadar([]string{"--root", t.TempDir()}, &stdout, &stderr); code != ExitError {
		t.Fatalf("radar error code = %d, want 2", code)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("run radar")) {
		t.Fatalf("stderr = %s", stderr.String())
	}
}
