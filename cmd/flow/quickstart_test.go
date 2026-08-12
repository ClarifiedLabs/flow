package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunQuickstartHumanAndAgent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(t.TempDir())

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run([]string{"quickstart"}, &stdout, &stderr); code != 0 {
		t.Fatalf("quickstart exitCode = %d, stderr = %q", code, stderr.String())
	}
	for _, want := range []string{"operating contract", "flow complete", "flow events", "flow task respond"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("human quickstart missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--agent", "quickstart"}, &stdout, &stderr); code != 0 {
		t.Fatalf("quickstart --agent exitCode = %d, stderr = %q", code, stderr.String())
	}
	var envelope struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
		Data    struct {
			Contract string `json:"contract"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode quickstart envelope: %v; output = %q", err, stdout.String())
	}
	if envelope.Command != "quickstart" || !envelope.OK {
		t.Fatalf("envelope = %+v, want quickstart/ok", envelope)
	}
	if !strings.Contains(envelope.Data.Contract, "flow complete") || !strings.Contains(envelope.Data.Contract, "never reset") {
		t.Fatalf("agent contract missing core rules:\n%s", envelope.Data.Contract)
	}

	// The alias resolves to the same command.
	stdout.Reset()
	if code := run([]string{"agent-instructions"}, &stdout, &stderr); code != 0 {
		t.Fatalf("agent-instructions exitCode = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "operating contract") {
		t.Fatalf("alias output = %q", stdout.String())
	}
}

func TestWriteAgentsBlockCreates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "AGENTS.md")
	outcome, err := writeAgentsBlock(path)
	if err != nil {
		t.Fatalf("writeAgentsBlock: %v", err)
	}
	if outcome != "created" {
		t.Fatalf("outcome = %q, want created", outcome)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.HasPrefix(string(content), agentsBlockBegin) || !strings.Contains(string(content), agentsBlockEnd) {
		t.Fatalf("created content missing markers:\n%s", content)
	}
}

func TestWriteAgentsBlockRefreshesOnlyTheBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	original := "# My project\n\nHandwritten notes stay.\n\n" + agentsBlockBegin + "\nstale flow content\n" + agentsBlockEnd + "\n\nMore notes below.\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	outcome, err := writeAgentsBlock(path)
	if err != nil {
		t.Fatalf("writeAgentsBlock: %v", err)
	}
	if outcome != "refreshed" {
		t.Fatalf("outcome = %q, want refreshed", outcome)
	}
	content, _ := os.ReadFile(path)
	got := string(content)
	if strings.Contains(got, "stale flow content") {
		t.Fatalf("refresh kept stale block content:\n%s", got)
	}
	for _, keep := range []string{"# My project", "Handwritten notes stay.", "More notes below."} {
		if !strings.Contains(got, keep) {
			t.Fatalf("refresh clobbered %q:\n%s", keep, got)
		}
	}
	if strings.Count(got, agentsBlockBegin) != 1 {
		t.Fatalf("refresh left %d begin markers, want 1", strings.Count(got, agentsBlockBegin))
	}
}

func TestWriteAgentsBlockAppendsToExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(path, []byte("# Existing instructions\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	outcome, err := writeAgentsBlock(path)
	if err != nil {
		t.Fatalf("writeAgentsBlock: %v", err)
	}
	if outcome != "appended" {
		t.Fatalf("outcome = %q, want appended", outcome)
	}
	content, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(content), "# Existing instructions\n") || !strings.Contains(string(content), agentsBlockBegin) {
		t.Fatalf("appended content wrong:\n%s", content)
	}
}

func TestWriteAgentsBlockForeignBlockWritesSidecar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	foreign := "# Project\n\n<!-- kata:begin -->\nkata owns this block\n<!-- kata:end -->\n"
	if err := os.WriteFile(path, []byte(foreign), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	outcome, err := writeAgentsBlock(path)
	if err != nil {
		t.Fatalf("writeAgentsBlock: %v", err)
	}
	if outcome != "proposed" {
		t.Fatalf("outcome = %q, want proposed", outcome)
	}
	content, _ := os.ReadFile(path)
	if string(content) != foreign {
		t.Fatalf("foreign file modified:\n%s", content)
	}
	sidecar, err := os.ReadFile(path + ".flow-proposed")
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if !strings.Contains(string(sidecar), agentsBlockBegin) {
		t.Fatalf("sidecar missing flow block:\n%s", sidecar)
	}
}

func TestWriteAgentsBlockRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.md")
	if err := os.WriteFile(target, []byte("real\n"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	link := filepath.Join(dir, "AGENTS.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	outcome, err := writeAgentsBlock(link)
	if err == nil {
		t.Fatalf("symlink outcome = %q, want refusal error", outcome)
	}
	if outcome != "skipped" {
		t.Fatalf("outcome = %q, want skipped", outcome)
	}
	content, _ := os.ReadFile(target)
	if string(content) != "real\n" {
		t.Fatalf("symlink target modified:\n%s", content)
	}
}

func TestInstallAgentsBlocksTargets(t *testing.T) {
	repo := t.TempDir()
	var stdout bytes.Buffer

	// No CLAUDE.md: only AGENTS.md is written.
	installAgentsBlocks(repo, &stdout)
	if _, err := os.Stat(filepath.Join(repo, "AGENTS.md")); err != nil {
		t.Fatalf("AGENTS.md not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatalf("CLAUDE.md should not be created, stat err = %v", err)
	}

	// Existing CLAUDE.md gets the block too on rerun.
	if err := os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("# Claude notes\n"), 0o644); err != nil {
		t.Fatalf("seed CLAUDE.md: %v", err)
	}
	stdout.Reset()
	installAgentsBlocks(repo, &stdout)
	claude, _ := os.ReadFile(filepath.Join(repo, "CLAUDE.md"))
	if !strings.Contains(string(claude), agentsBlockBegin) {
		t.Fatalf("CLAUDE.md missing flow block:\n%s", claude)
	}
	if !strings.Contains(stdout.String(), "AGENTS.md refreshed") {
		t.Fatalf("rerun output = %q, want AGENTS.md refreshed", stdout.String())
	}
}
