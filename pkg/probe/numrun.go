package probe

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/tyk-swe/xlfp/pkg/numprint"
)

// NumOptions controls a live number-fingerprint run.
type NumOptions struct {
	Rounds   int // sequential rounds (default 1)
	Count    int // samples per round, 1..3 (default 3)
	Parallel int // concurrent samples per round (default: Count)
	Strict   bool
	Stream   bool
	// Challenges, when set, are reused every round; its length
	// must equal Count. Otherwise fresh random challenges are
	// drawn per round.
	Challenges  []numprint.Challenge
	Effort      string
	ServiceTier string
	Rand        *rand.Rand // test seam; nil seeds from the clock
	OnSample    func(round, doneInRound int)
}

// NumRound is one scored round with its raw evidence.
type NumRound struct {
	Round      int                  `json:"round"`
	Challenges []numprint.Challenge `json:"challenges"`
	Answers    []numprint.Answer    `json:"answers"`
	Result     numprint.Result      `json:"result"`
}

// NumRunResult is a full live run.
type NumRunResult struct {
	Rounds        []NumRound `json:"rounds"`
	MostFrequent  string     `json:"most_frequent_top,omitempty"` // most common round winner (entry id), not a combined probability
	RequestsSpent int        `json:"requests_spent"`
}

// RunNumbers collects and scores number-fingerprint rounds. All
// samples in a round settle before the next round starts; a failed
// sample is recorded, never retried, and never stops later rounds.
// Every Chat attempt counts toward RequestsSpent.
func RunNumbers(ctx context.Context, c *Client, bank *numprint.Bank, opts NumOptions) NumRunResult {
	rounds := opts.Rounds
	if rounds <= 0 {
		rounds = 1
	}
	count := opts.Count
	if count <= 0 {
		count = 3
	}
	parallel := opts.Parallel
	if parallel <= 0 || parallel > count {
		parallel = count
	}
	rng := opts.Rand
	if rng == nil {
		rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	start := c.Requests()
	out := NumRunResult{}
	wins := map[string]int{}
	for round := 1; round <= rounds; round++ {
		chs := opts.Challenges
		if chs == nil {
			chs = numprint.GenerateChallenges(rng, count)
		}
		answers := make([]numprint.Answer, len(chs))
		sem := make(chan struct{}, parallel)
		var wg sync.WaitGroup
		var mu sync.Mutex
		done := 0
		for i, ch := range chs {
			wg.Add(1)
			go func(i int, ch numprint.Challenge) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				ans := numprint.Answer{ChallengeID: ch.ID, ExpectedCount: ch.ExpectedCount}
				req := ChatRequest{User: ch.Prompt, Stream: opts.Stream, Effort: opts.Effort, ServiceTier: opts.ServiceTier}
				if opts.Stream && !opts.Strict {
					expected := ch.ExpectedCount
					req.StopWhen = func(text string) bool {
						return numprint.CountIntegers(text) >= expected
					}
				}
				res, err := c.Chat(ctx, req)
				switch {
				case err != nil:
					ans.Error = err.Error()
					ans.Text = res.Text
				case res.Error != "":
					ans.Error = res.Error
					ans.Text = res.Text
				default:
					ans.Text = res.Text
					ans.Truncated = res.Truncated
					ans.Completed = res.Completed
				}
				mu.Lock()
				answers[i] = ans
				done++
				if opts.OnSample != nil {
					opts.OnSample(round, done)
				}
				mu.Unlock()
			}(i, ch)
		}
		wg.Wait()
		res := bank.Score(answers, numprint.ScoreOptions{Strict: opts.Strict})
		if res.Status == numprint.StatusRanked && res.TopEntryID != "" {
			wins[res.TopEntryID]++
		}
		out.Rounds = append(out.Rounds, NumRound{Round: round, Challenges: chs, Answers: answers, Result: res})
		if ctx.Err() != nil {
			break
		}
	}
	best, bestN := "", 0
	for id, n := range wins {
		if n > bestN || (n == bestN && id < best) {
			best, bestN = id, n
		}
	}
	out.MostFrequent = best
	out.RequestsSpent = c.Requests() - start
	return out
}
