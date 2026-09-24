package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shuyu2001/zsy_webview"
	"github.com/shuyu2001/zsy_webview/pkg/edge"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

//go:embed hud.html
var hudHTML string

//go:embed settings.html
var settingsHTML string

type Stock struct {
	Code           string  `json:"code"`
	Name           string  `json:"name"`
	CurrentPrice   float64 `json:"current"`
	YesterdayClose float64 `json:"prev_close"`
	OpenPrice      float64 `json:"open"`
	HighPrice      float64 `json:"high"`
	LowPrice       float64 `json:"low"`
	Volume         int64   `json:"volume"`
	Amount         float64 `json:"amount"`
	ChangeAmount   float64 `json:"change_amount"`
	ChangeRate     float64 `json:"change_rate"`
	LastUpdated    string  `json:"last_updated"`
}

type Config struct {
	Codes    []string `json:"codes"`
	Interval int      `json:"interval"`
}

type SafeState struct {
	sync.RWMutex
	Config     Config
	CachedList []Stock
	LastSync   string
}

var state = &SafeState{
	Config: Config{
		Codes:    []string{"sh000001", "sz399001", "sh600519"},
		Interval: 3,
	},
	LastSync: "--:--:--",
}

var resetTickerChan = make(chan struct{}, 1)

func fetchBatchStockQuotes(codes []string) map[string]Stock {
	result := make(map[string]Stock)
	if len(codes) == 0 {
		return result
	}

	url := fmt.Sprintf("https://qt.gtimg.cn/q=%s", strings.Join(codes, ","))
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Referer", "https://finance.qq.com")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return result
	}
	defer resp.Body.Close()

	bodyReader := transform.NewReader(resp.Body, simplifiedchinese.GBK.NewDecoder())
	bodyBytes, err := io.ReadAll(bodyReader)
	if err != nil {
		return result
	}

	lines := strings.Split(strings.TrimSpace(string(bodyBytes)), ";\n")
	for _, line := range lines {
		if !strings.Contains(line, "=") {
			continue
		}
		parts := strings.Split(line, "=")
		queryCode := strings.TrimSpace(strings.ReplaceAll(parts[0], "v_", ""))
		fields := strings.Split(strings.ReplaceAll(parts[1], "\"", ""), "~")
		if len(fields) < 35 {
			continue
		}

		current, _ := strconv.ParseFloat(fields[3], 64)
		prevClose, _ := strconv.ParseFloat(fields[4], 64)
		open, _ := strconv.ParseFloat(fields[5], 64)
		high, _ := strconv.ParseFloat(fields[33], 64)
		if high == 0 {
			high = current
		}
		low, _ := strconv.ParseFloat(fields[34], 64)
		if low == 0 {
			low = current
		}
		vol, _ := strconv.ParseInt(fields[6], 10, 64)
		amt, _ := strconv.ParseFloat(fields[37], 64)

		var changeRate float64
		if prevClose > 0 {
			changeRate = math.Round(((current-prevClose)/prevClose)*10000) / 100
		}
		lastUpdated := "--"
		if len(fields[30]) >= 14 {
			lastUpdated = fmt.Sprintf("%s:%s:%s", fields[30][8:10], fields[30][10:12], fields[30][12:14])
		}

		result[queryCode] = Stock{
			Code:           queryCode,
			Name:           fields[1],
			CurrentPrice:   current,
			YesterdayClose: prevClose,
			OpenPrice:      open,
			HighPrice:      high,
			LowPrice:       low,
			Volume:         vol,
			Amount:         amt,
			ChangeAmount:   math.Round((current-prevClose)*100) / 100,
			ChangeRate:     changeRate,
			LastUpdated:    lastUpdated,
		}
	}
	return result
}

func main() {
	chromium := edge.NewChromium()
	if chromium == nil {
		log.Fatal("Chromium 运行时初始化失败，请确保系统已安装 WebView2 Runtime")
	}

	// 1. 创建主悬浮面板（宽度300，高度260更舒适）
	app := zsy_webview.NewWithOptions(zsy_webview.WebviewOptions{
		Title:         "StockHUD",
		Width:         215, // 紧凑挂件宽度
		Height:        115, // 3只股票的最佳高度
		HideInTaskbar: true,
		Frameless:     true, // 无边框
		Transparent:   true, // 核心：开启 DirectComposition 穿透透明
		AlwaysOnTop:   true, // 始终置顶
		Center:        true,
		Chromium:      chromium,
	})

	app.DisableFileDrop()

	// 2. 创建设置子窗口
	settingsWin := app.NewChildWindow(zsy_webview.WebviewOptions{
		Title:       "股票监控设置",
		Width:       440,
		Height:      520,
		Frameless:   false,
		AlwaysOnTop: true,
		Center:      false,
		Debug:       false,
	})

	settingsWin.OnClose(func() bool {
		settingsWin.Window.HideWindow()
		return false
	})

	// 3. 注册安全通信通道 (JS主动拉取，不使用跨线程Eval)
	app.Bind("openSettings", func() {
		fmt.Println("进来了")
		settingsWin.Window.ShowWindow()

		settingsWin.Eval("alert(888)")
	})

	app.Bind("exitApp", func() {
		app.Destroy()
	})

	// 前端定时拉取的数据接口（彻底杜绝多线程 Eval 冲突）
	app.Bind("getStocksData", func() string {
		state.RLock()
		defer state.RUnlock()

		payload := map[string]interface{}{
			"stocks":  state.CachedList,
			"updated": state.LastSync,
		}
		b, _ := json.Marshal(payload)
		return string(b)
	})

	// 设置窗口绑定
	settingsWin.Bind("getConfig", func() string {
		state.RLock()
		defer state.RUnlock()
		b, _ := json.Marshal(state.Config)
		return string(b)
	})

	settingsWin.Bind("saveConfig", func(newCodes []string, interval int) {
		state.Lock()
		state.Config.Codes = newCodes
		if interval < 1 {
			interval = 1
		}
		state.Config.Interval = interval
		state.Unlock()

		// 立即中断等待刷新
		select {
		case resetTickerChan <- struct{}{}:
		default:
		}
	})

	// 4. 加载页面
	app.SetHtml(hudHTML)
	settingsWin.SetHtml(settingsHTML)

	// 5. 后台协程：只负责更新共享内存，零UI操作，极致稳定
	go func() {
		for {
			state.RLock()
			codes := make([]string, len(state.Config.Codes))
			copy(codes, state.Config.Codes)
			interval := state.Config.Interval
			state.RUnlock()

			if len(codes) > 0 {
				dataMap := fetchBatchStockQuotes(codes)
				var list []Stock
				for _, c := range codes {
					if item, ok := dataMap[c]; ok {
						list = append(list, item)
					}
				}

				state.Lock()
				state.CachedList = list
				state.LastSync = time.Now().Format("15:04:05")
				state.Unlock()
			} else {
				state.Lock()
				state.CachedList = nil
				state.LastSync = time.Now().Format("15:04:05")
				state.Unlock()
			}

			select {
			case <-time.After(time.Duration(interval) * time.Second):
			case <-resetTickerChan:
			}
		}
	}()

	// 6. 运行主消息循环
	app.Run()
}
