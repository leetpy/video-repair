package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var webFS embed.FS

type app struct {
	mu   sync.Mutex
	jobs map[string]*job
}

type job struct {
	ID         string    `json:"id"`
	FileName   string    `json:"fileName"`
	Status     string    `json:"status"`
	Stage      string    `json:"stage"`
	Progress   int       `json:"progress"`
	Message    string    `json:"message"`
	OutputPath string    `json:"outputPath,omitempty"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	Logs       []string  `json:"logs"`

	dir    string
	cancel context.CancelFunc
}

type processOptions struct {
	FFmpegPath   string
	FFprobePath  string
	RealESRGAN   string
	ModelPath    string
	Model        string
	UpscaleMode  string
	Target       string
	TileSize     int
	QueueMode    string
	Denoise      bool
	Deinterlace  bool
	OutputFolder string
}

type probeInfo struct {
	Streams []struct {
		CodecType    string `json:"codec_type"`
		Width        int    `json:"width"`
		Height       int    `json:"height"`
		RFrameRate   string `json:"r_frame_rate"`
		AvgFrameRate string `json:"avg_frame_rate"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

type progressSample struct {
	frames   int
	duration time.Duration
}

func main() {
	a := &app{jobs: make(map[string]*job)}
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/app.css", a.handleAsset("web/app.css", "text/css; charset=utf-8"))
	mux.HandleFunc("/app.js", a.handleAsset("web/app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("/api/tools", a.handleTools)
	mux.HandleFunc("/api/jobs", a.handleCreateJob)
	mux.HandleFunc("/api/jobs/", a.handleJob)

	address := "127.0.0.1:0"
	if port := strings.TrimSpace(os.Getenv("VR_PORT")); port != "" {
		address = "127.0.0.1:" + port
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatal(err)
	}
	url := "http://" + listener.Addr().String()
	log.Printf("VR video restorer is running at %s", url)
	if os.Getenv("VR_NO_BROWSER") != "1" {
		openBrowser(url)
	}

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	a.serveEmbedded(w, "web/index.html", "text/html; charset=utf-8")
}

func (a *app) handleAsset(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.serveEmbedded(w, name, contentType)
	}
}

func (a *app) serveEmbedded(w http.ResponseWriter, name, contentType string) {
	data, err := webFS.ReadFile(name)
	if err != nil {
		http.Error(w, "asset not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(data)
}

func (a *app) handleTools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]any{
		"ffmpeg":     lookup("ffmpeg"),
		"ffprobe":    lookup("ffprobe"),
		"realesrgan": localTool("realesrgan-ncnn-vulkan"),
	})
}

func (a *app) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	reader, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "expected multipart form data", http.StatusBadRequest)
		return
	}

	opts := defaultOptions()
	id := randomID()
	workDir := filepath.Join(os.TempDir(), "vr-video-restorer", id)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var inputPath, fileName string
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if part.FileName() != "" {
			name := filepath.Base(part.FileName())
			inputPath = filepath.Join(workDir, "input"+strings.ToLower(filepath.Ext(name)))
			fileName = name
			if err := savePart(inputPath, part); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			continue
		}
		value, _ := io.ReadAll(io.LimitReader(part, 4096))
		setOption(&opts, part.FormName(), string(value))
	}

	if inputPath == "" {
		http.Error(w, "choose a video file", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &job{
		ID:        id,
		FileName:  fileName,
		Status:    "queued",
		Stage:     "queued",
		Progress:  1,
		Message:   "Waiting to start",
		CreatedAt: time.Now(),
		dir:       workDir,
		cancel:    cancel,
	}
	a.mu.Lock()
	a.jobs[id] = j
	a.mu.Unlock()

	go a.process(ctx, j, inputPath, opts)
	writeJSON(w, j)
}

func (a *app) handleJob(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
	if strings.HasSuffix(path, "/events") {
		a.handleEvents(w, r, strings.TrimSuffix(path, "/events"))
		return
	}
	if strings.HasSuffix(path, "/cancel") {
		a.handleCancel(w, r, strings.TrimSuffix(path, "/cancel"))
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	j := a.getJob(path)
	if j == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, j)
}

func (a *app) handleEvents(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ticker := time.NewTicker(700 * time.Millisecond)
	defer ticker.Stop()
	for {
		j := a.getJob(id)
		if j == nil {
			return
		}
		data, _ := json.Marshal(j)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
		if j.Status == "done" || j.Status == "failed" || j.Status == "cancelled" {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *app) handleCancel(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	j := a.getJob(id)
	if j == nil {
		http.NotFound(w, r)
		return
	}
	if j.cancel != nil {
		j.cancel()
	}
	a.update(j.ID, func(j *job) {
		j.Status = "cancelled"
		j.Stage = "cancelled"
		j.Message = "Cancelled"
	})
	writeJSON(w, j)
}

func (a *app) process(ctx context.Context, j *job, inputPath string, opts processOptions) {
	defer func() {
		if r := recover(); r != nil {
			a.fail(j.ID, fmt.Errorf("panic: %v", r))
		}
	}()
	if err := validateTools(opts); err != nil {
		a.fail(j.ID, err)
		return
	}

	a.step(j.ID, "probe", 5, "Reading video metadata")
	info, err := probe(ctx, opts.FFprobePath, inputPath)
	if err != nil {
		a.fail(j.ID, err)
		return
	}
	width, height, fps := videoFacts(info)
	if width == 0 || height == 0 {
		a.fail(j.ID, errors.New("no video stream found"))
		return
	}
	scale := upscaleScale(width, height, opts.UpscaleMode)
	targetWidth, targetHeight := targetSize(opts.Target)
	a.log(j.ID, fmt.Sprintf("Detected %dx%d at %s fps; Real-ESRGAN scale x%d; target %s", width, height, fps, scale, targetLabel(opts.Target)))

	framesDir := filepath.Join(j.dir, "frames")
	upscaledDir := filepath.Join(j.dir, "upscaled")
	if err := os.MkdirAll(framesDir, 0o755); err != nil {
		a.fail(j.ID, err)
		return
	}
	if err := os.MkdirAll(upscaledDir, 0o755); err != nil {
		a.fail(j.ID, err)
		return
	}

	a.step(j.ID, "extract", 12, "Extracting frames from AVI")
	filters := make([]string, 0, 2)
	if opts.Deinterlace {
		filters = append(filters, "yadif")
	}
	if opts.Denoise {
		filters = append(filters, "hqdn3d=1.5:1.5:6:6")
	}
	args := []string{"-hide_banner", "-y", "-i", inputPath, "-map", "0:v:0"}
	if len(filters) > 0 {
		args = append(args, "-vf", strings.Join(filters, ","))
	}
	args = append(args, "-vsync", "0", filepath.Join(framesDir, "%08d.png"))
	if err := a.run(ctx, j.ID, opts.FFmpegPath, args...); err != nil {
		a.fail(j.ID, err)
		return
	}
	totalFrames, err := countFiles(framesDir, ".png")
	if err != nil {
		a.fail(j.ID, err)
		return
	}
	if totalFrames == 0 {
		a.fail(j.ID, errors.New("no frames were extracted from the input video"))
		return
	}
	a.log(j.ID, fmt.Sprintf("Extracted %d frames", totalFrames))

	a.step(j.ID, "upscale", 35, "Upscaling frames with Real-ESRGAN")
	args = []string{
		"-i", framesDir,
		"-o", upscaledDir,
		"-m", modelPathForExecutable(opts.RealESRGAN, opts.ModelPath),
		"-n", opts.Model,
		"-s", strconv.Itoa(scale),
		"-f", "png",
	}
	if opts.TileSize > 0 {
		args = append(args, "-t", strconv.Itoa(opts.TileSize))
	}
	if queue := queueSetting(opts.QueueMode); queue != "" {
		args = append(args, "-j", queue)
	}
	monitorDone := make(chan struct{})
	go a.monitorFrameProgress(j.ID, upscaledDir, totalFrames, monitorDone)
	err = a.run(ctx, j.ID, opts.RealESRGAN, args...)
	close(monitorDone)
	if err != nil {
		a.fail(j.ID, err)
		return
	}
	a.update(j.ID, func(j *job) {
		j.Progress = 77
		j.Message = "Upscaled all frames"
	})

	outputDir := opts.OutputFolder
	if outputDir == "" {
		outputDir = filepath.Dir(inputPath)
	}
	if outputDir == filepath.Dir(inputPath) {
		cwd, err := os.Getwd()
		if err == nil {
			outputDir = cwd
		}
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		a.fail(j.ID, err)
		return
	}
	outputName := strings.TrimSuffix(filepath.Base(j.FileName), filepath.Ext(j.FileName)) + "_" + outputSuffix(opts.Target) + ".mp4"
	outputPath := filepath.Join(outputDir, outputName)

	a.step(j.ID, "encode", 78, "Encoding MP4 with original audio")
	framePattern := filepath.Join(upscaledDir, "%08d.png")
	args = []string{"-hide_banner", "-y", "-framerate", fps, "-i", framePattern, "-i", inputPath}
	videoFilters := []string{"setsar=1"}
	if targetWidth > 0 && targetHeight > 0 {
		videoFilters = append(videoFilters,
			fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease", targetWidth, targetHeight),
			fmt.Sprintf("pad=%d:%d:(ow-iw)/2:(oh-ih)/2", targetWidth, targetHeight),
		)
	}
	args = append(args,
		"-map", "0:v:0",
		"-map", "1:a?",
		"-vf", strings.Join(videoFilters, ","),
		"-c:v", "libx264",
		"-pix_fmt", "yuv420p",
		"-crf", "18",
		"-preset", "slow",
		"-c:a", "aac",
		"-b:a", "192k",
		"-movflags", "+faststart",
		outputPath,
	)
	if err := a.run(ctx, j.ID, opts.FFmpegPath, args...); err != nil {
		a.fail(j.ID, err)
		return
	}

	a.update(j.ID, func(j *job) {
		j.Status = "done"
		j.Stage = "done"
		j.Progress = 100
		j.Message = "Finished"
		j.OutputPath = outputPath
	})
}

func (a *app) run(ctx context.Context, id, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	read := func(r io.Reader) {
		defer wg.Done()
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 1024), 1024*1024)
		for scanner.Scan() {
			a.log(id, scanner.Text())
		}
	}
	go read(stdout)
	go read(stderr)
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return errors.New("job cancelled")
		}
		return fmt.Errorf("%s failed: %w", filepath.Base(name), err)
	}
	return nil
}

func (a *app) getJob(id string) *job {
	a.mu.Lock()
	defer a.mu.Unlock()
	j, ok := a.jobs[id]
	if !ok {
		return nil
	}
	copyJob := *j
	copyJob.Logs = append([]string(nil), j.Logs...)
	return &copyJob
}

func (a *app) step(id, stage string, progress int, message string) {
	a.update(id, func(j *job) {
		j.Status = "running"
		j.Stage = stage
		j.Progress = progress
		j.Message = message
	})
	a.log(id, message)
}

func (a *app) fail(id string, err error) {
	a.update(id, func(j *job) {
		if j.Status == "cancelled" {
			return
		}
		j.Status = "failed"
		j.Stage = "failed"
		j.Error = err.Error()
		j.Message = "Failed"
	})
	a.log(id, "Error: "+err.Error())
}

func (a *app) log(id, line string) {
	a.update(id, func(j *job) {
		line = strings.TrimSpace(line)
		if line == "" || isPercentLine(line) {
			return
		}
		j.Logs = append(j.Logs, line)
		if len(j.Logs) > 300 {
			j.Logs = j.Logs[len(j.Logs)-300:]
		}
	})
}

func (a *app) monitorFrameProgress(id, dir string, total int, done <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastCount := 0
	lastTime := time.Now()
	lastFrameDuration := time.Duration(0)
	samples := make([]progressSample, 0, 8)
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			now := time.Now()
			count, err := countFiles(dir, ".png")
			if err != nil || total == 0 {
				continue
			}
			if count > lastCount {
				duration := now.Sub(lastTime)
				newFrames := count - lastCount
				samples = append(samples, progressSample{
					frames:   newFrames,
					duration: duration,
				})
				if len(samples) > 8 {
					samples = samples[len(samples)-8:]
				}
				if newFrames > 0 && duration > 0 {
					lastFrameDuration = duration / time.Duration(newFrames)
				}
				lastCount = count
				lastTime = now
			}
			progress := 35 + int(float64(count)/float64(total)*42)
			if progress > 77 {
				progress = 77
			}
			message := fmt.Sprintf("超分帧 %d/%d", count, total)
			if lastFrameDuration > 0 {
				message = fmt.Sprintf("%s · 最近 %.1fs/帧", message, lastFrameDuration.Seconds())
			}
			if eta, ok := estimateRemaining(count, total, samples); ok {
				message = fmt.Sprintf("%s · 预计剩余 %s", message, formatDuration(eta))
			} else if count > 0 {
				message = fmt.Sprintf("%s · 正在估算剩余时间", message)
			}
			a.update(id, func(j *job) {
				if j.Stage != "upscale" || j.Status != "running" {
					return
				}
				j.Progress = progress
				j.Message = message
			})
		}
	}
}

func (a *app) update(id string, fn func(*job)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if j, ok := a.jobs[id]; ok {
		fn(j)
	}
}

func defaultOptions() processOptions {
	return processOptions{
		FFmpegPath:  firstNonEmpty(lookup("ffmpeg"), "ffmpeg"),
		FFprobePath: firstNonEmpty(lookup("ffprobe"), "ffprobe"),
		RealESRGAN:  firstNonEmpty(localTool("realesrgan-ncnn-vulkan"), "realesrgan-ncnn-vulkan"),
		ModelPath:   localModelPath(),
		Model:       "realesrgan-x4plus",
		UpscaleMode: "gtx1060",
		Target:      "4k",
		TileSize:    64,
		QueueMode:   "safe",
		Denoise:     true,
		Deinterlace: true,
	}
}

func setOption(opts *processOptions, key, value string) {
	value = strings.TrimSpace(value)
	switch key {
	case "ffmpeg":
		if value != "" {
			opts.FFmpegPath = value
		}
	case "ffprobe":
		if value != "" {
			opts.FFprobePath = value
		}
	case "realesrgan":
		if value != "" {
			opts.RealESRGAN = value
		}
	case "model":
		if value != "" {
			opts.Model = value
		}
	case "upscaleMode":
		if value != "" {
			opts.UpscaleMode = value
		}
	case "target":
		if value != "" {
			opts.Target = value
		}
	case "tileSize":
		if tileSize, err := strconv.Atoi(value); err == nil && tileSize >= 0 {
			opts.TileSize = tileSize
		}
	case "queueMode":
		if value != "" {
			opts.QueueMode = value
		}
	case "outputFolder":
		opts.OutputFolder = value
	case "denoise":
		opts.Denoise = value == "true"
	case "deinterlace":
		opts.Deinterlace = value == "true"
	}
}

func validateTools(opts processOptions) error {
	for label, path := range map[string]string{
		"ffmpeg":      opts.FFmpegPath,
		"ffprobe":     opts.FFprobePath,
		"Real-ESRGAN": opts.RealESRGAN,
	} {
		if path == "" {
			return fmt.Errorf("%s path is empty", label)
		}
		if strings.ContainsRune(path, filepath.Separator) {
			if _, err := os.Stat(path); err != nil {
				return fmt.Errorf("%s not found at %q", label, path)
			}
			continue
		}
		if _, err := exec.LookPath(path); err != nil {
			return fmt.Errorf("%s not found in PATH; set its executable path", label)
		}
	}
	return nil
}

func probe(ctx context.Context, ffprobe, input string) (probeInfo, error) {
	var info probeInfo
	cmd := exec.CommandContext(ctx, ffprobe,
		"-v", "error",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		input,
	)
	data, err := cmd.Output()
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return info, err
	}
	return info, nil
}

func videoFacts(info probeInfo) (int, int, string) {
	for _, stream := range info.Streams {
		if stream.CodecType != "video" {
			continue
		}
		fps := stream.AvgFrameRate
		if fps == "" || fps == "0/0" {
			fps = stream.RFrameRate
		}
		if fps == "" || fps == "0/0" {
			fps = "25"
		}
		return stream.Width, stream.Height, fps
	}
	return 0, 0, "25"
}

func chooseScale(width, height int) int {
	longSide := max(width, height)
	switch {
	case longSide >= 3000:
		return 2
	case longSide >= 1500:
		return 2
	case longSide >= 1000:
		return 3
	default:
		return 4
	}
}

func upscaleScale(width, height int, mode string) int {
	switch mode {
	case "quality":
		return chooseScale(width, height)
	case "x4":
		return 4
	case "x2", "gtx1060":
		return 2
	default:
		return 2
	}
}

func targetSize(target string) (int, int) {
	switch target {
	case "4k":
		return 3840, 2160
	case "2k":
		return 2560, 1440
	case "1080p":
		return 1920, 1080
	case "ai":
		return 0, 0
	default:
		return 3840, 2160
	}
}

func queueSetting(mode string) string {
	switch mode {
	case "safe", "gtx1060":
		return "1:1:1"
	case "balanced":
		return "1:2:2"
	default:
		return "1:1:1"
	}
}

func targetLabel(target string) string {
	width, height := targetSize(target)
	if width == 0 || height == 0 {
		return "AI output"
	}
	return fmt.Sprintf("%dx%d", width, height)
}

func outputSuffix(target string) string {
	switch target {
	case "2k":
		return "2k"
	case "1080p":
		return "1080p"
	case "ai":
		return "ai"
	default:
		return "4k"
	}
}

func savePart(path string, part *multipart.Part) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(file, part)
	return err
}

func countFiles(dir, ext string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(entry.Name()), ext) {
			count++
		}
	}
	return count, nil
}

func isPercentLine(line string) bool {
	value := strings.TrimSuffix(line, "%")
	if value == line {
		return false
	}
	_, err := strconv.ParseFloat(value, 64)
	return err == nil
}

func estimateRemaining(done, total int, samples []progressSample) (time.Duration, bool) {
	remaining := total - done
	if remaining <= 0 || len(samples) == 0 {
		return 0, remaining <= 0
	}
	var sampleFrames int
	var sampleDuration time.Duration
	for _, sample := range samples {
		if sample.frames <= 0 || sample.duration <= 0 {
			continue
		}
		sampleFrames += sample.frames
		sampleDuration += sample.duration
	}
	if sampleFrames == 0 || sampleDuration <= 0 {
		return 0, false
	}
	perFrame := sampleDuration / time.Duration(sampleFrames)
	if perFrame <= 0 {
		return 0, false
	}
	return perFrame * time.Duration(remaining), true
}

func formatDuration(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	duration = duration.Round(time.Second)
	hours := int(duration / time.Hour)
	duration -= time.Duration(hours) * time.Hour
	minutes := int(duration / time.Minute)
	duration -= time.Duration(minutes) * time.Minute
	seconds := int(duration / time.Second)
	switch {
	case hours > 0:
		return fmt.Sprintf("%dh %02dm", hours, minutes)
	case minutes > 0:
		return fmt.Sprintf("%dm %02ds", minutes, seconds)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}

func lookup(name string) string {
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return path
}

func localTool(name string) string {
	candidates := []string{name}
	if runtime.GOOS == "windows" {
		candidates = append([]string{name + ".exe"}, candidates...)
	}
	roots := []string{"tools"}
	if exe, err := os.Executable(); err == nil {
		roots = append(roots, filepath.Join(filepath.Dir(exe), "tools"))
	}
	for _, root := range roots {
		for _, candidate := range candidates {
			path := filepath.Join(root, candidate)
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				return path
			}
		}
	}
	return lookup(name)
}

func localModelPath() string {
	candidates := []string{
		filepath.Join("tools", "models"),
		"models",
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "tools", "models"))
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "models"))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return "models"
}

func modelPathForExecutable(executable, configured string) string {
	if configured != "" {
		return configured
	}
	if strings.ContainsRune(executable, filepath.Separator) {
		sibling := filepath.Join(filepath.Dir(executable), "models")
		if info, err := os.Stat(sibling); err == nil && info.IsDir() {
			return sibling
		}
	}
	return localModelPath()
}

func randomID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
