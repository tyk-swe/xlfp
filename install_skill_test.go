package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMuseSkillInstallSourceSurvives(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell installer")
	}
	for _, name := range []string{"sh", "sed"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s unavailable", name)
		}
	}
	for _, tc := range []struct {
		name             string
		download, dryRun bool
	}{{"checkout", false, false}, {"download", true, false}, {"download dry run", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cache := filepath.Join(dir, "cache source's files")
			t.Setenv("XDG_CACHE_HOME", cache)
			script, err := filepath.Abs("scripts/install-skill.sh")
			if err != nil {
				t.Fatal(err)
			}
			if tc.download {
				raw, err := os.ReadFile(script)
				if err != nil {
					t.Fatal(err)
				}
				script = filepath.Join(dir, "install-skill.sh")
				if err := os.WriteFile(script, raw, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			mockbin := filepath.Join(dir, "mockbin")
			if err := os.Mkdir(mockbin, 0o700); err != nil {
				t.Fatal(err)
			}
			curl := `#!/bin/sh
while [ "$#" -gt 0 ]; do
  if [ "$1" = "-o" ]; then printf 'downloaded skill\n' > "$2"; exit 0; fi
  shift
done
exit 1
`
			muse := `#!/bin/sh
[ "$#" -eq 3 ] && [ "$1" = skills ] && [ "$2" = install ] && [ -s "$3/SKILL.md" ]
`
			for name, content := range map[string]string{"curl": curl, "muse": muse} {
				if err := os.WriteFile(filepath.Join(mockbin, name), []byte(content), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", mockbin+string(os.PathListSeparator)+os.Getenv("PATH"))
			args := []string{script, "--target", "muse"}
			if tc.dryRun {
				args = append(args, "--dry-run")
			}
			cmd := exec.Command("sh", args...)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("installer: %v\n%s", err, out)
			}
			if tc.dryRun {
				if _, err := os.Stat(cache); !os.IsNotExist(err) {
					t.Fatalf("dry run created persistent cache: %v", err)
				}
				return
			}
			for _, line := range strings.Split(string(out), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "muse skills install ") {
					// Execute only the printed command, against a stub Muse CLI.
					cmd := exec.Command("sh", "-c", line)
					if output, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("printed command cannot use its source: %v\n%s", err, output)
					}
					return
				}
			}
			t.Fatalf("installer did not print a Muse command:\n%s", out)
		})
	}
}
