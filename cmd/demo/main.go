package main

import (
	"log"

	"github.com/shuyu2001/zsy_webview"
	"github.com/shuyu2001/zsy_webview/pkg/edge"
)

func main() {
	// 1. 初始化 Chromium
	chromium := edge.NewChromium()
	if chromium == nil {
		log.Fatal("Chromium 运行时初始化失败，请确保系统已安装 WebView2 Runtime")
	}

	// 2. 创建极致通透窗口
	app := zsy_webview.NewWithOptions(zsy_webview.WebviewOptions{
		Title:       "极致通透番茄钟",
		Width:       220, // 紧凑挂件尺寸
		Height:      95,
		Frameless:   true, // 无边框
		Transparent: true, // 核心：开启 DirectComposition 穿透透明
		AlwaysOnTop: true, // 始终置顶
		Center:      true, // 初始居中
		Chromium:    chromium,
	})

	app.DisableFileDrop()

	// 3. 注入纯净透明 UI
	app.SetHtml(hudHTML)

	// 4. 启动 Win32 消息循环
	app.Run()
}

// 极致通透的前端 UI（HTML + CSS + JS）
const hudHTML = `
<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<style>
  /* 基础重置：彻底消除所有默认背景与滚动条 */
  * {
    margin: 0;
    padding: 0;
    box-sizing: border-box;
    user-select: none;
    -webkit-user-select: none;
  }

  html, body {
    width: 100vw;
    height: 100vh;
    background: transparent !important;
    overflow: hidden;
    display: flex;
    justify-content: center;
    align-items: center;
    font-family: "Cascadia Code", "JetBrains Mono", Consolas, "SF Pro Display", -apple-system, sans-serif;
  }

  /* 悬浮容器（空气感，无卡片实体） */
  .hud-wrapper {
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    cursor: move;
    padding: 6px 12px;
  }

  /* 状态小标签 (FOCUS / BREAK) */
  .hud-status {
    font-size: 10px;
    font-weight: 800;
    letter-spacing: 2px;
    color: #ffffff;
    opacity: 0.9;
    margin-bottom: -4px;
    /* 双重描边确保可见性 */
    text-shadow: 
      -1px -1px 0 #000,  
       1px -1px 0 #000,
      -1px  1px 0 #000,
       1px  1px 0 #000,
       0 0 6px rgba(0,0,0,0.8);
    transition: color 0.3s;
  }

  /* 核心时间数字：全场景自适应发光轮廓 */
  .hud-time {
    font-size: 44px;
    font-weight: 900;
    line-height: 1.1;
    letter-spacing: 1px;
    font-variant-numeric: tabular-nums;
    color: #ff4757; /* 默认番茄红 */
    
    /* 核心黑科技：四向死角黑描边 + 强效霓虹光晕 */
    /* 保证在纯白背景下有黑边托底，在纯黑背景下有红光发亮 */
    text-shadow: 
      -2px -2px 0 #000,  
       2px -2px 0 #000,
      -2px  2px 0 #000,
       2px  2px 0 #000,
      -1px  0   0 #000,
       1px  0   0 #000,
       0   -1px 0 #000,
       0    1px 0 #000,
       0 0 12px rgba(255, 71, 87, 0.75),
       0 0 24px rgba(255, 71, 87, 0.35);
    transition: color 0.3s, text-shadow 0.3s;
  }

  /* 休息状态下的绿色霓虹 */
  .hud-time.break-mode {
    color: #2ed573;
    text-shadow: 
      -2px -2px 0 #000,  
       2px -2px 0 #000,
      -2px  2px 0 #000,
       2px  2px 0 #000,
       0 0 12px rgba(46, 213, 115, 0.75),
       0 0 24px rgba(46, 213, 115, 0.35);
  }

  /* 控制按钮栏：默认隐藏，鼠标悬停时优雅淡入 */
  .hud-controls {
    display: flex;
    gap: 6px;
    margin-top: 2px;
    opacity: 0;
    transform: translateY(-2px);
    transition: opacity 0.2s ease, transform 0.2s ease;
  }

  .hud-wrapper:hover .hud-controls {
    opacity: 1;
    transform: translateY(0);
  }

  /* 极简发光操作按钮 */
  .hud-btn {
    background: rgba(0, 0, 0, 0.75);
    border: 1px solid rgba(255, 255, 255, 0.3);
    color: #ffffff;
    font-size: 11px;
    font-weight: 600;
    padding: 2px 8px;
    border-radius: 4px;
    cursor: pointer;
    box-shadow: 0 2px 6px rgba(0, 0, 0, 0.6);
    transition: all 0.15s ease;
  }

  .hud-btn:hover {
    background: #ff4757;
    border-color: #ff4757;
    color: #fff;
    box-shadow: 0 0 8px rgba(255, 71, 87, 0.8);
  }

  .hud-btn.btn-close:hover {
    background: #e84118;
    border-color: #e84118;
  }
</style>
</head>
<body>

  <div class="hud-wrapper" id="dragArea">
    <div class="hud-status" id="modeLabel">TOMATO</div>
    <div class="hud-time" id="timeDisplay">25:00</div>
    
    <div class="hud-controls">
      <button class="hud-btn" id="btnToggle" onclick="toggleTimer(event)">开始</button>
      <button class="hud-btn" onclick="switchMode(event)">模式</button>
      <button class="hud-btn" onclick="resetTimer(event)">重置</button>
      <button class="hud-btn btn-close" onclick="closeApp(event)">✕</button>
    </div>
  </div>

<script>
  // 1. 原生平滑拖拽处理（点击按钮不触发拖动）
  const dragArea = document.getElementById('dragArea');
  dragArea.addEventListener('mousedown', (e) => {
    if (e.target.tagName !== 'BUTTON') {
      window.chrome.webview.postMessage('__drag__');
    }
  });

  function closeApp(e) {
    e.stopPropagation();
    window.chrome.webview.postMessage('__close__');
  }

  // 2. 番茄钟状态机逻辑
  const FOCUS_TIME = 25 * 60;
  const BREAK_TIME = 5 * 60;

  let isFocusMode = true;
  let timeLeft = FOCUS_TIME;
  let timerId = null;
  let isRunning = false;

  const timeDisplay = document.getElementById('timeDisplay');
  const modeLabel = document.getElementById('modeLabel');
  const btnToggle = document.getElementById('btnToggle');

  // 原生合成提示音（无需下载任何外部 mp3）
  function playBeep() {
    try {
      const ctx = new (window.AudioContext || window.webkitAudioContext)();
      const osc = ctx.createOscillator();
      const gain = ctx.createGain();
      osc.type = 'sine';
      osc.frequency.setValueAtTime(880, ctx.currentTime); // A5 提示音
      gain.gain.setValueAtTime(0.1, ctx.currentTime);
      gain.gain.exponentialRampToValueAtTime(0.001, ctx.currentTime + 0.6);
      osc.connect(gain);
      gain.connect(ctx.destination);
      osc.start();
      osc.stop(ctx.currentTime + 0.6);
    } catch(e) {}
  }

  function render() {
    const m = String(Math.floor(timeLeft / 60)).padStart(2, '0');
    const s = String(timeLeft % 60).padStart(2, '0');
    timeDisplay.textContent = ` + "`${m}:${s}`" + `;
  }

  function toggleTimer(e) {
    if (e) e.stopPropagation();
    if (isRunning) {
      clearInterval(timerId);
      btnToggle.textContent = '继续';
    } else {
      timerId = setInterval(() => {
        if (timeLeft > 0) {
          timeLeft--;
          render();
        } else {
          clearInterval(timerId);
          isRunning = false;
          playBeep();
          // 自动切换模式
          switchMode();
          toggleTimer();
        }
      }, 1000);
      btnToggle.textContent = '暂停';
    }
    isRunning = !isRunning;
  }

  function resetTimer(e) {
    if (e) e.stopPropagation();
    clearInterval(timerId);
    isRunning = false;
    timeLeft = isFocusMode ? FOCUS_TIME : BREAK_TIME;
    btnToggle.textContent = '开始';
    render();
  }

  function switchMode(e) {
    if (e) e.stopPropagation();
    clearInterval(timerId);
    isRunning = false;
    isFocusMode = !isFocusMode;

    if (isFocusMode) {
      timeLeft = FOCUS_TIME;
      modeLabel.textContent = 'FOCUS';
      timeDisplay.classList.remove('break-mode');
    } else {
      timeLeft = BREAK_TIME;
      modeLabel.textContent = 'REST';
      timeDisplay.classList.add('break-mode');
    }

    btnToggle.textContent = '开始';
    render();
  }

  render();
</script>
</body>
</html>
`
