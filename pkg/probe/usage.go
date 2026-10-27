package probe

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/tyk-swe/xlfp/pkg/config"
)

// flexInt accepts exact, non-negative JSON integers, including integral
// decimal/exponent notation and numeric strings. Never round through float64:
// doing so can turn a fractional or overflowing count into plausible evidence.
type flexInt int

func (f *flexInt) UnmarshalJSON(raw []byte) error {
	s := strings.TrimSpace(string(raw))
	if strings.HasPrefix(s, "\"") {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return err
		}
		s = strings.TrimSpace(str)
	}
	invalid := func() error { return fmt.Errorf("invalid non-negative token count") }
	if s == "" || (s[0] != '-' && (s[0] < '0' || s[0] > '9')) || !json.Valid([]byte(s)) {
		return invalid()
	}
	var exponent int64
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		var err error
		exponent, err = strconv.ParseInt(s[i+1:], 10, 32)
		if err != nil {
			return invalid()
		}
		s = s[:i]
	}
	negative := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	if i := strings.IndexByte(s, '.'); i >= 0 {
		exponent -= int64(len(s) - i - 1)
		s = s[:i] + s[i+1:]
	}
	s = strings.TrimLeft(s, "0")
	if s == "" {
		*f = 0
		return nil
	}
	if negative {
		return invalid()
	}
	if exponent < 0 {
		if -exponent >= int64(len(s)) {
			return invalid()
		}
		cut := len(s) + int(exponent)
		if strings.Trim(s[cut:], "0") != "" {
			return invalid()
		}
		s = s[:cut]
		exponent = 0
	}
	// Bound the length before allocating exponent-expanded text.
	maxDigits := len(strconv.FormatUint(uint64(^uint(0)>>1), 10))
	if int64(len(s))+exponent > int64(maxDigits) {
		return invalid()
	}
	s += strings.Repeat("0", int(exponent))
	n, err := strconv.ParseInt(s, 10, strconv.IntSize)
	if err != nil {
		return invalid()
	}
	*f = flexInt(n)
	return nil
}

// parseUsage extracts the prompt-token count per transport. Pointer
// fields distinguish a missing usage field from a real zero. Anthropic
// totals include cache creation/read tokens when present.
func parseUsage(transport string, raw []byte) (int, bool) {
	switch transport {
	case config.TransportAnthropic:
		var r struct {
			Usage *struct {
				InputTokens         *flexInt `json:"input_tokens"`
				CacheCreationTokens *flexInt `json:"cache_creation_input_tokens"`
				CacheReadTokens     *flexInt `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(raw, &r) == nil && r.Usage != nil && r.Usage.InputTokens != nil {
			total := int(*r.Usage.InputTokens)
			for _, cached := range []*flexInt{r.Usage.CacheCreationTokens, r.Usage.CacheReadTokens} {
				if cached == nil {
					continue
				}
				n := int(*cached)
				if n > int(^uint(0)>>1)-total {
					return 0, false
				}
				total += n
			}
			return total, true
		}
	case config.TransportGemini:
		var r struct {
			UsageMetadata *struct {
				PromptTokenCount *flexInt `json:"promptTokenCount"`
			} `json:"usageMetadata"`
		}
		if json.Unmarshal(raw, &r) == nil && r.UsageMetadata != nil && r.UsageMetadata.PromptTokenCount != nil {
			return int(*r.UsageMetadata.PromptTokenCount), true
		}
	default:
		var r struct {
			Usage *struct {
				PromptTokens *flexInt `json:"prompt_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(raw, &r) == nil && r.Usage != nil && r.Usage.PromptTokens != nil {
			return int(*r.Usage.PromptTokens), true
		}
	}
	return 0, false
}
