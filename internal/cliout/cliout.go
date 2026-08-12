// Package cliout defines the versioned machine-readable output contract for
// the flow CLI. Human output is free to evolve; the contract governed by
// ContractVersion is what agents and scripts may rely on.
//
// Two machine modes exist:
//
//   - --json prints the command's primary result as bare JSON on stdout.
//   - --agent wraps the result in a versioned Envelope on stdout, including
//     structured errors, so an agent can parse exactly one stream.
//
// In both modes errors are reported on stdout (human-readable context may
// still go to stderr) and signaled through exit codes: 0 success, 1 command
// error, 2 usage error.
package cliout

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	flowclient "github.com/ClarifiedLabs/flow/internal/client"
)

// ContractVersion is bumped only on breaking changes to the machine output
// schema (envelope fields, error shape, or a command's data payload).
const ContractVersion = 1

// Mode selects how a command renders its result.
type Mode int

const (
	// ModeHuman is the default human-oriented rendering.
	ModeHuman Mode = iota
	// ModeJSON prints bare JSON results.
	ModeJSON
	// ModeAgent prints versioned envelopes with structured errors.
	ModeAgent
)

// FromFlags resolves the output mode from the --json/--agent flags. --agent
// implies --json semantics and wins when both are set.
func FromFlags(jsonOut, agentOut bool) Mode {
	switch {
	case agentOut:
		return ModeAgent
	case jsonOut:
		return ModeJSON
	default:
		return ModeHuman
	}
}

// Machine reports whether the mode emits machine-readable output.
func (m Mode) Machine() bool {
	return m != ModeHuman
}

// ErrorBody is the structured error carried by machine output.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Envelope is the --agent wrapper: a versioned, self-describing result.
type Envelope struct {
	ContractVersion int        `json:"contract_version"`
	Command         string     `json:"command"`
	OK              bool       `json:"ok"`
	Data            any        `json:"data,omitempty"`
	Error           *ErrorBody `json:"error,omitempty"`
}

// WriteData renders a successful result and returns the exit code (0).
// JSON mode writes bare data; agent mode wraps it in an Envelope.
func WriteData(w io.Writer, mode Mode, command string, data any) int {
	if mode == ModeAgent {
		return writeJSON(w, Envelope{
			ContractVersion: ContractVersion,
			Command:         command,
			OK:              true,
			Data:            data,
		})
	}
	return writeJSON(w, data)
}

// WriteError renders a command error and returns the exit code (1). The
// machine-readable error goes to w (stdout); callers may additionally log
// context to stderr.
func WriteError(w io.Writer, mode Mode, command string, err error) int {
	return writeFailure(w, mode, command, ErrorCode(err), ErrorMessage(err), 1)
}

// WriteUsageError renders a usage error and returns the exit code (2).
func WriteUsageError(w io.Writer, mode Mode, command string, message string) int {
	return writeFailure(w, mode, command, "usage_error", message, 2)
}

// WriteFailure renders a classified failure with a caller-chosen exit code
// (for commands with domain exit codes, e.g. flow wait's 3 = timeout).
func WriteFailure(w io.Writer, mode Mode, command, code, message string, exitCode int) int {
	return writeFailure(w, mode, command, code, message, exitCode)
}

func writeFailure(w io.Writer, mode Mode, command, code, message string, exitCode int) int {
	if mode == ModeAgent {
		writeJSON(w, Envelope{
			ContractVersion: ContractVersion,
			Command:         command,
			OK:              false,
			Error:           &ErrorBody{Code: code, Message: message},
		})
		return exitCode
	}
	writeJSON(w, map[string]any{"error": ErrorBody{Code: code, Message: message}})
	return exitCode
}

// ErrorCode extracts a stable machine code from an error. API failures carry
// the server-provided code; everything else is classified generically.
func ErrorCode(err error) (code string) {
	var statusErr *flowclient.HTTPStatusError
	if errors.As(err, &statusErr) {
		if code = strings.TrimSpace(statusErr.Code); code != "" {
			return code
		}
		return fmt.Sprintf("http_%d", statusErr.StatusCode)
	}
	return "command_failed"
}

// ErrorMessage extracts the human message from an error, preferring the
// server-provided message for API failures (their Error() prefixes the code).
func ErrorMessage(err error) string {
	var statusErr *flowclient.HTTPStatusError
	if errors.As(err, &statusErr) {
		if message := strings.TrimSpace(statusErr.Message); message != "" {
			return message
		}
	}
	return err.Error()
}

func writeJSON(w io.Writer, v any) int {
	encoded, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(w, `{"error":{"code":"encode_failed","message":%q}}`+"\n", err.Error())
		return 1
	}
	fmt.Fprintln(w, string(encoded))
	return 0
}
