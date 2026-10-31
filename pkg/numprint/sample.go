package numprint

import "fmt"

// SamplingChallenge is one fixed reference-bank challenge: a prompt
// environment (optional system text and user prefix) plus a
// challenge. The fixed set keeps bank collection reproducible and
// lets retraining estimate prompt-environment shifts.
type SamplingChallenge struct {
	ID          string `json:"id"`
	Environment string `json:"environment"`
	System      string `json:"system,omitempty"`
	Prefix      string `json:"prefix,omitempty"`
	Challenge   Challenge
}

// Prompt renders the full user message for this sampling challenge.
func (s SamplingChallenge) Prompt() string { return s.Prefix + s.Challenge.Prompt }

var samplingSystems = []struct{ name, text string }{
	{"nosys", ""},
	{"helpful", "You are a helpful assistant."},
	{"brief", "Answer briefly and directly."},
}

var samplingPrefixes = []struct{ name, text string }{
	{"plain", ""},
	{"please", "Please do this now:\n"},
	{"quick", "Quick task -\n"},
	{"hi", "Hi! "},
}

// Fixed requested counts for the sampling set (kept well inside the
// generation budget; detection challenges draw their own counts).
var samplingCounts = []int{230, 271, 318}

// SamplingSet returns the fixed 36-challenge collection set:
// 3 system variants × 4 user-prefix variants = 12 environments,
// 3 challenges each, languages rotating deterministically.
func SamplingSet() []SamplingChallenge {
	var out []SamplingChallenge
	k := 0
	for _, sys := range samplingSystems {
		for _, pre := range samplingPrefixes {
			env := sys.name + "-" + pre.name
			for ci, n := range samplingCounts {
				lang := challengeLanguages[k%len(challengeLanguages)]
				tmpls := challengeTemplates[lang]
				tmpl := tmpls[k%len(tmpls)]
				out = append(out, SamplingChallenge{
					ID:          fmt.Sprintf("%s-%d", env, ci+1),
					Environment: env,
					System:      sys.text,
					Prefix:      pre.text,
					Challenge: Challenge{
						ID:            fmt.Sprintf("s%d", k+1),
						Language:      lang,
						ExpectedCount: n,
						Prompt:        fmt.Sprintf(tmpl, n),
					},
				})
				k++
			}
		}
	}
	return out
}
