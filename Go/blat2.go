//go:build linux
// +build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-vgo/robotgo"
	"github.com/kbinani/screenshot"
	"github.com/otiai10/gosseract/v2"
	"gocv.io/x/gocv"
)

/*
	========================================================
	High-level notes
	========================================================
	- Triggers ("long","short","nomove") are dynamic and occur in the SAME region; only one is present at a time.
	  "nomove" == ignore
	- Position ("longposition","shortposition","noposition") is separate; determines if you hold a position and which side.
	- Refresh is required for ThinkOrSwim studies (we fire a small input sequence at the symbol box like your script).
	- profit goal from config => when hit, close & switch to next trading day (virtual mode) or simply reset target.
	- loss limit from config => when hit, close position immediately.
	- Order clicks:
	    * No position + trigger => single click Buy/Sell
	    * Opposite position + opposite trigger => double click opposite side (flip)
	  (Same as your Sikuli logic.)
*/

// =========================
// Config & Globals
// =========================

type Config struct {
	ProfitTarget         int    `json:"profitTarget"`
	LossLimit            int    `json:"lossLimit"`
	TradeSymbol          string `json:"tradeSymbol"`
	DayStartTime         string `json:"dayStartTime"`
	HighTradeStop        bool   `json:"highTradeStop"`
	OCRWhitelistTD       string `json:"ocrWhitelistTD"`
	ImageRoot            string // set at runtime
	Debug                bool   // internal use
	PNLOffsetFastX       int    // Added for configurable offsets
	PNLOffsetFastY       int
	PNLOffsetSlowX       int
	PNLOffsetSlowY       int
	PNLWidthExtra        int
	PNLHeightAdjust      int
	TradingDayOffsetX    int
	TradingDayOffsetY    int
	TradingDayWidthExtra int
	TradingDayHeightAdj  int
}

type Region struct {
	X int
	Y int
	W int
	H int
}

func (r Region) Rect() image.Rectangle { return image.Rect(r.X, r.Y, r.X+r.W, r.Y+r.H) }
func (r Region) Grow(px int) Region    { return Region{r.X - px, r.Y - px, r.W + 2*px, r.H + 2*px} }

type Action struct {
	Path   string
	Region *Region
	Tmpl   gocv.Mat // loaded template
}

var (
	cfg = Config{
		Debug:                false,
		HighTradeStop:        true,
		OCRWhitelistTD:       "0123456789:,",
		PNLOffsetFastX:       35, // Default offsets; can be overridden in config.json
		PNLOffsetFastY:       8,
		PNLOffsetSlowX:       35,
		PNLOffsetSlowY:       -15,
		PNLWidthExtra:        22,
		PNLHeightAdjust:      -10,
		TradingDayOffsetX:    119,
		TradingDayOffsetY:    5,
		TradingDayWidthExtra: 20,
		TradingDayHeightAdj:  -10,
	}

	actions = map[string]*Action{
		"buy":           {Path: "Images/Action/buy.png"},
		"sell":          {Path: "Images/Action/sell.png"},
		"long":          {Path: "Images/Routing/long.png"},
		"short":         {Path: "Images/Routing/short.png"},
		"nomove":        {Path: "Images/Routing/nomove.png"},
		"close":         {Path: "Images/Routing/close_pos.png"},
		"placems":       {Path: "Images/Routing/placemouse.png"},
		"noposition":    {Path: "Images/Position/noposition.png"},
		"longposition":  {Path: "Images/Position/longposition.png"},
		"shortposition": {Path: "Images/Position/shortposition.png"},
		"virtual":       {Path: "Images/Account/virtual_account.png"},
		"live":          {Path: "Images/Account/live_account.png"},
		"paper":         {Path: "Images/Account/paper_account.png"},
		"profitx":       {Path: "Images/Profit/plday.png"},
		"change_symbol": {Path: "Images/Action/change_symbol.png"},
		"rumble":        {Path: "Images/Rumble/followtrend.png"},
		"time_refresh":  {Path: "Images/TradingDay/timerefresh.png"},
		"go_refresh":    {Path: "Images/TradingDay/gorefresh.png"},
	}

	stateMu             sync.Mutex
	lastTradeTimeMu     sync.Mutex
	lastTradeTime       time.Time
	pnlOCRHistoryMu     sync.Mutex
	pnlOCRHistory       []int // Combined history for PNL (profit/loss)
	historySize         = 3
	year, month, day    int
	stopCtx, stopCancel = context.WithCancel(context.Background())
	ocrMu               sync.Mutex
	ocr                 *gosseract.Client
	templateMinScore    float32  = 0.90
	refreshInterval              = 5 * time.Second // Less aggressive refresh
	scanInterval                 = 500 * time.Millisecond
	pnlInterval                  = 500 * time.Millisecond
	fullScreenCache     gocv.Mat // Cached full screen for efficiency
	fullScreenCacheTime time.Time
	fullScreenCacheMu   sync.Mutex
	state               = struct {
		Trade        string
		Position     string
		CurrTrade    string
		CurrPos      string
		TradeHigh    int
		Day          string
		Month        string
		Year         string
		AccountType  string
		PNL          int
		PNLFast      int
		TargetProfit int
	}{}
)

// =========================
// Utility
// =========================

func must(err error, msg string) {
	if err != nil {
		log.Fatalf("%s: %v", msg, err)
	}
}

func loadMat(path string) gocv.Mat {
	mat := gocv.IMRead(path, gocv.IMReadColor)
	if mat.Empty() {
		log.Fatalf("failed to load image: %s", path)
	}
	return mat
}

func getFullScreen() gocv.Mat {
	fullScreenCacheMu.Lock()
	defer fullScreenCacheMu.Unlock()
	if time.Since(fullScreenCacheTime) < time.Second && !fullScreenCache.Empty() {
		return fullScreenCache.Clone()
	}
	b := screenshot.GetDisplayBounds(0)
	full := Region{b.Min.X, b.Min.Y, b.Dx(), b.Dy()}
	mat := screenshotRegion(full)
	fullScreenCache = mat.Clone()
	fullScreenCacheTime = time.Now()
	return mat
}

func screenshotRegion(r Region) gocv.Mat {
	img, err := screenshot.CaptureRect(r.Rect())
	if err != nil {
		log.Printf("screenshot error: %v", err)
		return gocv.NewMat()
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		log.Printf("png encode error: %v", err)
		return gocv.NewMat()
	}
	mat, err := gocv.IMDecode(buf.Bytes(), gocv.IMReadColor)
	if err != nil {
		log.Printf("mat decode error: %v", err)
	}
	return mat
}

func loadConfig(path string) Config {
	data, err := os.ReadFile(path)
	must(err, "read config.json")
	var c Config
	must(json.Unmarshal(data, &c), "parse config.json")
	if c.LossLimit > 0 {
		c.LossLimit = -c.LossLimit
	}
	if c.DayStartTime == "" {
		c.DayStartTime = "0630"
	}
	if c.OCRWhitelistTD == "" {
		c.OCRWhitelistTD = "0123456789:,"
	}
	// Defaults for new fields if not in JSON
	if c.PNLOffsetFastX == 0 {
		c.PNLOffsetFastX = 35
	}
	// ... similarly for others
	return c
}

func findOnceOnScreen(tmpl gocv.Mat) (Region, bool) {
	src := getFullScreen()
	defer src.Close()
	if src.Empty() {
		return Region{}, false
	}
	res := gocv.NewMatWithSize(src.Rows()-tmpl.Rows()+1, src.Cols()-tmpl.Cols()+1, gocv.MatTypeCV32F)
	defer res.Close()
	mask := gocv.NewMat()
	defer mask.Close()
	gocv.MatchTemplate(src, tmpl, &res, gocv.TmCcoeffNormed, mask)
	_, maxVal, _, maxLoc := gocv.MinMaxLoc(res)
	if maxVal >= templateMinScore {
		b := screenshot.GetDisplayBounds(0)
		return Region{X: b.Min.X + maxLoc.X, Y: b.Min.Y + maxLoc.Y, W: tmpl.Cols(), H: tmpl.Rows()}, true
	}
	return Region{}, false
}

func existsInRegion(tmpl gocv.Mat, r Region) bool {
	src := screenshotRegion(r)
	if src.Empty() {
		return false
	}
	defer src.Close()
	res := gocv.NewMatWithSize(src.Rows()-tmpl.Rows()+1, src.Cols()-tmpl.Cols()+1, gocv.MatTypeCV32F)
	defer res.Close()
	mask := gocv.NewMat()
	defer mask.Close()
	gocv.MatchTemplate(src, tmpl, &res, gocv.TmCcoeffNormed, mask)
	_, maxVal, _, _ := gocv.MinMaxLoc(res)
	return maxVal >= templateMinScore
}

func initOCR() {
	ocrMu.Lock()
	defer ocrMu.Unlock()
	if ocr != nil {
		return
	}
	c := gosseract.NewClient()
	if err := c.SetLanguage("eng"); err != nil {
		log.Printf("OCR set language error: %v", err)
	}
	if err := c.SetPageSegMode(gosseract.PSM_SINGLE_LINE); err != nil {
		log.Printf("OCR set PSM error: %v", err)
	}
	ocr = c
}

// =========================
// Regions bootstrap
// =========================

func setRegionFor(key string) {
	a := actions[key]
	if a == nil || a.Region != nil {
		return
	}
	if a.Tmpl.Empty() {
		a.Tmpl = loadMat(filepath.Join(cfg.ImageRoot, a.Path))
	}
	if region, ok := findOnceOnScreen(a.Tmpl); ok {
		r := region.Grow(10)
		switch key {
		case "noposition", "longposition", "shortposition":
			actions["noposition"].Region = &r
			actions["longposition"].Region = &r
			actions["shortposition"].Region = &r
		case "long", "short", "nomove", "close":
			actions["long"].Region = &r
			actions["short"].Region = &r
			actions["nomove"].Region = &r
			actions["close"].Region = &r
		case "virtual", "live", "paper":
			actions["virtual"].Region = &r
			actions["live"].Region = &r
			actions["paper"].Region = &r
		default:
			a.Region = &r
		}
	}
}

func scanForTriggersBootstrap() {
	var wg sync.WaitGroup
	for k := range actions {
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			setRegionFor(k)
		}(k)
	}
	wg.Wait()
}

// =========================
// Account type
// =========================

func detectAccountType() string {
	if actions["virtual"].Region != nil && existsInRegion(actions["virtual"].Tmpl, *actions["virtual"].Region) {
		return "virtual"
	}
	if actions["live"].Region != nil && existsInRegion(actions["live"].Tmpl, *actions["live"].Region) {
		return "live"
	}
	if actions["paper"].Region != nil && existsInRegion(actions["paper"].Tmpl, *actions["paper"].Region) {
		return "paper"
	}
	return "unknown"
}

// =========================
// Symbol refresh
// =========================

func refreshSymbolBox() {
	cs := actions["change_symbol"]
	if cs.Region == nil {
		return
	}
	x := cs.Region.X + cs.Region.W/2
	y := cs.Region.Y + cs.Region.H + 5
	robotgo.MoveMouse(x, y)
	robotgo.MouseClick("left", false)
	time.Sleep(100 * time.Millisecond)
	robotgo.KeyTap("right")
	robotgo.TypeStr("F")
	robotgo.KeyTap("enter")
	time.Sleep(100 * time.Millisecond)
	robotgo.KeyTap("right")
	robotgo.KeyTap("backspace")
	robotgo.KeyTap("enter")
}

func waitForVisibility(tmplPath string, threshold float32, interval time.Duration, maxAttempts int) error {
	tmpl := loadMat(filepath.Join(cfg.ImageRoot, tmplPath))
	defer tmpl.Close()
	for i := 0; i < maxAttempts; i++ {
		b := screenshot.GetDisplayBounds(0)
		screen, err := captureToMat(b)
		if err != nil {
			return err
		}
		found, _ := findTemplateGray(screen, tmpl, threshold)
		screen.Close()
		if found {
			return nil
		}
		if i >= 4 { // After 5 attempts, force symbol reload
			cs := actions["change_symbol"]
			if cs.Region != nil {
				x := cs.Region.X + cs.Region.W/2
				y := cs.Region.Y + cs.Region.H + 5
				robotgo.MoveMouse(x, y)
				robotgo.MouseClick("left", false)
				time.Sleep(100 * time.Millisecond)
				robotgo.KeyTap("a", "ctrl")
				robotgo.TypeStr(cfg.TradeSymbol)
				robotgo.KeyTap("enter")
			}
		}
		time.Sleep(interval)
	}
	return fmt.Errorf("visibility timeout for %s", tmplPath)
}

func findTemplateGray(region gocv.Mat, tmpl gocv.Mat, threshold float32) (bool, image.Rectangle) {
	if region.Empty() || tmpl.Empty() {
		return false, image.Rectangle{}
	}
	regionGray := gocv.NewMat()
	tmplGray := gocv.NewMat()
	defer regionGray.Close()
	defer tmplGray.Close()
	gocv.CvtColor(region, &regionGray, gocv.ColorBGRToGray)
	gocv.CvtColor(tmpl, &tmplGray, gocv.ColorBGRToGray)
	result := gocv.NewMat()
	defer result.Close()
	gocv.MatchTemplate(regionGray, tmplGray, &result, gocv.TmCcoeffNormed, gocv.NewMat())
	_, maxVal, _, maxLoc := gocv.MinMaxLoc(result)
	if maxVal >= threshold {
		return true, image.Rect(maxLoc.X, maxLoc.Y, maxLoc.X+tmpl.Cols(), maxLoc.Y+tmpl.Rows())
	}
	return false, image.Rectangle{}
}

func captureToMat(r image.Rectangle) (gocv.Mat, error) {
	time.Sleep(50 * time.Millisecond)
	img, err := screenshot.CaptureRect(r)
	if err != nil {
		return gocv.NewMat(), err
	}
	return gocv.ImageToMatRGB(img)
}

// =========================
// OCR helpers
// =========================

func mean(vals []int) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0
	for _, v := range vals {
		sum += v
	}
	return float64(sum) / float64(len(vals))
}

func cleanForOCR(img gocv.Mat) gocv.Mat {
	gray := gocv.NewMat()
	gocv.CvtColor(img, &gray, gocv.ColorBGRToGray)
	thresh := gocv.NewMat()
	gocv.Threshold(gray, &thresh, 0, 255, gocv.ThresholdBinary|gocv.ThresholdOtsu)
	gray.Close()
	gocv.MedianBlur(thresh, &thresh, 3)
	kernel := gocv.GetStructuringElement(gocv.MorphRect, image.Pt(2, 2))
	gocv.MorphologyEx(thresh, &thresh, gocv.MorphClose, kernel)
	kernel.Close()
	enlarged := gocv.NewMat()
	gocv.Resize(thresh, &enlarged, image.Pt(thresh.Cols()*3, thresh.Rows()*3), 0, 0, gocv.InterpolationNearestNeighbor)
	thresh.Close()
	return enlarged
}

func ocrPNLFromRegion(r Region, isProfit bool) (int, error) {
	img := screenshotRegion(r)
	if img.Empty() {
		return 0, fmt.Errorf("empty image for OCR")
	}
	defer img.Close()
	clean := cleanForOCR(img)
	defer clean.Close()
	var buf bytes.Buffer
	if err := gocv.IMEncode(".png", clean, &buf); err != nil {
		return 0, err
	}
	ocrMu.Lock()
	defer ocrMu.Unlock()
	if err := ocr.SetLanguage("eng"); err != nil {
		return 0, err
	}
	if err := ocr.SetPageSegMode(gosseract.PSM_SINGLE_WORD); err != nil {
		return 0, err
	}
	if err := ocr.SetVariable("tessedit_char_whitelist", "0123456789()-.,"); err != nil {
		return 0, err
	}
	if err := ocr.SetVariable("user_defined_dpi", "300"); err != nil {
		return 0, err
	}
	if err := ocr.SetImageFromBytes(buf.Bytes()); err != nil {
		return 0, err
	}
	text, err := ocr.Text()
	if err != nil {
		return 0, err
	}
	text = strings.TrimSpace(text)
	if strings.Contains(text, "(") {
		text = "-" + strings.Trim(text, "()")
	}
	val, err := strconv.Atoi(strings.Map(func(r rune) rune {
		if r == '-' || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, strings.ReplaceAll(text, ",", "")))
	if err != nil {
		return 0, err
	}
	pnlOCRHistoryMu.Lock()
	avg := mean(pnlOCRHistory)
	if avg > 0 && float64(val) > avg*5 {
		if len(pnlOCRHistory) > 0 {
			val = pnlOCRHistory[len(pnlOCRHistory)-1]
		}
	} else {
		pnlOCRHistory = append(pnlOCRHistory, val)
		if len(pnlOCRHistory) > historySize {
			pnlOCRHistory = pnlOCRHistory[1:]
		}
	}
	pnlOCRHistoryMu.Unlock()
	return val, nil
}

func readTradingDayFromHUD() (int, int, int) {
	anchor := actions["rumble"]
	if anchor == nil || anchor.Region == nil {
		return 0, 0, 0
	}
	r := Region{
		X: anchor.Region.X + cfg.TradingDayOffsetX,
		Y: anchor.Region.Y + cfg.TradingDayOffsetY,
		W: anchor.Region.W + cfg.TradingDayWidthExtra,
		H: anchor.Region.H + cfg.TradingDayHeightAdj,
	}
	img := screenshotRegion(r)
	if img.Empty() {
		return 0, 0, 0
	}
	defer img.Close()
	gray := gocv.NewMat()
	defer gray.Close()
	gocv.CvtColor(img, &gray, gocv.ColorBGRToGray)
	thresh := gocv.NewMat()
	defer thresh.Close()
	gocv.Threshold(gray, &thresh, 0, 255, gocv.ThresholdBinary|gocv.ThresholdOtsu)
	enlarged := gocv.NewMat()
	defer enlarged.Close()
	gocv.Resize(thresh, &enlarged, image.Pt(thresh.Cols()*3, thresh.Rows()*3), 0, 0, gocv.InterpolationLinear)
	var buf bytes.Buffer
	if err := gocv.IMEncode(".png", enlarged, &buf); err != nil {
		return 0, 0, 0
	}
	ocrMu.Lock()
	defer ocrMu.Unlock()
	if err := ocr.SetLanguage("eng"); err != nil {
		return 0, 0, 0
	}
	if err := ocr.SetPageSegMode(gosseract.PSM_SINGLE_WORD); err != nil {
		return 0, 0, 0
	}
	if err := ocr.SetVariable("tessedit_char_whitelist", "1234567890:"); err != nil {
		return 0, 0, 0
	}
	if err := ocr.SetVariable("user_defined_dpi", "300"); err != nil {
		return 0, 0, 0
	}
	if err := ocr.SetImageFromBytes(buf.Bytes()); err != nil {
		return 0, 0, 0
	}
	text, err := ocr.Text()
	if err != nil {
		return 0, 0, 0
	}
	text = strings.TrimSpace(strings.ReplaceAll(text, " ", ""))
	parts := strings.Split(text, ":")
	if len(parts) < 3 {
		return 0, 0, 0
	}
	yr := parts[0]
	if len(yr) > 4 {
		yr = yr[len(yr)-4:]
	}
	y, _ := strconv.Atoi(yr)
	m, _ := strconv.Atoi(parts[1])
	d, _ := strconv.Atoi(parts[2])
	return y, m, d
}

// =========================
// Trading logic
// =========================

func scanOne(kind string) {
	var list []string
	if kind == "position" {
		list = []string{"noposition", "longposition", "shortposition"}
	} else {
		list = []string{"long", "short", "close", "nomove"}
	}
	for _, name := range list {
		a := actions[name]
		if a == nil || a.Region == nil || a.Tmpl.Empty() {
			continue
		}
		if existsInRegion(a.Tmpl, *a.Region) {
			stateMu.Lock()
			if kind == "position" {
				state.Position = name
				state.CurrPos = name
			} else {
				state.Trade = name
				state.CurrTrade = name
			}
			stateMu.Unlock()
			return
		}
	}
}

func clickAction(name string) {
	a := actions[name]
	if a == nil || a.Region == nil {
		return
	}
	x := a.Region.X + a.Region.W/2
	y := a.Region.Y + a.Region.H/2
	robotgo.MoveMouseSmooth(x, y, 0.6, 0.2)
	time.Sleep(60 * time.Millisecond)
	robotgo.MouseClick("left", false)
}

func trade(action, position string, reason string) {
	lastTradeTimeMu.Lock()
	if time.Since(lastTradeTime) < 3*time.Second {
		lastTradeTimeMu.Unlock()
		return
	}
	lastTradeTime = time.Now()
	lastTradeTimeMu.Unlock()
	if action == "nomove" {
		return
	}
	resetPNL := func() {
		stateMu.Lock()
		state.TradeHigh = 0
		state.PNL = 0
		state.PNLFast = 0
		stateMu.Unlock()
	}
	wait := func() { time.Sleep(2 * time.Second) }
	switch {
	case action == "long" && position == "noposition":
		clickAction("buy")
		stateMu.Lock()
		state.Position = "longposition"
		state.CurrPos = "longposition"
		stateMu.Unlock()
		resetPNL()
		wait()
	case action == "short" && position == "noposition":
		clickAction("sell")
		stateMu.Lock()
		state.Position = "shortposition"
		state.CurrPos = "shortposition"
		stateMu.Unlock()
		resetPNL()
		wait()
	case action == "long" && position == "shortposition":
		clickAction("buy")
		clickAction("buy")
		stateMu.Lock()
		state.Position = "longposition"
		state.CurrPos = "longposition"
		stateMu.Unlock()
		resetPNL()
		wait()
	case action == "short" && position == "longposition":
		clickAction("sell")
		clickAction("sell")
		stateMu.Lock()
		state.Position = "shortposition"
		state.CurrPos = "shortposition"
		stateMu.Unlock()
		resetPNL()
		wait()
	case action == "close":
		if position == "longposition" {
			clickAction("sell")
		} else if position == "shortposition" {
			clickAction("buy")
		}
		stateMu.Lock()
		state.Position = "noposition"
		state.CurrPos = "noposition"
		stateMu.Unlock()
		resetPNL()
		wait()
	}
}

func profitTaking(pnl, target int) bool {
	return pnl >= target && target > 0
}

func lossTaking(pnl, maxLoss int) bool {
	return pnl <= maxLoss
}

func shortTradeProfitGate(tradeHigh, pnl int) bool {
	if !cfg.HighTradeStop {
		return false
	}
	p := cfg.ProfitTarget
	if tradeHigh >= int(float64(p)*0.75) && pnl <= tradeHigh-200 && pnl >= (p/2+20) {
		return true
	}
	return false
}

// =========================
// Loops
// =========================

func pnlLoop(ctx context.Context) {
	anchor := actions["profitx"]
	if anchor == nil || anchor.Region == nil {
		log.Println("PNL anchor not found")
		return
	}
	rFast := Region{
		X: anchor.Region.X + cfg.PNLOffsetFastX,
		Y: anchor.Region.Y + cfg.PNLOffsetFastY,
		W: anchor.Region.W + cfg.PNLWidthExtra,
		H: anchor.Region.H + cfg.PNLHeightAdjust,
	}
	rSlow := Region{
		X: anchor.Region.X + cfg.PNLOffsetSlowX,
		Y: anchor.Region.Y + cfg.PNLOffsetSlowY,
		W: anchor.Region.W + cfg.PNLWidthExtra,
		H: anchor.Region.H + cfg.PNLHeightAdjust,
	}
	t := time.NewTicker(pnlInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p0, err := ocrPNLFromRegion(rFast, true)
			if err != nil {
				continue
			}
			p1, err := ocrPNLFromRegion(rSlow, false)
			if err != nil {
				continue
			}
			stateMu.Lock()
			state.PNL = p0
			state.PNLFast = p1
			if cfg.HighTradeStop && p0 > state.TradeHigh {
				state.TradeHigh = p0
			}
			stateMu.Unlock()
		}
	}
}

func scanLoop(ctx context.Context) {
	t := time.NewTicker(scanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := waitForVisibility(actions["rumble"].Path, 0.80, 500*time.Millisecond, 10); err != nil {
				log.Printf("waitForVisibility error: %v", err)
				continue
			}
			scanOne("position")
			scanOne("trade")
		}
	}
}

func refreshLoop(ctx context.Context) {
	t := time.NewTicker(refreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refreshSymbolBox()
		}
	}
}

// =========================
// Day handling
// =========================

func nextTradingDayVirtual() {
	clickAction("time_refresh")
	time.Sleep(300 * time.Millisecond)
	clickAction("go_refresh")
	robotgo.KeyTap("a", "ctrl")
	time.Sleep(100 * time.Millisecond)
	robotgo.TypeStr(cfg.DayStartTime)
	time.Sleep(200 * time.Millisecond)
	robotgo.MoveMouse(125, 0) // Relative move; may need tuning
	robotgo.MouseClick("left", false)
	time.Sleep(1 * time.Second)
}

// =========================
// HUD / Status
// =========================

func clearScreen() {
	cmd := exec.Command("clear")
	cmd.Stdout = os.Stdout
	_ = cmd.Run()
}

func statusTick() {
	clearScreen()
	stateMu.Lock()
	defer stateMu.Unlock()
	fmt.Println("-------------------------------")
	fmt.Printf("Account Type: %s\n", state.AccountType)
	fmt.Println("-------------------------------")
	fmt.Printf("Current Trade Date: %d %d, %d\n", month, day, year)
	fmt.Printf("SYMBOL: %s\n", cfg.TradeSymbol)
	fmt.Printf("PROFIT AMOUNT: %d\n", cfg.ProfitTarget)
	fmt.Printf("LOSS AMOUNT: %d\n", cfg.LossLimit)
	fmt.Println("-------------------------------")
	fmt.Printf("Curr Trade Action: %s\n", state.CurrTrade)
	fmt.Printf("Curr Position:     %s\n", state.CurrPos)
	fmt.Printf("Curr P/L:          %d\n", state.PNL)
	fmt.Printf("Trade High P/L:    %d\n", state.TradeHigh)
	fmt.Printf("Target Profit:     %d\n", state.TargetProfit)
	fmt.Println("-------------------------------")
}

func computeNextTarget(curr int) int {
	p := cfg.ProfitTarget
	if p <= 0 {
		p = 250
	}
	steps := curr / p
	return steps*p + p + 20
}

// =========================
// Main
// =========================

func main() {
	defer func() {
		if !fullScreenCache.Empty() {
			fullScreenCache.Close()
		}
		for _, a := range actions {
			if !a.Tmpl.Empty() {
				a.Tmpl.Close()
			}
		}
	}()
	cwd, _ := os.Getwd()
	cfg.ImageRoot = cwd
	cfg = loadConfig(filepath.Join(cwd, "config.json"))
	initOCR()
	scanForTriggersBootstrap()
	if actions["nomove"].Region == nil {
		log.Fatalf("Failed to locate runtime UI")
	}
	state.AccountType = detectAccountType()
	year, month, day = readTradingDayFromHUD()
	state.TargetProfit = cfg.ProfitTarget + 20
	go scanLoop(stopCtx)
	go pnlLoop(stopCtx)
	go refreshLoop(stopCtx)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stopCtx.Done():
			return
		case <-ticker.C:
			stateMu.Lock()
			trig := state.Trade
			pos := state.Position
			pnl := state.PNL
			high := state.TradeHigh
			target := state.TargetProfit
			if trig == state.CurrTrade {
				trig = ""
			} else if trig != "" {
				state.CurrTrade = trig
			}
			state.Trade = ""
			stateMu.Unlock()
			if profitTaking(pnl, target) {
				trade("close", pos, "profit")
				stateMu.Lock()
				nextT := computeNextTarget(pnl)
				if nextT > pnl*3 {
					nextT = pnl + cfg.ProfitTarget + 20
				}
				state.TargetProfit = nextT
				stateMu.Unlock()
				if state.AccountType == "virtual" {
					nextTradingDayVirtual()
					time.Sleep(5 * time.Second)
					year, month, day = readTradingDayFromHUD()
				}
				statusTick()
				continue
			}
			if lossTaking(pnl, cfg.LossLimit) && pos != "noposition" {
				trade("close", pos, "loss")
				statusTick()
				continue
			}
			if shortTradeProfitGate(high, pnl) && pos != "noposition" {
				trade("close", pos, "giveback")
				statusTick()
				continue
			}
			if trig == "" {
				trig = state.CurrTrade
			}
			if trig != "" && trig != "nomove" {
				if (trig == "long" && pos == "noposition") ||
					(trig == "short" && pos == "noposition") ||
					(trig == "long" && pos == "shortposition") ||
					(trig == "short" && pos == "longposition") {
					trade(trig, pos, "")
				}
			}
			statusTick()
		}
	}
}
