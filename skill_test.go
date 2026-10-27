package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSkillFile keeps the shipped agent skill (skills/xlfp/SKILL.md)
// conformant with the shared Agent Skills format: YAML frontmatter with
// a name matching the directory and a non-empty description within the
// 1024-character limit. Full validation: muse skills validate
// skills/xlfp --json.
func TestSkillFile(t *testing.T) {
	path := filepath.Join("skills", "xlfp", "SKILL.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	if !strings.HasPrefix(content, "---\n") {
		t.Fatal("SKILL.md must start with YAML frontmatter")
	}
	front, _, found := strings.Cut(strings.TrimPrefix(content, "---\n"), "\n---")
	if !found {
		t.Fatal("frontmatter is not closed")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(front, "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if ok {
			fields[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	if fields["name"] != "xlfp" {
		t.Errorf("name = %q, want %q (must match directory name)", fields["name"], "xlfp")
	}
	if fields["license"] != "GPL-3.0-only" {
		t.Errorf("license = %q, want GPL-3.0-only (must match LICENSE)", fields["license"])
	}
	if desc := fields["description"]; desc == "" || len(desc) > 1024 {
		t.Errorf("description length %d, want 1..1024", len(desc))
	}
	// The body must keep the safety-critical guidance agents rely on.
	for _, want := range []string{"--json", "XLFP_API_KEY", "reason_code", "not proof of fraud"} {
		if !strings.Contains(content, want) {
			t.Errorf("SKILL.md missing required guidance %q", want)
		}
	}
}
