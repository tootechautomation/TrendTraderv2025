//go:build linux
// +build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
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

//type Config struct {
//	Debug          bool
//	SymbolLive     string
//	ProfitAmount   int // e.g., 250
//	LossAmount     int // negative (we convert if positive)
//	DayStartTime   string
//	HighTradeStop  bool
//	ImageRoot      string // root_directory
//	OCRWhitelistTD string // whitelist for trading day OCR
//}

type Config struct {
	ProfitTarget   int    `json:"profitTarget"`
	LossLimit      int    `json:"lossLimit"`
	TradeSymbol    string `json:"tradeSymbol"`
	DayStartTime   string `json:"dayStartTime"`
	HighTradeStop  bool
	OCRWhitelistTD string
	ImageRoot      string // set at runtime
	Debug          bool   // internal use
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

// SymbolLive:     "MESZ25",
// ProfitAmount:   250,
// LossAmount:     -300, // make negative if not already
var lastTradeTime time.Time

var (
	cfg = Config{
		Debug:          false,
		DayStartTime:   "0630",
		HighTradeStop:  true,
		ImageRoot:      "", // set at runtime to cwd
		OCRWhitelistTD: "0123456789:,",
	}

	// Images
	actions = map[string]*Action{
		// Action buttons
		"buy":  {Path: "Images/Action/buy.png"},
		"sell": {Path: "Images/Action/sell.png"},

		// Dynamic trade triggers (same region)
		"long":    {Path: "Images/Routing/long.png"},
		"short":   {Path: "Images/Routing/short.png"},
		"nomove":  {Path: "Images/Routing/nomove.png"},
		"close":   {Path: "Images/Routing/close_pos.png"},
		"placems": {Path: "Images/Routing/placemouse.png"},

		// Position indicator (same region)
		"noposition":    {Path: "Images/Position/noposition.png"},
		"longposition":  {Path: "Images/Position/longposition.png"},
		"shortposition": {Path: "Images/Position/shortposition.png"},

		// Account
		"virtual": {Path: "Images/Account/virtual_account.png"},
		"live":    {Path: "Images/Account/live_account.png"},
		"paper":   {Path: "Images/Account/paper_account.png"},

		// P&L anchor + OCR
		"profitx": {Path: "Images/Profit/plday.png"},

		// Symbol box + refresh affordances
		"change_symbol": {Path: "Images/Action/change_symbol.png"},
		"rumble":        {Path: "Images/Rumble/followtrend.png"},

		// Trading day controls
		"time_refresh": {Path: "Images/TradingDay/timerefresh.png"},
		"go_refresh":   {Path: "Images/TradingDay/gorefresh.png"},
	}

	// Trade state
	stateMu sync.Mutex
	state   = struct {
		Trade        string // long | short | close | nomove | ""
		Position     string // longposition | shortposition | noposition | ""
		CurrTrade    string
		CurrPos      string
		TradeHigh    int
		Day          string // OCR result (day of month as string)
		Month        string // OCR or manual
		Year         string // OCR or current year
		AccountType  string // virtual | live | paper
		PNL          int
		PNL_fast     int
		TargetProfit int // dynamic staircase target
	}{}

	// Control
	stopCtx, stopCancel = context.WithCancel(context.Background())

	// OCR client (gosseract)
	ocrMu sync.Mutex
	ocr   *gosseract.Client

	// General
	templateMinScore float32 = 0.90
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

func screenshotRegion(r Region) gocv.Mat {
	img, err := screenshot.CaptureRect(r.Rect())
	must(err, "screenshot")
	var buf bytes.Buffer
	must(png.Encode(&buf, img), "encode png")
	mat, err := gocv.IMDecode(buf.Bytes(), gocv.IMReadColor)
	must(err, "decode mat")
	return mat
}

func loadConfig(path string) Config {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("failed to read config.json: %v", err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		log.Fatalf("failed to parse config.json: %v", err)
	}

	// normalize values
	if c.LossLimit > 0 {
		c.LossLimit = -c.LossLimit
	}
	if c.DayStartTime == "" {
		c.DayStartTime = "0630"
	}
	if c.OCRWhitelistTD == "" {
		c.OCRWhitelistTD = "0123456789:,"
	}
	return c
}

func findOnceOnScreen(tmpl gocv.Mat) (Region, bool) {
	// scan full screen (primary monitor). You can extend to all monitors if needed.
	b := screenshot.GetDisplayBounds(0)
	full := Region{b.Min.X, b.Min.Y, b.Dx(), b.Dy()}
	src := screenshotRegion(full)
	defer src.Close()

	res := gocv.NewMatWithSize(src.Rows()-tmpl.Rows()+1, src.Cols()-tmpl.Cols()+1, gocv.MatTypeCV32F)
	defer res.Close()

	gocv.MatchTemplate(src, tmpl, &res, gocv.TmCcoeffNormed, gocv.NewMat())
	_, maxVal, _, maxLoc := gocv.MinMaxLoc(res)

	if maxVal >= templateMinScore {
		return Region{X: full.X + maxLoc.X, Y: full.Y + maxLoc.Y, W: tmpl.Cols(), H: tmpl.Rows()}, true
	}
	return Region{}, false
}

func existsInRegion(tmpl gocv.Mat, r Region) bool {
	src := screenshotRegion(r)
	defer src.Close()

	res := gocv.NewMatWithSize(src.Rows()-tmpl.Rows()+1, src.Cols()-tmpl.Cols()+1, gocv.MatTypeCV32F)
	defer res.Close()

	gocv.MatchTemplate(src, tmpl, &res, gocv.TmCcoeffNormed, gocv.NewMat())
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
	// Configure as you do in CLI:
	_ = c.SetLanguage("eng")
	_ = c.SetPageSegMode(gosseract.PSM_SINGLE_LINE)
	ocr = c
}

// =========================
// Regions bootstrap (like setRegion/scanForTriggers)
// =========================

func setRegionFor(key string) {
	a := actions[key]
	if a == nil {
		return
	}
	// already set?
	if a.Region != nil {
		return
	}
	// find image once on screen
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
			actions["long"].Region, actions["short"].Region = &r, &r
			actions["nomove"].Region, actions["close"].Region = &r, &r
		case "virtual", "live", "paper":
			actions["virtual"].Region, actions["live"].Region, actions["paper"].Region = &r, &r, &r
		default:
			actions[key].Region = &r
		}
	}
}

func scanForTriggersBootstrap() {
	wg := sync.WaitGroup{}
	for k := range actions {
		k := k
		wg.Add(1)
		go func() {
			defer wg.Done()
			setRegionFor(k)
		}()
	}
	wg.Wait()
}

// =========================
// Account type
// =========================

func detectAccountType() {
	if actions["virtual"].Region != nil && existsInRegion(actions["virtual"].Tmpl, *actions["virtual"].Region) {
		state.AccountType = "virtual"
		return
	}
	if actions["live"].Region != nil && existsInRegion(actions["live"].Tmpl, *actions["live"].Region) {
		state.AccountType = "live"
		return
	}
	if actions["paper"].Region != nil && existsInRegion(actions["paper"].Tmpl, *actions["paper"].Region) {
		state.AccountType = "paper"
		return
	}
	state.AccountType = "unknown"
}

func debugHighlight(r Region, label string) {
	img := screenshotRegion(r)
	gocv.Rectangle(&img, r.Rect(), color.RGBA{255, 0, 0, 0}, 2)
	gocv.PutText(&img, label, image.Pt(r.X, r.Y-5),
		gocv.FontHersheyPlain, 1.2, color.RGBA{0, 255, 0, 0}, 2)

	win := gocv.NewWindow("DEBUG")
	defer win.Close()
	win.IMShow(img)
	win.WaitKey(1000) // show 1 second
}

func showMouseCrosshair(x, y int) {
	b := screenshot.GetDisplayBounds(0)
	img := screenshotRegion(Region{b.Min.X, b.Min.Y, b.Dx(), b.Dy()})
	gocv.Line(&img, image.Pt(x-10, y), image.Pt(x+10, y), color.RGBA{0, 0, 255, 0}, 2)
	gocv.Line(&img, image.Pt(x, y-10), image.Pt(x, y+10), color.RGBA{0, 0, 255, 0}, 2)
	win := gocv.NewWindow("MouseCrosshair")
	defer win.Close()
	win.IMShow(img)
	win.WaitKey(1000)
}

// =========================
// Symbol refresh (like refreshSymbol in Sikuli)
// =========================

func refreshSymbolBox() {
	cs := actions["change_symbol"]
	if cs.Region == nil {
		return
	}
	// Click into the symbol field; send a small key ping that TOS uses to recalc the panes.
	x := cs.Region.X + cs.Region.W/2
	y := cs.Region.Y + cs.Region.H + 5
	//fmt.Printf("DEBUG: moving mouse to (%d,%d)\n", x, y)
	//robotgo.MoveMouseSmooth(x, y, 0.9, 0.5) // slower + visible
	//debugHighlight(*cs.Region, "change_symbol")
	//showMouseCrosshair(x, y)
	robotgo.MoveMouse(x, y)
	//robotgo.MoveMouse(cs.Region.X+cs.Region.W, cs.Region.Y+cs.Region.H+30)
	robotgo.MouseClick("left", false)
	time.Sleep(100 * time.Millisecond)

	// Your original does RIGHT + "F" + ENTER, then RIGHT + BACKSPACE + ENTER sequence.
	robotgo.KeyTap("right")
	robotgo.TypeStr("F")
	robotgo.KeyTap("enter")
	time.Sleep(100 * time.Millisecond)

	robotgo.KeyTap("right")
	robotgo.KeyTap("backspace")
	robotgo.KeyTap("enter")
}

func waitForVisibility(imgPath string, threshold float32, interval time.Duration) error {
	tmpl := gocv.IMRead(imgPath, gocv.IMReadColor)
	if tmpl.Empty() {
		return fmt.Errorf("failed to load image: %s", imgPath)
	}
	defer tmpl.Close()

	for i := 1; i <= 11; i++ {
		bounds := screenshot.GetDisplayBounds(0)
		screen, err := captureToMat(bounds)
		if err != nil {
			return fmt.Errorf("screenshot failed: %v", err)
		}

		found, _ := findTemplateGray(screen, tmpl, threshold)
		screen.Close()

		if found {
			return nil
		}

		if i >= 10 {
			cs := actions["change_symbol"]
			if cs.Region == nil {
				return nil
			}
			// Click into the symbol field; send a small key ping that TOS uses to recalc the panes.
			x := cs.Region.X + cs.Region.W/2
			y := cs.Region.Y + cs.Region.H + 5
			//fmt.Printf("DEBUG: moving mouse to (%d,%d)\n", x, y)
			//robotgo.MoveMouseSmooth(x, y, 0.9, 0.5) // slower + visible
			//debugHighlight(*cs.Region, "change_symbol")
			//showMouseCrosshair(x, y)
			robotgo.MoveMouse(x, y)
			//robotgo.MoveMouse(cs.Region.X+cs.Region.W, cs.Region.Y+cs.Region.H+30)
			robotgo.MouseClick("left", false)
			time.Sleep(100 * time.Millisecond)

			// Your original does RIGHT + "F" + ENTER, then RIGHT + BACKSPACE + ENTER sequence.
			robotgo.TypeStr(cfg.TradeSymbol + "F")
			robotgo.KeyTap("enter")
			time.Sleep(100 * time.Millisecond)
		}

		time.Sleep(interval)
	}
	return nil
}

// Grayscale-based template matcher (robust against color/BGR differences)
func findTemplateGray(region gocv.Mat, tmpl gocv.Mat, threshold float32) (bool, image.Rectangle) {
	if region.Empty() || tmpl.Empty() {
		return false, image.Rectangle{}
	}

	// Convert both to grayscale
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

	//fmt.Printf("🔎 [Gray] Match score %.2f\n", maxVal)

	if maxVal >= threshold {
		return true, image.Rect(maxLoc.X, maxLoc.Y, maxLoc.X+tmpl.Cols(), maxLoc.Y+tmpl.Rows())
	}
	return false, image.Rectangle{}
}

func captureToMat(r image.Rectangle) (gocv.Mat, error) {
	robotgo.MilliSleep(50)
	img, err := screenshot.CaptureRect(r)
	if err != nil {
		return gocv.Mat{}, err
	}
	mat, err := gocv.ImageToMatRGB(img)
	if err != nil {
		return gocv.Mat{}, err
	}
	return mat, nil
}

// =========================
// OCR helpers
// =========================

func ocrPNLFromRegion(r Region) (int, error) {
	img := screenshotRegion(r)
	defer img.Close()

	tmp := "/tmp/pnl_ocr.png"
	ok := gocv.IMWrite(tmp, img)
	if !ok {
		return 0, fmt.Errorf("failed to write temp png")
	}
	ocrMu.Lock()
	defer ocrMu.Unlock()
	_ = ocr.SetWhitelist("0123456789()-.,")

	_ = ocr.SetImage(tmp)
	text, err := ocr.Text()
	if err != nil {
		return 0, err
	}
	return parseIntLike(text), nil
}

func parseIntLike(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	// normalize: "(123)" => -123 ; remove commas; drop decimals.
	neg := strings.Contains(s, "(")
	s = strings.ReplaceAll(s, "(", "")
	s = strings.ReplaceAll(s, ")", "")
	s = strings.ReplaceAll(s, ",", "")

	// keep leading minus if any
	idx := strings.Index(s, ".")
	if idx != -1 {
		s = s[:idx]
	}
	//i := 0
	for _, ch := range s {
		if (ch >= '0' && ch <= '9') || ch == '-' {
			// ok
		} else {
			// strip non-digits, but keep leading '-' if present
			continue
		}
	}
	// last pass:
	s = strings.Map(func(r rune) rune {
		if r == '-' || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, s)
	if s == "" || s == "-" {
		return 0
	}
	val := 0
	fmt.Sscanf(s, "%d", &val)
	if neg && val > 0 {
		val = -val
	}
	return val
}

// =========================
// Trading day OCR (year:month:day from a small HUD region)
// =========================

func readTradingDayFromHUD(anchor *Action) (year, month, day string) {
	if anchor == nil || anchor.Region == nil {
		return "", "", ""
	}
	// Sample a small strip offset from anchor (tuned like your script)
	r := Region{
		X: anchor.Region.X + 119,
		Y: anchor.Region.Y + 5,
		W: anchor.Region.W + 20,
		H: anchor.Region.H - 10,
	}
	img := screenshotRegion(r)
	defer img.Close()

	tmp := "/tmp/td_ocr.png"
	_ = gocv.IMWrite(tmp, img)

	ocrMu.Lock()
	defer ocrMu.Unlock()
	_ = ocr.SetWhitelist(cfg.OCRWhitelistTD)
	_ = ocr.SetImage(tmp)
	text, err := ocr.Text()
	if err != nil || strings.TrimSpace(text) == "" {
		return "", "", ""
	}
	text = strings.TrimSpace(text)
	parts := strings.Split(strings.ReplaceAll(text, " ", ""), ":")
	if len(parts) < 3 {
		return "", "", ""
	}
	yr := parts[0]
	if len(yr) == 5 {
		yr = yr[1:]
	}
	return yr, parts[1], parts[2]
}

// =========================
// Trading logic
// =========================

func scanOne(kind string) {
	// kind: "position" or "trade"
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
			break
		}
	}
}

func clickAction(name string) {
	a := actions[name]
	if a == nil || a.Region == nil {
		return
	}
	// click center of known button region
	x := a.Region.X + a.Region.W/2
	y := a.Region.Y + a.Region.H/2
	robotgo.MoveMouseSmooth(x, y, 0.6, 0.2)
	time.Sleep(60 * time.Millisecond)
	robotgo.MouseClick("left", false)
}

/*
*

	func trade(action, position string, reason string) {
		// Enforce semantics:
		// - nomove => ignore
		if action == "nomove" {
			return
		}

		wait := func() { time.Sleep(2 * time.Second) }
		resetPNL := func() {
			stateMu.Lock()
			state.TradeHigh = 0
			state.PNL = 0
			state.PNL_fast = 0
			stateMu.Unlock()
		}

		switch {
		// enter new from flat
		case action == "long" && position == "noposition":
			clickAction("buy")
			stateMu.Lock()
			state.Position = "longposition"
			state.CurrPos = "longposition"
			state.TradeHigh = 0
			stateMu.Unlock()
			resetPNL()
			wait()
		case action == "short" && position == "noposition":
			clickAction("sell")
			stateMu.Lock()
			state.Position = "shortposition"
			state.CurrPos = "shortposition"
			state.TradeHigh = 0
			stateMu.Unlock()
			resetPNL()
			wait()

		// flip
		case action == "long" && position == "shortposition":
			clickAction("buy")
			clickAction("buy")
			stateMu.Lock()
			state.Position = "longposition"
			state.CurrPos = "longposition"
			state.TradeHigh = 0
			stateMu.Unlock()
			resetPNL()
			wait()
		case action == "short" && position == "longposition":
			clickAction("sell")
			clickAction("sell")
			stateMu.Lock()
			state.Position = "shortposition"
			state.CurrPos = "shortposition"
			state.TradeHigh = 0
			stateMu.Unlock()
			resetPNL()
			wait()

		// hold on same side (no clicks)
		case action == "long" && position == "longposition":
			// no-op
		case action == "short" && position == "shortposition":
			// no-op

		// close if holding
		case action == "close" && position == "longposition":
			clickAction("sell")
			stateMu.Lock()
			state.Position = "noposition"
			state.CurrPos = "noposition"
			state.TradeHigh = 0
			stateMu.Unlock()
			resetPNL()
			wait()
		case action == "close" && position == "shortposition":
			clickAction("buy")
			stateMu.Lock()
			state.Position = "noposition"
			state.CurrPos = "noposition"
			state.TradeHigh = 0
			stateMu.Unlock()
			resetPNL()
			wait()

		// close when already flat (mostly “market closed” flow)
		case action == "close" && position == "noposition":
			// no clicks; allow next-day workflow to proceed
			stateMu.Lock()
			state.TradeHigh = 0
			stateMu.Unlock()
			resetPNL()
			wait()
		}
	}

*
*/
func trade(action, position string, reason string) {
	// block if within 3s of last trade
	if time.Since(lastTradeTime) < 3*time.Second {
		return
	}

	// Enforce semantics:
	if action == "nomove" {
		return
	}

	wait := func() { time.Sleep(2 * time.Second) }
	resetPNL := func() {
		stateMu.Lock()
		state.TradeHigh = 0
		state.PNL = 0
		state.PNL_fast = 0
		stateMu.Unlock()
	}

	switch {
	case action == "long" && position == "noposition":
		clickAction("buy")
		stateMu.Lock()
		state.Position = "longposition"
		state.CurrPos = "longposition"
		state.TradeHigh = 0
		stateMu.Unlock()
		resetPNL()
		wait()
		lastTradeTime = time.Now() // ✅ mark trade time

	case action == "short" && position == "noposition":
		clickAction("sell")
		stateMu.Lock()
		state.Position = "shortposition"
		state.CurrPos = "shortposition"
		state.TradeHigh = 0
		stateMu.Unlock()
		resetPNL()
		wait()
		lastTradeTime = time.Now()

	case action == "long" && position == "shortposition":
		clickAction("buy")
		clickAction("buy")
		stateMu.Lock()
		state.Position = "longposition"
		state.CurrPos = "longposition"
		state.TradeHigh = 0
		stateMu.Unlock()
		resetPNL()
		wait()
		lastTradeTime = time.Now()

	case action == "short" && position == "longposition":
		clickAction("sell")
		clickAction("sell")
		stateMu.Lock()
		state.Position = "shortposition"
		state.CurrPos = "shortposition"
		state.TradeHigh = 0
		stateMu.Unlock()
		resetPNL()
		wait()
		lastTradeTime = time.Now()

		// (other close cases … also set lastTradeTime at the end)
	}
}

func profitTaking(pnl, target int) bool {
	return pnl >= target && target > 0
}

func lossTaking(pnl, maxLoss int) bool {
	// maxLoss should be negative (we coerce in main)
	if maxLoss >= 0 {
		maxLoss = -int(math.Abs(float64(maxLoss)))
	}
	return pnl <= maxLoss
}

func shortTradeProfitGate(tradeHigh, pnl int) bool {
	// Optional “give-back” logic similar to your script (high trade stop)
	// Example: if we’ve reached 0.75*ProfitAmount, then trip if pullback is >= 200, etc.
	if !cfg.HighTradeStop {
		return false
	}
	if tradeHigh >= int(float64(int(cfg.ProfitTarget))*0.75) {
		if pnl <= tradeHigh-200 && pnl >= (int(cfg.ProfitTarget)/2+20) {
			return true
		}
	}
	return false
}

// =========================
// PNL loop
// =========================

func pnlLoop(ctx context.Context) {
	// Define two OCR slices like your script (bottom and above area near plday anchor)
	anchor := actions["profitx"]
	if anchor == nil || anchor.Region == nil {
		return
	}
	// typical offsets – tune to your UI
	rFast := Region{X: anchor.Region.X + 25, Y: anchor.Region.Y + 8, W: anchor.Region.W + 32, H: anchor.Region.H - 10}
	rSlow := Region{X: anchor.Region.X + 25, Y: anchor.Region.Y - 15, W: anchor.Region.W + 32, H: anchor.Region.H - 10}

	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p0, _ := ocrPNLFromRegion(rFast)
			p1, _ := ocrPNLFromRegion(rSlow)
			stateMu.Lock()
			state.PNL = p0
			state.PNL_fast = p1
			if cfg.HighTradeStop && p0 > state.TradeHigh {
				state.TradeHigh = p0
			}
			stateMu.Unlock()
		}
	}
}

// =========================
// Trade scanner loop
// =========================

func scanLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			refreshSymbolBox()
			// Position first
			scanOne("position")
			// Then triggers
			scanOne("trade")
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// =========================
// Day handling (simplified)
// =========================

func nextTradingDayVirtual() {
	// Click the trading-day picker and select next weekday, then set time and apply.
	// This mirrors your Sikuli flow but keeps it minimal; wire your day-grid images if needed.
	clickAction("time_refresh")
	time.Sleep(300 * time.Millisecond)
	clickAction("go_refresh")
	// … then send keys to set cfg.DayStartTime etc.
	robotgo.KeyTap("a", "ctrl")
	time.Sleep(100 * time.Millisecond)
	robotgo.TypeStr(cfg.DayStartTime)
	time.Sleep(200 * time.Millisecond)
	robotgo.MoveMouse(125, 0)
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
	fmt.Printf("SYMBOL: %s\n", cfg.TradeSymbol)
	fmt.Printf("PROFIT AMOUNT: %d\n", int(cfg.ProfitTarget))
	fmt.Printf("LOSS AMOUNT: %d\n", int(cfg.LossLimit))
	fmt.Println("-------------------------------")
	fmt.Printf("Curr Trade Action: %s\n", state.CurrTrade)
	fmt.Printf("Curr Position:     %s\n", state.CurrPos)
	fmt.Printf("Curr P/L:          %d\n", state.PNL)
	fmt.Printf("Trade High P/L:    %d\n", state.TradeHigh)
	fmt.Printf("Target Profit:     %d\n", state.TargetProfit)
	fmt.Println("-------------------------------")
}

func computeNextTarget(curr int) int {
	// staircase target similar to your round_to_nearest_profit logic
	p := int(cfg.ProfitTarget)
	if p <= 0 {
		p = 250
	}
	steps := int(math.Round(float64(curr) / float64(p)))
	return steps*p + p + 20
}

// =========================
// Main control loop
// =========================

func main() {
	cwd, _ := os.Getwd()
	cfg = loadConfig("config.json")
	cfg.ImageRoot = cwd

	// Normalize loss to negative
	if int(cfg.LossLimit) > 0 {
		cfg.LossLimit = (cfg.LossLimit * -1)
	}

	// Preload templates
	for k, a := range actions {
		actions[k].Path = filepath.Join(cfg.ImageRoot, a.Path)
		actions[k].Tmpl = loadMat(actions[k].Path)
	}

	initOCR()
	scanForTriggersBootstrap()
	if actions["nomove"].Region == nil {
		log.Fatalf("Failed to locate runtime UI. Check ThinkOrSwim on-screen and your image assets.")
	}

	// Detect account
	detectAccountType()

	// Seed target
	stateMu.Lock()
	state.TargetProfit = int(cfg.ProfitTarget) + 20
	stateMu.Unlock()

	// Start loops
	go scanLoop(stopCtx)
	go pnlLoop(stopCtx)

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		if err := waitForVisibility("Images/Rumble/followtrend.png", 0.80, 500*time.Millisecond); err != nil {
			log.Fatal("❌ waitForVisibility error:", err)
		} else {
			select {
			case <-ticker.C:
				// pull snapshot
				stateMu.Lock()
				trig := state.Trade
				pos := state.Position
				pnl := state.PNL
				high := state.TradeHigh
				target := state.TargetProfit

				// ignore duplicate trigger (Option A)
				if trig == state.CurrTrade {
					trig = ""
				} else if trig != "" {
					state.CurrTrade = trig
				}

				state.Trade = "" // consume once
				stateMu.Unlock()

				//fmt.Printf("DEBUG: trig=%s, pos=%s, currTrade=%s\n", trig, pos, state.CurrTrade)

				// Decision logic ordering mirrors your script:
				// 1) Profit hit => close & (virtual) go next day; reset target
				if profitTaking(pnl, target) {
					trade("close", pos, "profit")
					stateMu.Lock()
					// next target uses staircase method (cap it sanely)
					state.TargetProfit = computeNextTarget(pnl)
					if state.TargetProfit > pnl*3 {
						state.TargetProfit = pnl + (int(cfg.ProfitTarget) + 20)
					}
					stateMu.Unlock()
					if state.AccountType == "virtual" {
						nextTradingDayVirtual()
					}
					//statusTick()
					continue
				}

				// 2) Loss stop => close immediately
				if lossTaking(pnl, int(cfg.LossLimit)) && pos != "noposition" {
					trade("close", pos, "loss")
					//statusTick()
					continue
				}

				// 3) Optional “short trade” profit give-back
				if shortTradeProfitGate(high, pnl) && pos != "noposition" {
					trade("close", pos, "giveback")
					//statusTick()
					continue
				}

				// 4) Fresh trigger (ignore nomove)
				if trig == "" {
					trig = state.CurrTrade
				}

				if trig != "" && trig != "nomove" {
					// trade only if position inconsistent
					if (trig == "long" && pos == "noposition") ||
						(trig == "short" && pos == "noposition") ||
						(trig == "long" && pos == "shortposition") ||
						(trig == "short" && pos == "longposition") {
						trade(trig, pos, "")
					}
				}

				//if trig != "" && trig != "nomove" {
				//	trade(trig, pos, "")
				//}

				//statusTick()
			}
		}
	}
}
