package harness

import (
	"encoding/json"
	"fmt"
	"strings"
)

const SignalActivity = "activity"

type NativeHookInput struct {
	Harness       string
	RawJSON       []byte
	ExplicitEvent string
}

type NativeHookSignal struct {
	Signal        string
	Harness       string
	HookEventName string
	Details       string
}

func ParseNativeHook(input NativeHookInput) (NativeHookSignal, error) {
	harnessName := NormalizeName(input.Harness)
	definition, ok := Lookup(harnessName)
	if !ok {
		return NativeHookSignal{}, fmt.Errorf("unsupported native hook harness %q", input.Harness)
	}

	payload := map[string]any{}
	raw := strings.TrimSpace(string(input.RawJSON))
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			if strings.TrimSpace(input.ExplicitEvent) == "" {
				return NativeHookSignal{}, fmt.Errorf("parse native hook payload: %w", err)
			}
		}
	}

	event := firstStringField(payload, "hook_event_name", "hookEventName", "hookName")
	if event == "" {
		event = firstStringField(payload, "type")
	}
	if event == "" {
		event = strings.TrimSpace(input.ExplicitEvent)
	}
	if event == "" {
		return NativeHookSignal{}, fmt.Errorf("native hook event name is required")
	}

	signal := NativeHookSignal{
		Signal:        SignalActivity,
		Harness:       harnessName,
		HookEventName: event,
	}
	notificationType := firstStringField(payload, "notification_type", "notificationType")
	if harnessName == Claude && notificationType != "" {
		signal.Details = "notification_type=" + notificationType
	}
	if definition.HookState != nil {
		if mapped := definition.HookState(event, notificationType); mapped != "" {
			signal.Signal = mapped
		}
	}

	return signal, nil
}

// The map*NativeHook functions return "" for events they do not explicitly
// recognize; ParseNativeHook applies SignalActivity as the default. This lets
// the HookEvents/HookState parity test distinguish an explicit activity
// classification from an unrecognized event.

func mapCodexNativeHook(event string) string {
	switch strings.ToLower(strings.TrimSpace(event)) {
	case "sessionstart", "userpromptsubmit", "pretooluse":
		return StateWorking
	case "permissionrequest", "stop":
		return StateWaiting
	case "posttooluse", "precompact", "postcompact":
		return SignalActivity
	default:
		return ""
	}
}

func mapClaudeNativeHook(event string, notificationType string) string {
	switch strings.ToLower(strings.TrimSpace(event)) {
	case "userpromptsubmit", "pretooluse":
		return StateWorking
	case "permissionrequest", "stop", "stopfailure":
		return StateWaiting
	case "notification":
		switch strings.ToLower(strings.TrimSpace(notificationType)) {
		case "permission_prompt", "idle_prompt":
			return StateWaiting
		default:
			return SignalActivity
		}
	case "posttooluse", "posttoolusefailure":
		return SignalActivity
	default:
		return ""
	}
}

func mapHarnessNativeHook(event string) string {
	switch strings.ToLower(strings.TrimSpace(event)) {
	case "sessionstart", "userpromptsubmit", "pretooluse":
		return StateWorking
	case "stop":
		return StateWaiting
	case "posttooluse", "precompact", "postcompact":
		return SignalActivity
	default:
		return ""
	}
}

func firstStringField(payload map[string]any, names ...string) string {
	for _, name := range names {
		value, ok := payload[name]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			if trimmed := strings.TrimSpace(typed); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}
