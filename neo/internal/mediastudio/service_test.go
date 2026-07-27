package mediastudio

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenRecoversInterruptedJobsWithoutRepeatingThem(t *testing.T) {
	mediaDir := filepath.Join(t.TempDir(), "media")
	service, err := Open(context.Background(), Config{MediaDir: mediaDir})
	if err != nil {
		t.Fatalf("open service: %v", err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	job := Job{
		ID: "job-interrupted", Kind: KindTextToVideo, Status: StatusRunning,
		Provider: "matrix", Progress: 5,
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
		Provider: "matrix", Progress: 100, Request: request,
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
