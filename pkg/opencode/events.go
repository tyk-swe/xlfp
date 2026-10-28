package opencode

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
)

type stepPart struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	MessageID string `json:"messageID"`
	Type      string `json:"type"`
	Reason    string `json:"reason"`
	Tokens    *struct {
		Input *json.Number `json:"input"`
		Cache *struct {
			Read  *json.Number `json:"read"`
			Write *json.Number `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
}

func parseEvents(raw []byte) (int, error) {
	var session, message, startID string
	var finish *stepPart
	total := 0
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event struct {
			Type      string    `json:"type"`
			SessionID string    `json:"sessionID"`
			Part      *stepPart `json:"part"`
		}
		if err := json.Unmarshal(line, &event); err != nil || event.Type == "" {
			return 0, errors.New("invalid OpenCode JSON events; v1.19.4-compatible --format json output is required")
		}
		if event.Type == "error" {
			return 0, errors.New("OpenCode reported an error; check your OpenCode login and model availability")
		}
		if event.Type == "tool_use" {
			return 0, errors.New("OpenCode used a tool; session context is not suitable for fingerprinting")
		}
		if event.SessionID != "" {
			if session != "" && session != event.SessionID {
				return 0, errors.New("OpenCode emitted multiple sessions; no token evidence accepted")
			}
			session = event.SessionID
		}
		if event.Type != "step_start" && event.Type != "step_finish" {
			continue
		}
		p := event.Part
		if p == nil || p.ID == "" || p.MessageID == "" || event.SessionID == "" || p.SessionID != event.SessionID {
			return 0, errors.New("OpenCode step is missing its session or message identity")
		}
		if message != "" && message != p.MessageID {
			return 0, errors.New("OpenCode emitted multiple messages; no token evidence accepted")
		}
		message = p.MessageID
		if event.Type == "step_start" {
			if p.Type != "step-start" || (startID != "" && startID != p.ID) || (finish != nil && startID != p.ID) {
				return 0, errors.New("OpenCode emitted multiple or invalid steps; no token evidence accepted")
			}
			startID = p.ID
			continue
		}
		if p.Type != "step-finish" {
			return 0, errors.New("invalid OpenCode completed step")
		}
		if p.Reason == "tool-calls" {
			return 0, errors.New("OpenCode requested a tool; session context is not suitable for fingerprinting")
		}
		count, err := promptTokens(p)
		if err != nil {
			return 0, err
		}
		if finish != nil && (finish.ID != p.ID || total != count) {
			return 0, errors.New("OpenCode emitted multiple completed steps; no token evidence accepted")
		}
		finish, total = p, count
	}
	if finish == nil {
		return 0, errors.New("OpenCode returned no completed step with prompt-token usage; check v1 JSON support")
	}
	return total, nil
}

func promptTokens(p *stepPart) (int, error) {
	invalid := errors.New("OpenCode returned missing, invalid, or overflowing prompt-token usage")
	if p.Tokens == nil || p.Tokens.Input == nil {
		return 0, invalid
	}
	fields := []*json.Number{p.Tokens.Input}
	if p.Tokens.Cache != nil {
		fields = append(fields, p.Tokens.Cache.Read, p.Tokens.Cache.Write)
	}
	total, max := 0, int(^uint(0)>>1)
	for _, field := range fields {
		if field == nil {
			continue
		}
		n, err := strconv.ParseInt(field.String(), 10, strconv.IntSize)
		if err != nil || n < 0 || int(n) > max-total {
			return 0, invalid
		}
		total += int(n)
	}
	if total <= 0 {
		return 0, invalid
	}
	return total, nil
}
