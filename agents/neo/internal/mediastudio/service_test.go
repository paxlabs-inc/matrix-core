package mediastudio

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"centra/agents/neo/internal/tools"
)

func TestJobJSONAlwaysUsesAssetArray(t *testing.T) {
	job := cloneJob(Job{ID: "legacy-null-assets"})
	if job.Assets == nil {
		t.Fatal("clone retained nil assets")
	}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal job: %v", err)
	}
	if !strings.Contains(string(data), `"assets":[]`) {
		t.Fatalf("job assets were not encoded as an array: %s", data)
	}
}

func TestOpenRecoversInterruptedJobsWithoutRepeatingThem(t *testing.T) {
	mediaDir := filepath.Join(t.TempDir(), "media")
	service, err := Open(context.Background(), Config{MediaDir: mediaDir})
	if err != nil {
		t.Fatalf("open service: %v", err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	job := Job{
		ID: "job-interrupted", Kind: KindTextToVideo, Status: StatusRunning,
		Provider: "Centra Media", Progress: 5,
		Request: Request{Kind: KindTextToVideo, Prompt: "A slow camera move", Seed: -1},
		Assets:  []Asset{}, CreatedAt: now, UpdatedAt: now,
		IdempotencyKey: "idem-interrupted",
	}
	service.mu.Lock()
	service.jobs[job.ID] = job
	service.idempotent[job.IdempotencyKey] = job.ID
	if err := service.persistLocked(); err != nil {
		service.mu.Unlock()
		t.Fatalf("persist running job: %v", err)
	}
	service.mu.Unlock()
	service.Close()

	reopened, err := Open(context.Background(), Config{MediaDir: mediaDir})
	if err != nil {
		t.Fatalf("reopen service: %v", err)
	}
	defer reopened.Close()
	recovered, ok := reopened.Get(job.ID)
	if !ok {
		t.Fatal("interrupted job missing after restart")
	}
	if recovered.Status != StatusFailed || recovered.CompletedAt == nil {
		t.Fatalf("recovered status = %s completed=%v", recovered.Status, recovered.CompletedAt)
	}
	if !strings.Contains(recovered.Error, "paid request was not repeated") {
		t.Fatalf("recovery error = %q", recovered.Error)
	}
}

func TestNormalizeUsesOwnedMediaReferencesAndDeleteRemovesOnlyOutput(t *testing.T) {
	mediaDir := filepath.Join(t.TempDir(), "media")
	service, err := Open(context.Background(), Config{MediaDir: mediaDir})
	if err != nil {
		t.Fatalf("open service: %v", err)
	}
	defer service.Close()
	source := filepath.Join(mediaDir, "source.png")
	if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	request, err := service.normalize(Request{
		Kind: KindImageToImage, Prompt: "Editorial restyle", Image: "/media/source.png",
	})
	if err != nil {
		t.Fatalf("normalize request: %v", err)
	}
	if request.Strength != 0.7 || request.AspectRatio != "1:1" || request.Seed != -1 {
		t.Fatalf("defaults = %+v", request)
	}
	if _, err := service.normalize(Request{
		Kind: KindImageToImage, Prompt: "Escape", Image: "/media/../secret.png",
	}); err == nil {
		t.Fatal("path traversal media reference accepted")
	}

	output := filepath.Join(mediaDir, "generated.png")
	if err := os.WriteFile(output, []byte("generated"), 0o600); err != nil {
		t.Fatalf("write output: %v", err)
	}
	now := time.Now().UTC()
	job := Job{
		ID: "job-delete", Kind: KindImageToImage, Status: StatusSucceeded,
		Provider: "Centra Media", Progress: 100, Request: request,
		Assets: []Asset{{
			ID: "asset-delete", MediaType: "image", MIMEType: "image/png",
			Name: "generated.png", Size: 9, URL: "/media/generated.png",
		}},
		CreatedAt: now, UpdatedAt: now, CompletedAt: &now,
		IdempotencyKey: "idem-delete",
	}
	service.mu.Lock()
	service.jobs[job.ID] = job
	service.idempotent[job.IdempotencyKey] = job.ID
	if err := service.persistLocked(); err != nil {
		service.mu.Unlock()
		t.Fatalf("persist completed job: %v", err)
	}
	service.mu.Unlock()
	if err := service.Delete(job.ID); err != nil {
		t.Fatalf("delete job: %v", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("generated output still exists: %v", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source upload was removed: %v", err)
	}
}

func TestToolCallUsesBoundMediaFunctionNames(t *testing.T) {
	tests := []struct {
		kind Kind
		want string
	}{
		{KindTextToImage, "media__generate_image"},
		{KindImageToImage, "media__edit_image"},
		{KindTextToVideo, "media__generate_video"},
		{KindImageToVideo, "media__animate_image"},
		{KindInpainting, "media__inpaint_image"},
		{KindCleanup, "media__cleanup_image"},
		{KindRemoveBackground, "media__remove_background"},
		{KindReplaceBackground, "media__replace_background"},
		{KindRemoveText, "media__remove_text"},
		{KindMergeFace, "media__merge_faces"},
		{KindUpscale, "media__upscale_image"},
	}
	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			got, _ := toolCall(Request{Kind: tc.kind})
			if got != tc.want {
				t.Fatalf("tool = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDecodeMediaResultPreservesTypedFailuresAndSuccess(t *testing.T) {
	success, err := decodeMediaResult(tools.DirectResult{
		Content: `{"ok":true,"kind":"image","url":"/media/generated.png","mime":"image/png","bytes":42}`,
	})
	if err != nil || success.URL != "/media/generated.png" || success.Bytes != 42 {
		t.Fatalf("success decode = %+v err=%v", success, err)
	}

	_, err = decodeMediaResult(tools.DirectResult{
		Content:        `unknown tool "generate_image"`,
		IsError:        true,
		FailureMessage: "The media operation is unavailable.",
	})
	if err == nil || err.Error() != "The media operation is unavailable." {
		t.Fatalf("typed plain-text failure lost: %v", err)
	}

	_, err = decodeMediaResult(tools.DirectResult{
		Content: `{"ok":false,"error":"The provider rejected this prompt."}`,
		IsError: true,
	})
	if err == nil || err.Error() != "The provider rejected this prompt." {
		t.Fatalf("structured provider failure lost: %v", err)
	}
}
