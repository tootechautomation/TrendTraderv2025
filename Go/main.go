//go:build ignore
// +build ignore

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/go-vgo/robotgo"
	"github.com/kbinani/screenshot"
	"github.com/otiai10/gosseract/v2"
	"gocv.io/x/gocv"
)

/* =========================
   Config & Globals
   ========================= */

type Config struct {
	ProfitTarget float64           `json:"profitTarget"`
	LossLimit    float64           `json:"lossLimit"`
	Templates    map[string]string `json:"templates"` // trigger images
	Positions    map[string]string `json:"positions"` // position images
	TradeSymbol  string            `json:"tradeSymbol"`
}

const (
	ColorReset  = "\033[0m"
	ColorRed    = "\033[31m"
	ColorGreen  = "\033[32m"
	ColorYellow = "\033[33m"
	ColorCyan   = "\033[36m"
	ColorWhite  = "\033[37m"
)

var (
	TRADE_YEAR        int
	TRADE_MONTH       int
	TRADE_DAY_START   int
	config            Config
	configLock        sync.RWMutex
	cachedRegion      image.Rectangle
	regionCached      bool
	buyRegion         image.Rectangle
	sellRegion        image.Rectangle
	templatesCV       map[string]gocv.Mat
	positionTemplates map[string]gocv.Mat
	triggerRegions    = map[string]image.Rectangle{}
)

type Trade struct {
	Position string
	Quantity int
	PnL      float64
	Closed   bool
}

var lastTradeTime time.Time
var tradeCooldown = 3 * time.Second // ⏳ 3-second cooldown
var activeTrade = Trade{Position: "NoMove"}
var desiredPosition = "NoMove"       // what we intend to hold after our next action
var waitingForConfirm bool           // true after we click, until on-screen matches desiredPosition
var confirmTimeout = 2 * time.Second // how long we allow for the UI to reflect the change
var confirmStarted time.Time

/* =========================
   Config load + watcher
   ========================= */

func loadConfig(path string) Config {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("❌ read config: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		log.Fatalf("❌ parse config: %v", err)
	}
	return cfg
}

func watchConfig(path string) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer w.Close()

	if err := w.Add(path); err != nil {
		log.Fatal(err)
	}

	for {
		select {
		case ev := <-w.Events:
			if ev.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				newCfg := loadConfig(path)
				configLock.Lock()
				config = newCfg
				configLock.Unlock()
				fmt.Printf("🔄 Config reloaded: ProfitTarget=%.2f LossLimit=%.2f\n",
					newCfg.ProfitTarget, newCfg.LossLimit)
			}
		case err := <-w.Errors:
			fmt.Println("⚠️ Config watcher error:", err)
		}
	}
}

/* =========================
   Screenshot & CV helpers
   ========================= */

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

func findTemplate(region gocv.Mat, tmpl gocv.Mat, threshold float32) (bool, image.Rectangle) {
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

	//fmt.Printf("🔎 Match score %.2f for template\n", maxVal)

	if maxVal >= threshold {
		return true, image.Rect(maxLoc.X, maxLoc.Y, maxLoc.X+tmpl.Cols(), maxLoc.Y+tmpl.Rows())
	}
	return false, image.Rectangle{}
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

func scanMultipleTemplates(region gocv.Mat, templates map[string]gocv.Mat, threshold float32) (string, image.Rectangle) {
	order := []string{"Long", "Short", "NoMove"}
	var bestName string
	var bestScore float32
	var bestRect image.Rectangle

	for _, name := range order {
		if tmpl, ok := templates[name]; ok {
			ok2, rect := findTemplateGray(region, tmpl, threshold)
			if ok2 {
				// Calculate score again for reporting
				result := gocv.NewMat()
				defer result.Close()
				gocv.MatchTemplate(region, tmpl, &result, gocv.TmCcoeffNormed, gocv.NewMat())
				_, maxVal, _, _ := gocv.MinMaxLoc(result)

				//fmt.Printf("Trigger %s (gray) match score = %.2f\n", name, maxVal)

				if float32(maxVal) > bestScore {
					bestScore = float32(maxVal)
					bestName = name
					bestRect = rect
				}
			}
		}
	}
	fmt.Printf("Best trigger=%s (score=%.2f)\n", bestName, bestScore)

	if bestScore >= threshold {
		return bestName, bestRect
	}
	return "", image.Rectangle{}
}

/* =========================
   Buy/Sell Button Caching
   ========================= */

func cacheActionButtons() error {
	bounds := screenshot.GetDisplayBounds(0)
	screen, err := screenshot.CaptureRect(bounds)
	if err != nil {
		return err
	}
	screenMat, _ := gocv.ImageToMatRGB(screen)
	defer screenMat.Close()

	buyMat := gocv.IMRead("Images/Action/buy.png", gocv.IMReadColor)
	sellMat := gocv.IMRead("Images/Action/sell.png", gocv.IMReadColor)
	if buyMat.Empty() || sellMat.Empty() {
		return fmt.Errorf("failed to load buy/sell images")
	}
	defer buyMat.Close()
	defer sellMat.Close()

	if ok, rect := findTemplate(screenMat, buyMat, 0.75); ok {
		buyRegion = rect
		fmt.Println("✅ Cached Buy button:", rect)
	} else {
		return fmt.Errorf("buy button not found")
	}
	if ok, rect := findTemplate(screenMat, sellMat, 0.75); ok {
		sellRegion = rect
		fmt.Println("✅ Cached Sell button:", rect)
	} else {
		return fmt.Errorf("sell button not found")
	}
	return nil
}

func cacheTriggerRegions() error {
	bounds := screenshot.GetDisplayBounds(0)
	screen, err := screenshot.CaptureRect(bounds)
	if err != nil {
		return err
	}
	screenMat, _ := gocv.ImageToMatRGB(screen)
	defer screenMat.Close()

	for name, tmpl := range templatesCV {
		if tmpl.Empty() {
			continue
		}
		if ok, rect := findTemplateGray(screenMat, tmpl, 0.80); ok {
			triggerRegions[name] = rect
			fmt.Printf("✅ Cached %s trigger region: %v\n", name, rect)
		} else {
			fmt.Printf("⚠️ Could not find %s trigger on screen\n", name)
		}
	}

	if len(triggerRegions) == 0 {
		return fmt.Errorf("no triggers cached")
	}
	return nil
}

func clickAt(rect image.Rectangle) {
	x := rect.Min.X + rect.Dx()/2
	y := rect.Min.Y + rect.Dy()/2
	robotgo.MoveMouse(x, y)
	time.Sleep(100 * time.Millisecond)
	robotgo.MouseClick("left", false)
	fmt.Printf("🖱️ Clicked at (%d,%d)\n", x, y)
}

func clickBuy() {
	if buyRegion.Empty() {
		fmt.Println("⚠️ Buy region empty, rescanning...")
		if err := cacheActionButtons(); err != nil {
			fmt.Println("❌ Cannot find Buy button:", err)
			return
		}
	}
	clickAt(buyRegion)
}

func clickSell() {
	if sellRegion.Empty() {
		fmt.Println("⚠️ Sell region empty, rescanning...")
		if err := cacheActionButtons(); err != nil {
			fmt.Println("❌ Cannot find Sell button:", err)
			return
		}
	}
	clickAt(sellRegion)
}

/* =========================
   OCR (for PnL/Qty)
   ========================= */

func preprocessAuto(img image.Image) ([]byte, error) {
	mat, _ := gocv.ImageToMatRGB(img)
	defer mat.Close()
	gray := gocv.NewMat()
	defer gray.Close()
	gocv.CvtColor(mat, &gray, gocv.ColorBGRToGray)
	bin := gocv.NewMat()
	defer bin.Close()
	gocv.Threshold(gray, &bin, 0, 255, gocv.ThresholdBinary|gocv.ThresholdOtsu)
	goimg, _ := bin.ToImage()
	var buf bytes.Buffer
	_ = png.Encode(&buf, goimg)
	return buf.Bytes(), nil
}

func runOCR(region image.Rectangle) (int, float64, error) {
	if region.Empty() {
		return 0, 0, fmt.Errorf("empty OCR region")
	}
	img, err := screenshot.CaptureRect(region)
	if err != nil {
		return 0, 0, err
	}
	processed, _ := preprocessAuto(img)
	client := gosseract.NewClient()
	defer client.Close()
	_ = client.SetImageFromBytes(processed)
	text, _ := client.Text()

	reQty := regexp.MustCompile(`\b\d+\b`)
	rePnL := regexp.MustCompile(`-?\d+(?:\.\d+)?`)
	qty := 0
	if m := reQty.FindString(text); m != "" {
		qty, _ = strconv.Atoi(m)
	}
	pnl := 0.0
	if m := rePnL.FindString(text); m != "" {
		pnl, _ = strconv.ParseFloat(strings.ReplaceAll(m, ",", ""), 64)
	}
	return qty, pnl, nil
}

/* =========================
   Position Detection
   ========================= */

func detectCurrentPosition() string {
	bounds := screenshot.GetDisplayBounds(0)
	screen, _ := screenshot.CaptureRect(bounds)
	screenMat, _ := gocv.ImageToMatRGB(screen)
	defer screenMat.Close()

	for name, tmpl := range positionTemplates {
		if tmpl.Empty() {
			continue
		}
		if ok, _ := findTemplateGray(screenMat, tmpl, 0.80); ok {
			n := strings.ToLower(name)
			switch {
			case strings.Contains(n, "short"):
				return "Short"
			case strings.Contains(n, "long"):
				return "Long"
			case strings.Contains(n, "nopos"), strings.Contains(n, "none"), strings.Contains(n, "flat"), strings.Contains(n, "nomove"):
				return "NoMove"
			}
		}
	}
	return "Unknown"
}

func detectTrigger() string {
	for name, rect := range triggerRegions {
		mat, err := captureToMat(rect)
		if err != nil {
			continue
		}
		defer mat.Close()

		tmpl := templatesCV[name]
		if ok, _ := findTemplateGray(mat, tmpl, 0.80); ok {
			return name
		}
	}
	return ""
}

/* =========================
   Trading Logic
   ========================= */

func determineTrade(trigger string, currentPos string, qty int, pnl float64) {
	// If we recently clicked, wait for screen to confirm — but only if trigger
	// still agrees with our desiredPosition. If it doesn’t, cancel wait.

	if waitingForConfirm {
		if currentPos == desiredPosition {
			activeTrade.Position = currentPos
			waitingForConfirm = false
			fmt.Println("✅ Position change confirmed on screen:", currentPos)
		} else if trigger != "" && trigger != desiredPosition {
			// Trigger disagrees with what we wanted — cancel and re-evaluate
			fmt.Println("⚠️ Trigger disagrees with desiredPosition; cancelling wait")
			waitingForConfirm = false
		} else if time.Since(confirmStarted) > confirmTimeout {
			fmt.Println("⏳ Confirmation timeout; will allow re-attempts")
			waitingForConfirm = false
		} else {
			fmt.Println("⏸ Waiting for on-screen confirmation; skipping this loop")
			return
		}
	}
	if !waitingForConfirm {
		detectedPos := detectCurrentPosition()
		activeTrade.Position = detectedPos
	}

	// Cooldown between distinct actions
	if time.Since(lastTradeTime) < tradeCooldown {
		fmt.Println("⏸ Cooldown active, skipping trade")
		return
	}

	if currentPos == "Unknown" {
		fmt.Println("❓ Position unknown — skipping trade this loop")
		return
	}

	// Map trigger to the position we want to hold after the action
	var target string
	switch trigger {
	case "Long":
		target = "Long"
	case "Short":
		target = "Short"
	case "closepos":
		target = "NoMove"
	default:
		// no-op trigger
		return
	}

	// If we already hold what the trigger wants, do nothing
	if currentPos == target {
		fmt.Println("✔ Already at desired position:", target)
		return
	}

	fmt.Printf("🔍 Position before trade: %s → target: %s\n", currentPos, target)

	switch target {
	case "Long":
		if currentPos == "NoMove" {
			clickBuy()
		} else if currentPos == "Short" {
			// need two clicks: first closes, second opens long
			clickBuy()
			time.Sleep(150 * time.Millisecond) // short delay so UI registers
			clickBuy()
		}
	case "Short":
		if currentPos == "NoMove" {
			clickSell()
		} else if currentPos == "Long" {
			// need two clicks: first closes, second opens short
			clickSell()
			time.Sleep(150 * time.Millisecond)
			clickSell()
		}
	case "NoMove":
		if currentPos == "Long" {
			clickSell()
		} else if currentPos == "Short" {
			clickBuy()
		}
	}

	// Start confirmation phase: do not click again until the on-screen badge matches
	desiredPosition = target
	waitingForConfirm = true
	confirmStarted = time.Now()
	lastTradeTime = time.Now()

	// Update trade bookkeeping (qty/pnl come from OCR)
	activeTrade.Quantity = qty
	activeTrade.PnL = pnl
	activeTrade.Closed = (target == "NoMove")

	fmt.Printf("determineTrade → Trigger=%s, CurrentPos=%s, Desired=%s, Waiting=%v\n",
		trigger, currentPos, desiredPosition, waitingForConfirm)
}

func initTradingDay() error {
	full := screenshot.GetDisplayBounds(0)
	screen, err := captureToMat(full)
	if err != nil {
		return fmt.Errorf("screenshot failed: %v", err)
	}
	defer screen.Close()

	tmpl := gocv.IMRead("Images/Rumble/followtrend.png", gocv.IMReadColor)
	if tmpl.Empty() {
		tmpl.Close()
		return fmt.Errorf("failed to load followtrend.png")
	}
	defer tmpl.Close()

	found, rect := findTemplateGray(screen, tmpl, 0.80)
	if !found {
		return fmt.Errorf("followtrend.png not found on screen")
	}

	// Apply offsets: (+119,+5,+20,-10)
	region := image.Rect(
		rect.Min.X+119,
		rect.Min.Y+5,
		rect.Max.X+20,
		rect.Max.Y-10,
	)

	// OCR that region
	img, err := screenshot.CaptureRect(region)
	if err != nil {
		return fmt.Errorf("capture failed: %v", err)
	}
	processed, _ := preprocessAuto(img)
	client := gosseract.NewClient()
	defer client.Close()
	_ = client.SetImageFromBytes(processed)
	text, _ := client.Text()

	// Clean and split
	clean := strings.TrimSpace(text)
	clean = strings.ReplaceAll(clean, " ", "")
	clean = strings.ReplaceAll(clean, ",", "") // remove commas from year
	parts := strings.Split(clean, ":")

	if len(parts) < 3 {
		return fmt.Errorf("unexpected trading day format: %q", clean)
	}

	// Parse Year
	year, err := strconv.Atoi(parts[0])
	if err != nil {
		return fmt.Errorf("invalid year: %v (%s)", err, parts[0])
	}

	// Parse Month
	month, err := strconv.Atoi(parts[1])
	if err != nil {
		return fmt.Errorf("invalid month: %v (%s)", err, parts[1])
	}

	// Parse Day
	day, err := strconv.Atoi(parts[2])
	if err != nil {
		return fmt.Errorf("invalid day: %v (%s)", err, parts[2])
	}

	TRADE_YEAR = year
	TRADE_MONTH = month
	TRADE_DAY_START = day

	fmt.Printf("📅 Initialized Trading Day: %04d-%02d-%02d\n", TRADE_YEAR, TRADE_MONTH, TRADE_DAY_START)
	return nil
}

func refreshSecurity(imgPath string, xOffset, yOffset int) error {
	full := screenshot.GetDisplayBounds(0)

	screen, err := captureToMat(full)
	if err != nil {
		return fmt.Errorf("screenshot failed: %v", err)
	}
	defer screen.Close()

	tmpl := gocv.IMRead(imgPath, gocv.IMReadColor)
	if tmpl.Empty() {
		tmpl.Close()
		return fmt.Errorf("failed to load template: %s", imgPath)
	}
	defer tmpl.Close()

	found, rect := findTemplate(screen, tmpl, 0.85)
	if !found {
		return fmt.Errorf("refresh image not found: %s", imgPath)
	}

	x := rect.Min.X + rect.Dx()/2 + xOffset
	y := rect.Min.Y + rect.Dy()/2 + yOffset

	robotgo.MoveMouse(x, y)
	time.Sleep(100 * time.Millisecond)
	robotgo.MouseClick("left", false)

	robotgo.TypeStr("N")
	time.Sleep(80 * time.Millisecond)
	robotgo.KeyTap("enter")

	robotgo.TypeStr(config.TradeSymbol)
	time.Sleep(80 * time.Millisecond)
	robotgo.KeyTap("enter")

	robotgo.MoveMouse((x - 40), y)
	time.Sleep(200 * time.Millisecond)
	return nil
}

func daysInMonth(year, month int) int {
	// Go trick: day 0 of next month = last day of current month
	t := time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.Local)
	return t.Day()
}

func nextTradingDay() error {
	year := TRADE_YEAR
	month := TRADE_MONTH
	day := TRADE_DAY_START + 1

	for {
		// rollover month
		if day > daysInMonth(year, month) {
			day = 1
			month++
			if month > 12 {
				month = 1
				year++
			}
		}

		if isMarketOpen(year, month, day) {
			TRADE_YEAR = year
			TRADE_MONTH = month
			TRADE_DAY_START = day
			fmt.Printf("📅 Next trading day: %04d-%02d-%02d\n", year, month, day)
			return nil
		}

		// try next day
		day++
	}
}

func isMarketOpen(year, month, day int) bool {
	date := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.Local)
	weekday := date.Weekday()

	if weekday == time.Saturday || weekday == time.Sunday {
		return false
	}

	// 2025 holidays
	stockMarketHolidays := map[[2]int]bool{
		{1, 1}:   true, // New Year's Day
		{1, 20}:  true, // Martin Luther King Jr. Day
		{2, 17}:  true, // Washington's Birthday
		{4, 18}:  true, // Good Friday
		{5, 26}:  true, // Memorial Day
		{6, 19}:  true, // Juneteenth
		{7, 4}:   true, // Independence Day
		{9, 1}:   true, // Labor Day
		{11, 27}: true, // Thanksgiving
		{12, 25}: true, // Christmas
	}

	if stockMarketHolidays[[2]int{int(date.Month()), date.Day()}] {
		return false
	}

	return true
}

/* =========================
   Status Output
   ========================= */

func printStatus() {
	fmt.Print("\033[2J\033[H")
	posColor := ColorYellow
	switch activeTrade.Position {
	case "Long":
		posColor = ColorGreen
	case "Short":
		posColor = ColorRed
	}
	pnlColor := ColorWhite
	if activeTrade.PnL > 0 {
		pnlColor = ColorGreen
	} else if activeTrade.PnL < 0 {
		pnlColor = ColorRed
	}
	fmt.Printf("=== 📊 Trading Status ===\n")
	fmt.Printf(" Position : %s%s%s\n", posColor, activeTrade.Position, ColorReset)
	fmt.Printf(" Shares   : %s%d%s\n", ColorCyan, activeTrade.Quantity, ColorReset)
	fmt.Printf(" PnL      : %s%.2f%s\n", pnlColor, activeTrade.PnL, ColorReset)
	fmt.Printf(" Closed   : %v\n", activeTrade.Closed)
	fmt.Printf("=========================\n")
}

/* =========================
   Main Loop
   ========================= */

func main() {
	config = loadConfig("config.json")
	go watchConfig("config.json")

	// Load triggers
	templatesCV = make(map[string]gocv.Mat)
	for name, path := range config.Templates {
		mat := gocv.IMRead(path, gocv.IMReadColor)
		if mat.Empty() {
			log.Fatalf("Failed to load trigger template %s", path)
		}
		gocv.CvtColor(mat, &mat, gocv.ColorBGRToRGB) // normalize to RGB
		templatesCV[name] = mat
	}

	// Load positions
	positionTemplates = make(map[string]gocv.Mat)
	for name, path := range config.Positions {
		mat := gocv.IMRead(path, gocv.IMReadColor)
		if mat.Empty() {
			log.Fatalf("Failed to load position template %s", path)
		}
		gocv.CvtColor(mat, &mat, gocv.ColorBGRToRGB) // normalize to RGB
		positionTemplates[name] = mat
	}

	if err := cacheTriggerRegions(); err != nil {
		log.Fatal("❌", err)
	}

	// Cache Buy/Sell
	if err := cacheActionButtons(); err != nil {
		log.Fatal("❌", err)
	}

	fmt.Println("✅ Bot ready. Starting loop...")

	for {
		// 🔥 Crash-safe loop
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Println("🔥 Panic recovered in loop:", r)
				}
			}()

			currentPos := detectCurrentPosition()
			activeTrade.Position = currentPos // trust on-screen badges

			// 1) Refresh screen/symbol
			if err := refreshSecurity("Images/Action/change_symbol.png", 0, 30); err != nil {
				fmt.Println("⚠️ Refresh error:", err)
				time.Sleep(500 * time.Millisecond)
				return
			}

			// 2) Screenshot region (trigger detection)
			var regionMat gocv.Mat
			var err error
			if regionCached {
				regionMat, err = captureToMat(cachedRegion)
			} else {
				full := screenshot.GetDisplayBounds(0)
				regionMat, err = captureToMat(full)
			}
			if err != nil {
				fmt.Println("⚠️ Screenshot error:", err)
				time.Sleep(500 * time.Millisecond)
				return
			}

			trigger := detectTrigger()
			fmt.Printf("Trigger detected: %q\n", trigger)

			// With this:
			//trigger, foundRect := scanMultipleTemplates(regionMat, templatesCV, 0.80)
			//fmt.Printf("Trigger detected: %q (rect=%v)\n", trigger, foundRect)
			//if trigger != "" && !regionCached {
			//	cachedRegion = foundRect
			//	regionCached = true
			//}
			//trigger := detectTrigger()
			//fmt.Printf("Trigger detected: %q\n", trigger)

			//trigger, foundRect := scanMultipleTemplates(regionMat, templatesCV, 0.85)
			regionMat.Close() // ✅ Always close before moving on

			//fmt.Printf("Trigger detected: %q (region=%v)\n", trigger, foundRect)

			//if trigger != "" && !regionCached {
			//cachedRegion = foundRect
			//	regionCached = true
			//}

			// 3) Detect position once per loop
			//currentPos := detectCurrentPosition()

			// 4) OCR qty/pnl
			qty, pnl, _ := runOCR(cachedRegion)

			// 5) Auto-close check
			configLock.RLock()
			pt := config.ProfitTarget
			sl := config.LossLimit
			configLock.RUnlock()
			if (currentPos == "Long" || currentPos == "Short") && (pnl >= pt || pnl <= sl) {
				fmt.Printf("⚡ Threshold hit PnL=%.2f → closepos\n", pnl)
				trigger = "closepos"
				// Advance trading day
				if err := nextTradingDay(); err != nil {
					fmt.Println("⚠️ nextTradingDay failed:", err)
				}
			}

			// 6) Trade decision (pass in currentPos, don’t re-detect inside)
			if trigger != "" {
				determineTrade(trigger, currentPos, qty, pnl)
			}

			// 7) Print status
			//printStatus()

		}() // run wrapper

		// ⏳ loop sleep
		time.Sleep(300 * time.Millisecond)
	}
}
