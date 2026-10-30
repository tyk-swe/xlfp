package numprint

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// ParseSequence extracts the longest run of integers in
// ValueMin..ValueMax from a reply. Runs are sequences of integer
// tokens separated only by whitespace or common separators
// (commas, semicolons); a letter or an out-of-range token ends a
// run. Signed or decimal tokens are not integers here.
func ParseSequence(text string) []int {
	var best, cur []int
	flush := func() {
		if len(cur) > len(best) {
			best = append([]int(nil), cur...)
		}
		cur = cur[:0]
	}
	i := 0
	runes := []rune(text)
	for i < len(runes) {
		r := runes[i]
		switch {
		case r >= '0' && r <= '9':
			j := i
			for j < len(runes) && runes[j] >= '0' && runes[j] <= '9' {
				j++
			}
			// A digit run glued to a letter, decimal point, or
			// leading sign is not a standalone unsigned integer.
			afterOK := j >= len(runes) || !(unicode.IsLetter(runes[j]) || runes[j] == '.')
			beforeOK := i == 0 || !(unicode.IsLetter(runes[i-1]) || runes[i-1] == '.' || runes[i-1] == '-' || runes[i-1] == '+' || runes[i-1] == '−')
			v, err := strconv.Atoi(string(runes[i:j]))
			if err == nil && afterOK && beforeOK && v >= ValueMin && v <= ValueMax {
				cur = append(cur, v)
			} else {
				flush()
			}
			i = j
		case r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ',' || r == ';' || r == '、' || r == '，':
			i++
		default:
			flush()
			i++
		}
	}
	flush()
	return best
}

// CountIntegers counts complete integers in ParseSequence's longest
// run for streaming early-stop. Trailing digits may continue in the
// next delta, so they count only once a delimiter arrives. Naturally
// completed replies use ParseSequence directly and keep the last value.
func CountIntegers(text string) int {
	return len(ParseSequence(strings.TrimRight(text, "0123456789")))
}

// evidenceSequence applies the same requested-count cap to queries,
// references, calibration, and feature export. Retained raw evidence
// may include additional values; they do not affect the detector.
func evidenceSequence(seq []int, expected int) []int {
	if expected > 0 && len(seq) > expected {
		return seq[:expected]
	}
	return seq
}

// SequenceStats summarizes one parsed sequence for the reply check.
type SequenceStats struct {
	Autocorr  float64 // lag-1 autocorrelation
	SmallStep float64 // share of |Δ| <= 20 steps
	Min, Max  int
}

// Stats computes the reply-check statistics.
func Stats(seq []int) SequenceStats {
	st := SequenceStats{Min: ValueMax, Max: ValueMin}
	if len(seq) == 0 {
		return st
	}
	mean := 0.0
	for _, v := range seq {
		mean += float64(v)
		st.Min = min(st.Min, v)
		st.Max = max(st.Max, v)
	}
	mean /= float64(len(seq))
	if len(seq) > 1 {
		var num, den float64
		small := 0
		for k, v := range seq {
			d := float64(v) - mean
			den += d * d
			if k > 0 {
				num += d * (float64(seq[k-1]) - mean)
				step := v - seq[k-1]
				if step < 0 {
					step = -step
				}
				if step <= 20 {
					small++
				}
			}
		}
		if den > 0 {
			st.Autocorr = num / den
		}
		st.SmallStep = float64(small) / float64(len(seq)-1)
	}
	return st
}

const (
	valueDims   = ValueMax // one per value 1..355
	posBlocks   = 4
	posBins     = 16
	posDims     = posBlocks*posBins + 10
	featureDims = valueDims + posDims
	valueWeight = 0.70
	smoothing   = 0.5
)

// Featurize maps a sequence to the comparison vector: a Hellinger
// embedding of the value distribution (Euclidean distance is then
// proportional to Hellinger distance), concatenated with
// position-block and final-digit distributions, each block
// unit-normalized and weighted valueWeight : 1-valueWeight.
func Featurize(seq []int) []float64 {
	out := make([]float64, featureDims)
	if len(seq) == 0 {
		return out
	}
	counts := make([]float64, valueDims)
	for _, v := range seq {
		if v >= ValueMin && v <= ValueMax {
			counts[v-ValueMin]++
		}
	}
	n := float64(len(seq))
	for i := range counts {
		out[i] = math.Sqrt((counts[i] + smoothing) / (n + valueDims*smoothing))
	}
	// Position blocks: 4 quarters × 16 value bins, then final digits.
	pos := out[valueDims:]
	for b := 0; b < posBlocks; b++ {
		lo := b * len(seq) / posBlocks
		hi := (b + 1) * len(seq) / posBlocks
		bins := make([]float64, posBins)
		for k := lo; k < hi; k++ {
			bin := (seq[k] - ValueMin) * posBins / (ValueMax - ValueMin + 1)
			bins[bin]++
		}
		blockN := float64(max(hi-lo, 1))
		for bi := range bins {
			pos[b*posBins+bi] = math.Sqrt((bins[bi] + smoothing) / (blockN + posBins*smoothing))
		}
	}
	digits := make([]float64, 10)
	for _, v := range seq {
		digits[v%10]++
	}
	for d := range digits {
		pos[posBlocks*posBins+d] = math.Sqrt((digits[d] + smoothing) / (n + 10*smoothing))
	}
	normalize(pos)
	sv, sp := math.Sqrt(valueWeight), math.Sqrt(1-valueWeight)
	for i := 0; i < valueDims; i++ {
		out[i] *= sv
	}
	for i := range pos {
		pos[i] *= sp
	}
	return out
}

func normalize(v []float64) {
	norm := 0.0
	for _, x := range v {
		norm += x * x
	}
	if norm <= 0 {
		return
	}
	s := 1 / math.Sqrt(norm)
	for i := range v {
		v[i] *= s
	}
}

func cosine(a, b []float64) float64 {
	dot, na, nb := 0.0, 0.0, 0.0
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na <= 0 || nb <= 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}

func sqDist(a, b []float64) float64 {
	s := 0.0
	for i := range a {
		d := a[i] - b[i]
		s += d * d
	}
	return s
}

func meanVec(vs [][]float64) []float64 {
	if len(vs) == 0 {
		return nil
	}
	out := make([]float64, len(vs[0]))
	for _, v := range vs {
		for i := range out {
			out[i] += v[i]
		}
	}
	for i := range out {
		out[i] /= float64(len(vs))
	}
	return out
}

func medianFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// sequenceString renders a sequence for offline report files.
func sequenceString(seq []int) string {
	parts := make([]string, len(seq))
	for i, v := range seq {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, " ")
}
