package mediastudio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"matrix/neo/internal/tools"
	"matrix/vault"
)

type Kind string

const (
	KindTextToImage       Kind = "text-to-image"
	KindImageToImage      Kind = "image-to-image"
	KindTextToVideo       Kind = "text-to-video"
	KindImageToVideo      Kind = "image-to-video"
	KindInpainting        Kind = "inpainting"
	KindCleanup           Kind = "cleanup"
	KindRemoveBackground  Kind = "remove-background"
	KindReplaceBackground Kind = "replace-background"
	KindRemoveText        Kind = "remove-text"
	KindMergeFace         Kind = "merge-face"
	KindUpscale           Kind = "upscale"
)

type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
)

type Request struct {
	Kind           Kind    `json:"kind"`
	Prompt         string  `json:"prompt,omitempty"`
	NegativePrompt string  `json:"negative_prompt,omitempty"`
	AspectRatio    string  `json:"aspect_ratio,omitempty"`
	Seed           int64   `json:"seed"`
	Strength       float64 `json:"strength,omitempty"`
	Frames         int     `json:"frames,omitempty"`
	FPS            int     `json:"fps,omitempty"`
	Scale          float64 `json:"scale,omitempty"`
	Image          string  `json:"image,omitempty"`
	Mask           string  `json:"mask,omitempty"`
	Face           string  `json:"face,omitempty"`
}

type Asset struct {
	ID        string `json:"id"`
	MediaType string `json:"media_type"`
	MIMEType  string `json:"mime_type"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	URL       string `json:"url"`
}

type Job struct {
	ID             string     `json:"id"`
	Kind           Kind       `json:"kind"`
	Status         Status     `json:"status"`
	Provider       string     `json:"provider"`
	Progress       int        `json:"progress"`
	Error          string     `json:"error,omitempty"`
	Request        Request    `json:"request"`
	Assets         []Asset    `json:"assets"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	IdempotencyKey string     `json:"-"`
}

type StatusView struct {
	Configured bool   `json:"configured"`
	Provider   string `json:"provider"`
	Message    string `json:"message"`
}

type persistedState struct {
	Jobs []persistedJob `json:"jobs"`
}

type persistedJob struct {
	Job
	IdempotencyKey string `json:"idempotency_key"`
}

type Config struct {
	MediaDir  string
	APIKey    string
	Tools     *tools.Manager
	Vault     *vault.Session
	VaultUser string
}

type Service struct {
	mu         sync.RWMutex
	jobs       map[string]Job
	idempotent map[string]string
	mediaDir   string
	root       string
	statePath  string
	configured bool
	tools      *tools.Manager
	vault      *vault.Session
	vaultUser  string
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}

const (
	maxActiveJobs = 4
	storeName     = "neo.media-studio"
	schemaName    = "media-studio.v1"
)

func Open(parent context.Context, config Config) (*Service, error) {
	mediaDir := filepath.Clean(strings.TrimSpace(config.MediaDir))
	if mediaDir == "." || mediaDir == "" {
		return nil, fmt.Errorf("media studio: media directory is required")
	}
	if err := os.MkdirAll(mediaDir, 0o700); err != nil {
		return nil, fmt.Errorf("media studio: create media directory: %w", err)
	}
	root := filepath.Join(filepath.Dir(mediaDir), "media-studio")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("media studio: create state directory: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	service := &Service{
		jobs: make(map[string]Job), idempotent: make(map[string]string),
		mediaDir: mediaDir, root: root, statePath: filepath.Join(root, "jobs.json"),
		configured: strings.TrimSpace(config.APIKey) != "" && config.Tools != nil,
		tools:      config.Tools, vault: config.Vault, vaultUser: config.VaultUser,
		ctx: ctx, cancel: cancel,
	}
	if err := service.load(); err != nil {
		cancel()
		return nil, err
	}
	now := time.Now().UTC()
	changed := false
	for id, job := range service.jobs {
		if job.Status != StatusQueued && job.Status != StatusRunning {
			continue
		}
		job.Status = StatusFailed
		job.Progress = 0
		job.Error = "Generation was interrupted before its provider result was recorded. The paid request was not repeated."
		job.UpdatedAt = now
		job.CompletedAt = &now
		service.jobs[id] = job
		changed = true
	}
	if changed {
		if err := service.persistLocked(); err != nil {
			cancel()
			return nil, err
		}
	}
	return service, nil
}

func (service *Service) Status() StatusView {
	if service != nil && service.configured {
		return StatusView{
			Configured: true, Provider: "Matrix Media",
			Message: "Image and video generation is ready.",
		}
	}
	return StatusView{
		Configured: false, Provider: "Matrix Media",
		Message: "Image and video generation is not configured on this machine.",
	}
}

func (service *Service) List() []Job {
	service.mu.RLock()
	defer service.mu.RUnlock()
	jobs := make([]Job, 0, len(service.jobs))
	for _, job := range service.jobs {
		jobs = append(jobs, cloneJob(job))
	}
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].UpdatedAt.After(jobs[j].UpdatedAt)
	})
	if len(jobs) > 100 {
		jobs = jobs[:100]
	}
	return jobs
}

func (service *Service) Get(id string) (Job, bool) {
	service.mu.RLock()
	defer service.mu.RUnlock()
	job, ok := service.jobs[id]
	return cloneJob(job), ok
}

func (service *Service) Create(idempotencyKey string, input Request) (Job, error) {
	if service == nil || !service.configured {
		return Job{}, errors.New("image and video generation is not configured")
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 200 {
		return Job{}, fmt.Errorf("a valid idempotency key is required")
	}
	request, err := service.normalize(input)
	if err != nil {
		return Job{}, err
	}
	service.mu.Lock()
	if id, ok := service.idempotent[idempotencyKey]; ok {
		job, exists := service.jobs[id]
		service.mu.Unlock()
		if exists {
			return cloneJob(job), nil
		}
		return Job{}, fmt.Errorf("media studio: idempotency record is unavailable")
	}
	active := 0
	for _, job := range service.jobs {
		if job.Status == StatusQueued || job.Status == StatusRunning {
			active++
		}
	}
	if active >= maxActiveJobs {
		service.mu.Unlock()
		return Job{}, fmt.Errorf("wait for an active generation to finish before starting another")
	}
	now := time.Now().UTC()
	job := Job{
		ID: uuid.NewString(), Kind: request.Kind, Status: StatusQueued,
		Provider: "matrix", Request: request, Assets: []Asset{},
		CreatedAt: now, UpdatedAt: now, IdempotencyKey: idempotencyKey,
	}
	service.jobs[job.ID] = job
	service.idempotent[idempotencyKey] = job.ID
	if err := service.persistLocked(); err != nil {
		delete(service.jobs, job.ID)
		delete(service.idempotent, idempotencyKey)
		service.mu.Unlock()
		return Job{}, err
	}
	service.mu.Unlock()
	service.start(job.ID)
	return cloneJob(job), nil
}

func (service *Service) Delete(id string) error {
	service.mu.Lock()
	job, ok := service.jobs[id]
	if !ok {
		service.mu.Unlock()
		return os.ErrNotExist
	}
	if job.Status == StatusQueued || job.Status == StatusRunning {
		service.mu.Unlock()
		return fmt.Errorf("wait for the active generation to finish before deleting it")
	}
	delete(service.jobs, id)
	delete(service.idempotent, job.IdempotencyKey)
	if err := service.persistLocked(); err != nil {
		service.jobs[id] = job
		service.idempotent[job.IdempotencyKey] = id
		service.mu.Unlock()
		return err
	}
	service.mu.Unlock()
	for _, asset := range job.Assets {
		if name, ok := mediaName(asset.URL); ok {
			_ = os.Remove(filepath.Join(service.mediaDir, name))
		}
	}
	return nil
}

func (service *Service) Close() {
	if service == nil {
		return
	}
	service.cancel()
	service.wg.Wait()
}

func (service *Service) start(id string) {
	service.wg.Add(1)
	go func() {
		defer service.wg.Done()
		service.run(id)
	}()
}

func (service *Service) run(id string) {
	service.mu.Lock()
	job, ok := service.jobs[id]
	if !ok {
		service.mu.Unlock()
		return
	}
	job.Status = StatusRunning
	job.Progress = 5
	job.UpdatedAt = time.Now().UTC()
	service.jobs[id] = job
	if err := service.persistLocked(); err != nil {
		service.mu.Unlock()
		service.fail(id, err)
		return
	}
	service.mu.Unlock()

	toolName, args := toolCall(job.Request)
	runCtx, cancel := context.WithTimeout(service.ctx, 30*time.Minute)
	result, err := service.tools.CallDirect(runCtx, toolName, args)
	cancel()
	if service.ctx.Err() != nil {
		return
	}
	if err != nil {
		service.fail(id, err)
		return
	}
	payload, decodeErr := decodeMediaResult(result)
	if decodeErr != nil {
		service.fail(id, decodeErr)
		return
	}
	name, valid := mediaName(payload.URL)
	if !valid {
		service.fail(id, fmt.Errorf("media provider returned an invalid stored output"))
		return
	}
	info, statErr := os.Stat(filepath.Join(service.mediaDir, name))
	if statErr != nil || info.IsDir() {
		service.fail(id, fmt.Errorf("generated media output is unavailable"))
		return
	}
	mimeType := strings.TrimSpace(payload.MIME)
	if mimeType == "" {
		mimeType = mime.TypeByExtension(filepath.Ext(name))
	}
	mediaType := payload.Kind
	if mediaType != "image" && mediaType != "video" {
		mediaType = "image"
		if strings.HasPrefix(mimeType, "video/") {
			mediaType = "video"
		}
	}
	now := time.Now().UTC()
	service.mu.Lock()
	current, exists := service.jobs[id]
	if exists {
		current.Status = StatusSucceeded
		current.Progress = 100
		current.Error = ""
		current.Assets = []Asset{{
			ID: uuid.NewString(), MediaType: mediaType, MIMEType: mimeType,
			Name: name, Size: info.Size(), URL: payload.URL,
		}}
		current.UpdatedAt = now
		current.CompletedAt = &now
		service.jobs[id] = current
		_ = service.persistLocked()
	}
	service.mu.Unlock()
}

func (service *Service) fail(id string, reason error) {
	now := time.Now().UTC()
	service.mu.Lock()
	defer service.mu.Unlock()
	job, ok := service.jobs[id]
	if !ok {
		return
	}
	job.Status = StatusFailed
	job.Error = boundedError(reason)
	job.UpdatedAt = now
	job.CompletedAt = &now
	service.jobs[id] = job
	_ = service.persistLocked()
}

func (service *Service) normalize(input Request) (Request, error) {
	input.Kind = Kind(strings.TrimSpace(string(input.Kind)))
	input.Prompt = strings.TrimSpace(input.Prompt)
	input.NegativePrompt = strings.TrimSpace(input.NegativePrompt)
	input.AspectRatio = strings.TrimSpace(input.AspectRatio)
	input.Image = strings.TrimSpace(input.Image)
	input.Mask = strings.TrimSpace(input.Mask)
	input.Face = strings.TrimSpace(input.Face)
	if !validKind(input.Kind) {
		return Request{}, fmt.Errorf("unsupported media workflow")
	}
	if len(input.Prompt) > 4000 || len(input.NegativePrompt) > 2000 {
		return Request{}, fmt.Errorf("prompt exceeds its length limit")
	}
	if input.AspectRatio == "" {
		if input.Kind == KindTextToVideo {
			input.AspectRatio = "16:9"
		} else {
			input.AspectRatio = "1:1"
		}
	}
	if !validAspect(input.Kind, input.AspectRatio) {
		return Request{}, fmt.Errorf("unsupported aspect ratio for this workflow")
	}
	if input.Seed == 0 {
		input.Seed = -1
	}
	if input.Strength == 0 {
		input.Strength = 0.7
	}
	if input.Frames == 0 {
		if input.Kind == KindImageToVideo {
			input.Frames = 25
		} else {
			input.Frames = 64
		}
	}
	if input.FPS == 0 {
		input.FPS = 6
	}
	if input.Scale == 0 {
		input.Scale = 2
	}
	if input.Seed < -1 || input.Seed > 2147483647 ||
		input.Strength < 0.05 || input.Strength > 1 ||
		input.Frames < 14 || input.Frames > 128 ||
		input.FPS < 1 || input.FPS > 30 ||
		input.Scale < 1.1 || input.Scale > 4 {
		return Request{}, fmt.Errorf("generation controls are out of range")
	}
	if needsPrompt(input.Kind) && input.Prompt == "" {
		return Request{}, fmt.Errorf("a prompt is required")
	}
	if needsImage(input.Kind) {
		if err := service.validateReference(input.Image, "source image"); err != nil {
			return Request{}, err
		}
	}
	if input.Kind == KindInpainting || input.Kind == KindCleanup {
		if err := service.validateReference(input.Mask, "mask image"); err != nil {
			return Request{}, err
		}
	}
	if input.Kind == KindMergeFace {
		if err := service.validateReference(input.Face, "face reference"); err != nil {
			return Request{}, err
		}
	}
	return input, nil
}

func (service *Service) validateReference(reference, label string) error {
	name, ok := mediaName(reference)
	if !ok {
		return fmt.Errorf("a valid uploaded %s is required", label)
	}
	info, err := os.Stat(filepath.Join(service.mediaDir, name))
	if err != nil || info.IsDir() {
		return fmt.Errorf("the uploaded %s is unavailable", label)
	}
	return nil
}

type mediaResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	Kind  string `json:"kind"`
	URL   string `json:"url"`
	MIME  string `json:"mime"`
	Bytes int64  `json:"bytes"`
}

func decodeMediaResult(result tools.DirectResult) (mediaResult, error) {
	var payload mediaResult
	decodeErr := json.Unmarshal([]byte(strings.TrimSpace(result.Content)), &payload)
	if result.IsError {
		message := strings.TrimSpace(payload.Error)
		if message == "" {
			message = strings.TrimSpace(result.FailureMessage)
		}
		if message == "" {
			message = "The media engine could not complete the job"
		}
		return mediaResult{}, errors.New(message)
	}
	if decodeErr != nil {
		return mediaResult{}, fmt.Errorf("media bridge returned an invalid response")
	}
	if !payload.OK {
		message := strings.TrimSpace(payload.Error)
		if message == "" {
			message = "The media engine could not complete the job"
		}
		return mediaResult{}, errors.New(message)
	}
	return payload, nil
}

func toolCall(request Request) (string, map[string]interface{}) {
	args := map[string]interface{}{"provider": "novita", "seed": request.Seed}
	if request.Prompt != "" {
		args["prompt"] = request.Prompt
	}
	if request.NegativePrompt != "" {
		args["negative_prompt"] = request.NegativePrompt
	}
	if request.AspectRatio != "" {
		args["aspect_ratio"] = request.AspectRatio
	}
	switch request.Kind {
	case KindTextToImage:
		return tools.FunctionName("media", "generate_image"), args
	case KindImageToImage:
		args["image"], args["strength"] = request.Image, request.Strength
		return tools.FunctionName("media", "edit_image"), args
	case KindTextToVideo:
		args["frames"] = request.Frames
		return tools.FunctionName("media", "generate_video"), args
	case KindImageToVideo:
		args["image"], args["frames"], args["fps"] = request.Image, request.Frames, request.FPS
		return tools.FunctionName("media", "animate_image"), args
	case KindInpainting:
		args["image"], args["mask"], args["strength"] = request.Image, request.Mask, request.Strength
		return tools.FunctionName("media", "inpaint_image"), args
	case KindCleanup:
		args["image"], args["mask"] = request.Image, request.Mask
		return tools.FunctionName("media", "cleanup_image"), args
	case KindRemoveBackground:
		args["image"] = request.Image
		return tools.FunctionName("media", "remove_background"), args
	case KindReplaceBackground:
		args["image"] = request.Image
		return tools.FunctionName("media", "replace_background"), args
	case KindRemoveText:
		args["image"] = request.Image
		return tools.FunctionName("media", "remove_text"), args
	case KindMergeFace:
		args["image"], args["face"] = request.Image, request.Face
		return tools.FunctionName("media", "merge_faces"), args
	case KindUpscale:
		args["image"], args["scale"] = request.Image, request.Scale
		return tools.FunctionName("media", "upscale_image"), args
	default:
		return "", args
	}
}

func validKind(kind Kind) bool {
	switch kind {
	case KindTextToImage, KindImageToImage, KindTextToVideo, KindImageToVideo,
		KindInpainting, KindCleanup, KindRemoveBackground, KindReplaceBackground,
		KindRemoveText, KindMergeFace, KindUpscale:
		return true
	default:
		return false
	}
}

func needsPrompt(kind Kind) bool {
	switch kind {
	case KindTextToImage, KindImageToImage, KindTextToVideo,
		KindInpainting, KindReplaceBackground:
		return true
	default:
		return false
	}
}

func needsImage(kind Kind) bool {
	return kind != KindTextToImage && kind != KindTextToVideo
}

func validAspect(kind Kind, value string) bool {
	if kind == KindTextToVideo || kind == KindImageToVideo {
		return value == "16:9" || value == "9:16" || value == "1:1"
	}
	return value == "1:1" || value == "16:9" || value == "9:16" ||
		value == "4:3" || value == "3:2"
}

func mediaName(reference string) (string, bool) {
	const prefix = "/media/"
	if !strings.HasPrefix(reference, prefix) {
		return "", false
	}
	name := strings.TrimSpace(strings.TrimPrefix(reference, prefix))
	return name, name != "" && name == filepath.Base(name) &&
		!strings.Contains(name, "..") && !strings.ContainsAny(name, `/\`)
}

func cloneJob(job Job) Job {
	job.Assets = append([]Asset(nil), job.Assets...)
	return job
}

func boundedError(err error) string {
	if err == nil {
		return "The media job failed."
	}
	message := strings.TrimSpace(strings.TrimPrefix(err.Error(), "media studio: "))
	if strings.Contains(strings.ToLower(message), "novita") {
		message = "The media provider could not complete the job."
	}
	if len(message) > 300 {
		message = message[:300]
	}
	if message == "" {
		return "The media job failed."
	}
	return message
}

func (service *Service) ad() vault.AD {
	return vault.AD{
		User: service.vaultUser, Store: storeName,
		Stream: "jobs", Schema: schemaName,
	}
}

func (service *Service) load() error {
	data, err := os.ReadFile(service.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("media studio: read state: %w", err)
	}
	if vault.IsVault(data) {
		if service.vault == nil || service.vault.UserVault() == nil {
			return fmt.Errorf("media studio: encrypted state requires the user vault")
		}
		data, err = service.vault.UserVault().OpenFile(service.ad(), data)
		if err != nil {
			return fmt.Errorf("media studio: open encrypted state: %w", err)
		}
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("media studio: decode state: %w", err)
	}
	for _, stored := range state.Jobs {
		job := stored.Job
		job.IdempotencyKey = stored.IdempotencyKey
		if job.ID == "" || job.IdempotencyKey == "" {
			return fmt.Errorf("media studio: stored job is incomplete")
		}
		service.jobs[job.ID] = job
		service.idempotent[job.IdempotencyKey] = job.ID
	}
	return nil
}

func (service *Service) persistLocked() error {
	state := persistedState{Jobs: make([]persistedJob, 0, len(service.jobs))}
	for _, job := range service.jobs {
		state.Jobs = append(state.Jobs, persistedJob{
			Job: job, IdempotencyKey: job.IdempotencyKey,
		})
	}
	sort.Slice(state.Jobs, func(i, j int) bool {
		return state.Jobs[i].CreatedAt.Before(state.Jobs[j].CreatedAt)
	})
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("media studio: encode state: %w", err)
	}
	if service.vault != nil {
		data, err = service.vault.MaybeSealFile(service.ad(), data)
		if err != nil {
			return fmt.Errorf("media studio: seal state: %w", err)
		}
	}
	temp, err := os.CreateTemp(service.root, ".jobs-*.tmp")
	if err != nil {
		return fmt.Errorf("media studio: create state file: %w", err)
	}
	tempName := temp.Name()
	remove := true
	defer func() {
		_ = temp.Close()
		if remove {
			_ = os.Remove(tempName)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("media studio: secure state file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		return fmt.Errorf("media studio: write state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("media studio: sync state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("media studio: close state: %w", err)
	}
	if err := os.Rename(tempName, service.statePath); err != nil {
		return fmt.Errorf("media studio: replace state: %w", err)
	}
	remove = false
	return nil
}
