package harness

import "testing"

func TestParseNativeHookSignalMappings(t *testing.T) {
	tests := []struct {
		name    string
		harness string
		payload string
		want    string
	}{
		{name: "codex stop", harness: Codex, payload: `{"hook_event_name":"Stop"}`, want: StateWaiting},
		{name: "codex permission", harness: Codex, payload: `{"hook_event_name":"PermissionRequest"}`, want: StateWaiting},
		{name: "codex prompt", harness: Codex, payload: `{"hook_event_name":"UserPromptSubmit"}`, want: StateWorking},
		{name: "codex post tool", harness: Codex, payload: `{"hook_event_name":"PostToolUse"}`, want: SignalActivity},
		{name: "codex session start", harness: Codex, payload: `{"hook_event_name":"SessionStart"}`, want: StateWorking},
		{name: "codex pre compact", harness: Codex, payload: `{"hook_event_name":"PreCompact"}`, want: SignalActivity},
		{name: "codex post compact", harness: Codex, payload: `{"hook_event_name":"PostCompact"}`, want: SignalActivity},
		{name: "claude idle notification", harness: Claude, payload: `{"hook_event_name":"Notification","notification_type":"idle_prompt"}`, want: StateWaiting},
		{name: "claude permission notification", harness: Claude, payload: `{"hook_event_name":"Notification","notification_type":"permission_prompt"}`, want: StateWaiting},
		{name: "claude auth notification", harness: Claude, payload: `{"hook_event_name":"Notification","notification_type":"auth_success"}`, want: SignalActivity},
		{name: "claude prompt", harness: Claude, payload: `{"hook_event_name":"UserPromptSubmit"}`, want: StateWorking},
		{name: "harness session start", harness: Harness, payload: `{"hook_event_name":"SessionStart"}`, want: StateWorking},
		{name: "harness prompt", harness: Harness, payload: `{"hook_event_name":"UserPromptSubmit"}`, want: StateWorking},
		{name: "harness stop", harness: Harness, payload: `{"hook_event_name":"Stop"}`, want: StateWaiting},
		{name: "harness post tool", harness: Harness, payload: `{"hook_event_name":"PostToolUse"}`, want: SignalActivity},
		{name: "unknown event", harness: Codex, payload: `{"hook_event_name":"UnknownEvent"}`, want: SignalActivity},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signal, err := ParseNativeHook(NativeHookInput{
				Harness: tt.harness,
				RawJSON: []byte(tt.payload),
			})
			if err != nil {
				t.Fatalf("ParseNativeHook err = %v", err)
			}
			if signal.Signal != tt.want {
				t.Fatalf("Signal = %q, want %q", signal.Signal, tt.want)
			}
			if signal.Harness != tt.harness {
				t.Fatalf("Harness = %q, want %q", signal.Harness, tt.harness)
			}
			if signal.HookEventName == "" {
				t.Fatalf("HookEventName = empty")
			}
		})
	}
}

func TestParseNativeHookEventNameFallbacks(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{name: "camel", payload: `{"hookEventName":"Stop"}`, want: "Stop"},
		{name: "hook name", payload: `{"hookName":"PreToolUse"}`, want: "PreToolUse"},
		{name: "type fallback", payload: `{"type":"PostToolUse"}`, want: "PostToolUse"},
		{name: "explicit event", payload: `{`, want: "Stop"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := NativeHookInput{
				Harness:       Codex,
				RawJSON:       []byte(tt.payload),
				ExplicitEvent: tt.want,
			}
			if tt.name != "explicit event" {
				input.ExplicitEvent = ""
			}
			signal, err := ParseNativeHook(input)
			if err != nil {
				t.Fatalf("ParseNativeHook err = %v", err)
			}
			if signal.HookEventName != tt.want {
				t.Fatalf("HookEventName = %q, want %q", signal.HookEventName, tt.want)
			}
		})
	}
}

func TestParseNativeHookRejectsInvalidPayloadWithoutFallback(t *testing.T) {
	if _, err := ParseNativeHook(NativeHookInput{Harness: Codex, RawJSON: []byte(`{`)}); err == nil {
		t.Fatal("ParseNativeHook invalid JSON err = nil, want error")
	}
}
