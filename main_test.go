package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestEfficientInputSize(t *testing.T) {
	tests := []struct {
		name                  string
		width, height         int
		targetWidth           int
		targetHeight          int
		scale                 int
		wantWidth, wantHeight int
	}{
		{
			name:  "1080p source is reduced for AI x2",
			width: 1920, height: 1080,
			targetWidth: 1920, targetHeight: 1080,
			scale:     2,
			wantWidth: 960, wantHeight: 540,
		},
		{
			name:  "four by three source keeps its aspect ratio",
			width: 1440, height: 1080,
			targetWidth: 1920, targetHeight: 1080,
			scale:     2,
			wantWidth: 720, wantHeight: 540,
		},
		{
			name:  "small source is not enlarged before AI",
			width: 640, height: 360,
			targetWidth: 1920, targetHeight: 1080,
			scale:     2,
			wantWidth: 640, wantHeight: 360,
		},
		{
			name:  "AI output target leaves the input unchanged",
			width: 1920, height: 1080,
			scale:     2,
			wantWidth: 1920, wantHeight: 1080,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			width, height := efficientInputSize(
				tt.width,
				tt.height,
				tt.targetWidth,
				tt.targetHeight,
				tt.scale,
			)
			if width != tt.wantWidth || height != tt.wantHeight {
				t.Fatalf("got %dx%d, want %dx%d", width, height, tt.wantWidth, tt.wantHeight)
			}
		})
	}
}

func TestEncoderArgs(t *testing.T) {
	tests := []struct {
		encoder string
		want    []string
	}{
		{
			encoder: "h264_nvenc",
			want: []string{
				"-c:v", "h264_nvenc", "-preset", "p4", "-cq", "19", "-b:v", "0",
			},
		},
		{
			encoder: "h264_qsv",
			want: []string{
				"-c:v", "h264_qsv", "-preset", "medium", "-global_quality", "19",
			},
		},
		{
			encoder: "h264_amf",
			want: []string{
				"-c:v", "h264_amf", "-quality", "balanced", "-rc", "cqp",
				"-qp_i", "18", "-qp_p", "20",
			},
		},
		{
			encoder: "libx264",
			want: []string{
				"-c:v", "libx264", "-crf", "18", "-preset", "medium",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.encoder, func(t *testing.T) {
			if got := encoderArgs(tt.encoder); !slices.Equal(got, tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTurboModeUsesX2Scale(t *testing.T) {
	if got := upscaleScale(1920, 1080, "turbo"); got != 2 {
		t.Fatalf("got scale %d, want 2", got)
	}
	if got := queueSetting("balanced"); got != "1:2:2" {
		t.Fatalf("got queue %q, want %q", got, "1:2:2")
	}
}

func TestEncodingArgsPreview(t *testing.T) {
	request := encodingRequest{
		inputArgs:  []string{"-i", "input.avi"},
		mapArgs:    []string{"-map", "0:v:0"},
		outputPath: "preview.mp4",
		frameLimit: defaultPreviewFrameCount,
		shortest:   true,
	}
	args := encodingArgs(request, "libx264")
	wantSequence := []string{"-frames:v", "20", "-shortest"}
	if !containsSequence(args, wantSequence) {
		t.Fatalf("preview arguments %q do not contain %q", args, wantSequence)
	}
}

func TestPrepareOutputPathPreview(t *testing.T) {
	outputDir := t.TempDir()
	path, err := prepareOutputPath(
		"/tmp/input.avi",
		"family.avi",
		processOptions{
			OutputFolder:  outputDir,
			Target:        "1080p",
			Preview:       true,
			PreviewFrames: defaultPreviewFrameCount,
		},
	)
	if err != nil {
		t.Fatalf("prepareOutputPath returned an error: %v", err)
	}
	want := filepath.Join(outputDir, "family_1080p_preview_20f.mp4")
	if path != want {
		t.Fatalf("got path %q, want %q", path, want)
	}
}

func containsSequence(values, sequence []string) bool {
	if len(sequence) > len(values) {
		return false
	}
	for i := 0; i <= len(values)-len(sequence); i++ {
		if slices.Equal(values[i:i+len(sequence)], sequence) {
			return true
		}
	}
	return false
}

func TestHandleJobOutput(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "preview.mp4")
	wantBody := []byte("local preview")
	if err := os.WriteFile(outputPath, wantBody, 0o600); err != nil {
		t.Fatalf("writing preview fixture: %v", err)
	}
	a := &app{
		jobs: map[string]*job{
			"preview-job": {
				ID:         "preview-job",
				Status:     "done",
				OutputPath: outputPath,
			},
		},
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/jobs/preview-job/output", nil)

	a.handleJob(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Body.Bytes(); !slices.Equal(got, wantBody) {
		t.Fatalf("got body %q, want %q", got, wantBody)
	}
}
