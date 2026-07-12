#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dist_dir="$root_dir/dist/windows"
cache_dir="${TMPDIR:-/tmp}/video-restore-4k"
zip_path="$cache_dir/realesrgan-ncnn-vulkan-windows.zip"
realesrgan_url="https://github.com/xinntao/Real-ESRGAN/releases/download/v0.2.5.0/realesrgan-ncnn-vulkan-20220424-windows.zip"

mkdir -p "$dist_dir/tools" "$cache_dir"

echo "Building Windows amd64 app..."
(
  cd "$root_dir"
  GOOS=windows GOARCH=amd64 go build -o "$dist_dir/video-restore-4k.exe" .
)

if [ ! -f "$zip_path" ]; then
  echo "Downloading Real-ESRGAN-ncnn-vulkan for Windows..."
  curl -L -o "$zip_path" "$realesrgan_url"
fi

echo "Extracting Real-ESRGAN runtime..."
unzip -o "$zip_path" -d "$dist_dir/tools"

cat > "$dist_dir/README-WINDOWS.txt" <<'TXT'
Video Restore 4K - Windows amd64

1. Install FFmpeg for Windows and make sure ffmpeg.exe and ffprobe.exe are in PATH.
2. Double-click video-restore-4k.exe.
3. The browser UI should show Real-ESRGAN ready.
4. For GTX 1060 3GB, keep:
   - Processing mode: GTX 1060 fast
   - Tile: 64
   - Target: 4K or 1080p for faster tests
   - If Vulkan reports vkQueueSubmit failed -4, try Tile: 32 and close other GPU-heavy apps.

If FFmpeg is missing, fill the absolute paths to ffmpeg.exe and ffprobe.exe in the UI.
TXT

echo "Done: $dist_dir"
