const form = document.querySelector("#jobForm");
const fileInput = document.querySelector("#video");
const fileLabel = document.querySelector("#fileLabel");
const startBtn = document.querySelector("#startBtn");
const previewBtn = document.querySelector("#previewBtn");
const cancelBtn = document.querySelector("#cancelBtn");
const stageText = document.querySelector("#stageText");
const percentText = document.querySelector("#percentText");
const meterBar = document.querySelector("#meterBar");
const resultText = document.querySelector("#resultText");
const logEl = document.querySelector("#log");
const clearLog = document.querySelector("#clearLog");
const toolStatus = document.querySelector("#toolStatus");
const upscaleMode = document.querySelector("#upscaleMode");
const model = document.querySelector("#model");
const previewArea = document.querySelector("#previewArea");
const previewPlayer = document.querySelector("#previewPlayer");
const previewFrames = document.querySelector("#previewFrames");
const previewTitle = document.querySelector("#previewTitle");

let currentJob = null;
let events = null;
let preferredModel = model.value;
let currentIsPreview = false;
let currentPreviewFrames = 20;

previewFrames.addEventListener("input", syncPreviewFrameText);

function syncPreviewFrameText() {
	const frames = Math.max(1, Number.parseInt(previewFrames.value, 10) || 20);
	previewBtn.textContent = `预览前 ${frames} 帧`;
}

model.addEventListener("change", () => {
  if (upscaleMode.value !== "turbo") preferredModel = model.value;
});

upscaleMode.addEventListener("change", syncModeControls);

function syncModeControls() {
  if (upscaleMode.value === "turbo") {
    model.value = "realesr-animevideov3";
    model.disabled = true;
    return;
  }
  model.value = preferredModel;
  model.disabled = upscaleMode.value === "fast";
}

fileInput.addEventListener("change", () => {
  const file = fileInput.files[0];
  fileLabel.textContent = file ? file.name : "选择本地 AVI 视频";
});

clearLog.addEventListener("click", () => {
  logEl.textContent = "";
});

cancelBtn.addEventListener("click", async () => {
  if (!currentJob) return;
  await fetch(`/api/jobs/${currentJob}/cancel`, { method: "POST" });
});

form.addEventListener("submit", (event) => {
	event.preventDefault();
	startJob(false);
});

previewBtn.addEventListener("click", () => startJob(true));

async function startJob(preview) {
	if (!form.reportValidity()) return;
	currentPreviewFrames = Math.max(1, Number.parseInt(previewFrames.value, 10) || 20);
  resultText.textContent = "";
  logEl.textContent = "";
	previewArea.hidden = true;
	previewPlayer.removeAttribute("src");
	previewPlayer.load();
  setBusy(true);
	setProgress(2, preview ? "上传视频并准备预览" : "上传视频");

  const data = new FormData(form);
  data.set("denoise", form.denoise.checked ? "true" : "false");
  data.set("deinterlace", form.deinterlace.checked ? "true" : "false");
	data.set("preview", preview ? "true" : "false");

  try {
    const response = await fetch("/api/jobs", { method: "POST", body: data });
    if (!response.ok) throw new Error(await response.text());
    const job = await response.json();
    currentJob = job.id;
	currentIsPreview = preview;
    watchJob(job.id);
  } catch (error) {
    appendLog(`Error: ${error.message}`);
    setProgress(0, "启动失败");
    setBusy(false);
  }
}

async function loadTools() {
  try {
    const response = await fetch("/api/tools");
    const tools = await response.json();
    document.querySelector("#ffmpeg").value = tools.ffmpeg || "";
    document.querySelector("#ffprobe").value = tools.ffprobe || "";
    document.querySelector("#realesrgan").value = tools.realesrgan || "";
    const missing = [];
    if (!tools.ffmpeg) missing.push("FFmpeg");
    if (!tools.ffprobe) missing.push("FFprobe");
    if (!tools.realesrgan) missing.push("Real-ESRGAN");
    toolStatus.textContent = missing.length ? `缺少：${missing.join("、")}` : "工具已就绪";
  } catch {
    toolStatus.textContent = "工具检查失败";
  }
}

function watchJob(id) {
  if (events) events.close();
  events = new EventSource(`/api/jobs/${id}/events`);
  events.onmessage = (message) => {
    const job = JSON.parse(message.data);
    renderJob(job);
  };
  events.onerror = () => {
    events.close();
    setBusy(false);
  };
}

function renderJob(job) {
  setProgress(job.progress, job.message || job.stage);
  logEl.textContent = (job.logs || []).join("\n");
  logEl.scrollTop = logEl.scrollHeight;

  if (job.status === "done") {
    resultText.textContent = `完成：${job.outputPath}`;
	if (currentIsPreview) {
	  previewTitle.textContent = `修复预览 · 前 ${currentPreviewFrames} 帧`;
	  previewPlayer.src = `/api/jobs/${job.id}/output`;
	  previewArea.hidden = false;
	  previewPlayer.load();
	}
    setBusy(false);
    if (events) events.close();
  }
  if (job.status === "failed") {
    resultText.textContent = job.error || "处理失败";
    setBusy(false);
    if (events) events.close();
  }
  if (job.status === "cancelled") {
    resultText.textContent = "已取消";
    setBusy(false);
    if (events) events.close();
  }
}

function setProgress(percent, stage) {
  const value = Math.max(0, Math.min(100, Number(percent) || 0));
  meterBar.style.width = `${value}%`;
  percentText.textContent = `${value}%`;
  stageText.textContent = stage;
}

function setBusy(busy) {
  startBtn.disabled = busy;
	previewBtn.disabled = busy;
  cancelBtn.disabled = !busy;
}

function appendLog(line) {
  logEl.textContent += `${line}\n`;
}

loadTools();
syncModeControls();
syncPreviewFrameText();
