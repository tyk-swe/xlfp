package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// debug config resolves the same managed and organization settings as run,
// without submitting a prompt. Pure mode disables plugin hooks during both.
func (c *Client) checkConfiguration(ctx context.Context) error {
	raw, err := c.run(ctx, []string{"debug", "config"}, "", 4<<20, false)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("checking isolated OpenCode configuration (requires `opencode debug config`): %w", err)
	}
	return validateConfiguration(raw, c.config)
}

func validateConfiguration(raw, expected []byte) error {
	// Never include resolved values or raw diagnostics: provider configuration
	// can contain credentials even when the debug command redacts known keys.
	invalid := errors.New("OpenCode effective configuration differs from the isolated probe configuration; refusing to probe (check managed or organization settings)")
	var got, want map[string]any
	if json.Unmarshal(raw, &got) != nil || json.Unmarshal(expected, &want) != nil || got == nil || want == nil {
		return invalid
	}
	if username, ok := got["username"]; ok {
		if _, ok := username.(string); !ok {
			return invalid
		}
		delete(got, "username")
	}
	// OpenCode initializes these collections even with no user configuration.
	// Only empty collections are compatible with an isolated probe.
	for _, key := range []string{"mode", "command", "mcp", "provider"} {
		if value, exists := got[key]; exists {
			if object, ok := value.(map[string]any); !ok || len(object) != 0 {
				return invalid
			}
			delete(got, key)
		}
	}
	for _, key := range []string{"plugin", "plugin_origins", "instructions"} {
		if value, exists := got[key]; exists {
			if list, ok := value.([]any); !ok || len(list) != 0 {
				return invalid
			}
			delete(got, key)
		}
	}
	// Compare the entire remainder, including nested permissions/options and
	// unknown fields, rather than trusting just the agent name or deny wildcard.
	if !reflect.DeepEqual(got, want) {
		return invalid
	}
	return nil
}
