# Video Restore 4K

一个免费的本地视频修复工具原型：选择 AVI 或其他本地视频，调用 FFmpeg 抽帧，调用 Real-ESRGAN-ncnn-vulkan 超分，再合成为 4K MP4。

## 依赖

需要先安装并能运行这些免费工具：

- FFmpeg 和 FFprobe
- Real-ESRGAN-ncnn-vulkan

macOS 可以用 Homebrew 安装 FFmpeg：

```bash
brew install ffmpeg
```

Real-ESRGAN-ncnn-vulkan 下载官方 Windows/macOS 便携版后，有两种使用方式：

- 放到项目的 `tools/` 目录，程序会自动识别 `tools/realesrgan-ncnn-vulkan` 或 `tools/realesrgan-ncnn-vulkan.exe`
- 或者把可执行文件路径填到界面里的 `Real-ESRGAN` 输入框

## 运行

```bash
go run .
```

程序会启动本地服务，并自动打开浏览器界面。

## 构建

当前机器构建：

```bash
go build -o video-restore-4k .
```

Windows amd64：

```bash
GOOS=windows GOARCH=amd64 go build -o video-restore-4k.exe .
```

macOS amd64：

```bash
GOOS=darwin GOARCH=amd64 go build -o video-restore-4k .
```

## 处理说明

- 默认启用轻度降噪和去隔行，适合老 AVI。
- 默认处理模式是 `GTX 1060 快速`：Real-ESRGAN 先做 AI x2，再由 FFmpeg 缩放到目标分辨率。
- GTX 1060 3GB 建议保持 `Tile 显存控制 = 64`；如果稳定后想加速，可以试 `96` 或 `128`。
- GTX 1060 快速模式会额外使用 Real-ESRGAN 的保守队列参数 `-j 1:1:1`，降低 `vkQueueSubmit failed -4` / 显卡设备丢失的概率。
- `画质优先` 会按原视频尺寸自动选择 x2/x3/x4，速度更慢，显存压力更高。
- 默认目标分辨率是 `3840x2160`，会保持画面比例并补黑边。
- 输出文件名为 `原文件名_4k.mp4`、`原文件名_1080p.mp4` 等。
- 如果留空输出目录，文件会保存到程序启动时所在目录。
- 超分阶段会根据最近 8 个完成帧采样窗口估算剩余时间；刚开始样本少，预计时间会动态跳动。
- 超分阶段会显示最近一帧处理时长；如果 1 秒内完成多帧，会显示这批帧的平均耗时。
- 大视频会很慢，建议先用短片段测试。

## Windows GTX 1060 打包

在 macOS/Linux 开发机上可以生成 Windows amd64 目录：

```bash
scripts/package-windows.sh
```

输出目录：

```text
dist/windows/
  video-restore-4k.exe
  tools/
    realesrgan-ncnn-vulkan.exe
    vcomp140.dll
    models/
```

Windows 机器上仍需要 FFmpeg。安装后如果程序没自动找到，在界面里填 `ffmpeg.exe` 和 `ffprobe.exe` 的完整路径。
