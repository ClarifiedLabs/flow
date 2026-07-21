package coordinator

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsImageContentType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		contentType string
		want        bool
	}{
		{"png", "image/png", true},
		{"jpeg", "image/jpeg", true},
		{"gif", "image/gif", true},
		{"webp", "image/webp", true},
		{"avif", "image/avif", true},
		{"bmp", "image/bmp", true},
		{"svg excluded", "image/svg+xml", false},
		{"text", "text/plain", false},
		{"octet stream", "application/octet-stream", false},
		{"empty", "", false},
		{"with params", "image/png; charset=utf-8", true},
		{"upper case", "IMAGE/PNG", true},
		{"surrounding space", "  image/png  ", true},
		{"malformed", "not a media type", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsImageContentType(tc.contentType); got != tc.want {
				t.Fatalf("IsImageContentType(%q) = %v, want %v", tc.contentType, got, tc.want)
			}
		})
	}
}

func TestStampImageAttachmentsFiltersToImagesAndStampsDescriptors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	_, issues := newIssueService(t, filepath.Join(dir, "flow.db"))
	store := NewIssueAttachmentStore(filepath.Join(dir, "attachments"))

	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "Image issue"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	// An image attachment (png) and a non-image attachment (text) and an svg
	// (image/* but excluded).
	if _, err := issues.CreateIssueAttachment(ctx, CreateIssueAttachmentInput{
		IssueID: issue.ID, Stage: IssueAttachmentStageInitial,
		Filename: "shot.png", ContentType: "image/png",
		CreatedBy: ActorHuman, Reader: strings.NewReader("png-bytes"),
	}, store); err != nil {
		t.Fatalf("create png attachment: %v", err)
	}
	if _, err := issues.CreateIssueAttachment(ctx, CreateIssueAttachmentInput{
		IssueID: issue.ID, Stage: IssueAttachmentStageInitial,
		Filename: "notes.txt", ContentType: "text/plain",
		CreatedBy: ActorHuman, Reader: strings.NewReader("notes"),
	}, store); err != nil {
		t.Fatalf("create txt attachment: %v", err)
	}
	if _, err := issues.CreateIssueAttachment(ctx, CreateIssueAttachmentInput{
		IssueID: issue.ID, Stage: IssueAttachmentStageInitial,
		Filename: "diagram.svg", ContentType: "image/svg+xml",
		CreatedBy: ActorHuman, Reader: strings.NewReader("<svg/>"),
	}, store); err != nil {
		t.Fatalf("create svg attachment: %v", err)
	}

	payload := map[string]any{}
	if err := stampImageAttachments(ctx, issues, payload, issue.ID); err != nil {
		t.Fatalf("stamp image attachments: %v", err)
	}
	descriptors, ok := payload["image_attachments"].([]IssueImageAttachment)
	if !ok {
		t.Fatalf("image_attachments = %#v, want []IssueImageAttachment", payload["image_attachments"])
	}
	if len(descriptors) != 1 {
		t.Fatalf("image attachments = %+v, want 1 (png only)", descriptors)
	}
	if descriptors[0].Filename != "shot.png" || descriptors[0].ID == "" {
		t.Fatalf("descriptor = %+v, want id + shot.png", descriptors[0])
	}
}

func TestStampImageAttachmentsStampsEmptyListForIssueWithoutImages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	_, issues := newIssueService(t, filepath.Join(dir, "flow.db"))
	issue, err := issues.CreateIssue(ctx, CreateIssueInput{Title: "No images"})
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	payload := map[string]any{}
	if err := stampImageAttachments(ctx, issues, payload, issue.ID); err != nil {
		t.Fatalf("stamp image attachments: %v", err)
	}
	descriptors, ok := payload["image_attachments"].([]IssueImageAttachment)
	if !ok {
		t.Fatalf("image_attachments = %#v, want typed empty slice", payload["image_attachments"])
	}
	if len(descriptors) != 0 {
		t.Fatalf("image attachments = %+v, want empty", descriptors)
	}
}
