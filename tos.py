############################################################################################################
## MODULES
############################################################################################################
from sikuli import *
import time
from datetime import datetime
import java.awt.event.InputEvent as InputEvent
import java.awt.MouseInfo as MouseInfo
import java.awt.event.KeyEvent as KeyEvent
import os
import re
import subprocess
import tempfile
import threading
############################################################################################################
## GLOBAL SETTINGS
############################################################################################################
Settings.MinSimilarity = 0.9 
Settings.MoveMouseDelay = 0
Settings.ActionLogs = False
Settings.ObserveScanRate = 10

#root_directory = os.getcwd()
running_directory = getBundlePath()
xconfig_path = running_directory + "/" + "/TrendTraderv2025.conf"
############################################################################################################
## TRADING SETTINGS
############################################################################################################
TRADE_DAY_START = None # 2
TRADE_MONTH     = None # 6
SMYBOL_FAKE_REFRESH = "ZX"
TIN_TIM = None

config_vars = {}
profit_amount_current = 0
profit_taking_status = False
timeout_seconds = 1
current_trade = "nomove"
current_position = "noposition"
accountType = None
lock = threading.Lock()
threads = []

default_region = Region(1105,62,812,360)
############################################################################################################
## LOG COLORS
############################################################################################################
# ANSI bash color codes
COLORS = {
    "red": "\033[31m",
    "green": "\033[32m",
    "yellow": "\033[33m",
    "blue": "\033[34m",
    "reset": "\033[0m"
}
############################################################################################################
## LOGGING
############################################################################################################
def log_message(message, color):
    # Get color code or default to no color
    color_code = COLORS.get(color.lower(), "")
    # Print message with color and reset
    print("{}{}{}".format(color_code, message, COLORS["reset"]))
def currentSettings():
    global config_vars
    
    log_message("settings:", "blue")
    log_message("DEBUG: " + config_vars["DEBUG"], "yellow")
    log_message("SYMBOL: " + config_vars["SYMBOL_LIVE"], "yellow")
    log_message("PROFIT AMOUNT: " + config_vars["PROFIT_AMOUNT"], "yellow")
    log_message("LOSS AMOUNT: " + config_vars["LOSS_AMOUNT"], "yellow")    
    print("-------------------------------")
############################################################################################################
## CONFIG
############################################################################################################
def validate_value(value):
    """Validate if value is a string or integer."""
    try:
        # Try converting to integer
        return int(value)
    except ValueError:
        # Treat as string if not integer
        try:
            return bool(value)
        except ValueError:                        
            return value.strip()

def search_config_file(config_path, search_key):
    """Search .conf file for a variable and return its value."""
    global config_vars
    if not os.path.exists(config_path):
        print("Config file not found: " + config_path)
        return None

    try:
        current_section = ""
        with open(config_path, 'r') as file:
            for line in file:
                line = line.strip()
                # Skip empty lines or comments
                if not line or line.startswith('#') or line.startswith(';'):
                    continue
                # Check for section
                if line.startswith('[') and line.endswith(']'):
                    current_section = line[1:-1].strip()
                    continue
                # Check for key-value pair
                if '=' in line:
                    key, value = [part.strip() for part in line.split('=', 1)]
                    value = value.replace('"','')
                    full_key = current_section + "." + key #if current_section else key
                    if full_key == search_key:
                        config_vars[key] = value
                        return
        print("Key '" + search_key + "' not found in " + config_path)
        return None
    except Exception as e:
        print("Error reading config file: " + str(e))
        return None
def searchVariableData():
    global xconfig_path
    
    # Example search keys
    search_keys = [
        "settings.DEBUG", 
        "settings.SYMBOL_LIVE",
        "settings.PROFIT_AMOUNT",
        "settings.LOSS_AMOUNT"         
    ]
    
    try:
        for key in search_keys:
            result = search_config_file(xconfig_path, key)
            if result is not None:
                print("Found " + key + " = " + result + " (Type: " + type(result).__name__)
    
    except Exception as e:
        print("Error: " + str(e))    
############################################################################################################
## Scan Images and Regions
############################################################################################################
actionPath = {
    "change_symbol":   {"path":"Images/Action/change_symbol.png",    "region":None},
    "sell":            {"path":"Images/Action/sell.png",             "region":None},
    "buy":             {"path":"Images/Action/buy.png",              "region":None},
    "short":           {"path":"Images/Routing/short.png",           "region":None},
    "long":            {"path":"Images/Routing/long.png",            "region":None},
    "nomove":          {"path":"Images/Routing/nomove.png",          "region":None},
    "noposition":      {"path":"Images/Position/noposition.png",     "region":None},
    "longposition":    {"path":"Images/Position/longposition.png",   "region":None},
    "shortposition":   {"path":"Images/Position/shortposition.png",  "region":None},
    "close_pos":       {"path":"Images/Routing/close_pos.png",       "region":None},
    "placemouse":      {"path":"Images/Routing/placemouse.png",      "region":None},
    "virtual_account": {"path":"Images/Account/virtual_account.png", "region":None},
    "live_account":    {"path":"Images/Account/live_account.png",    "region":None},
    "paper_account":   {"path":"Images/Account/paper_account.png",   "region":None},
    "time_refresh":    {"path":"Images/TradingDay/timerefresh.png",  "region":None},
    "go_refresh":      {"path":"Images/TradingDay/gorefresh.png",    "region":None},
    "flatten":         {"path":"Images/Routing/flatten.png",         "region":None},
    "profitx":         {"path":"Images/Profit/plday.png",            "region":None},
    "reset_start":     {"path":"Images/Reset/reset_start.png",       "region":None},
    "console":         {"path":"Images/Console/console.png",         "region":None}
}
def setRegion(region):
    global actionPath 
    global default_region
    global config_vars

    with lock:
        if config_vars["DEBUG"] == "True":
            start_time = time.time()  
        result = False   
        regionState = default_region.exists(actionPath[region]["path"],0)
        if regionState:
            if region == "noposition" or region == "shortposition" or region == "longposition":            
                    new_region = Region(regionState.getX() - 10, regionState.getY() - 10, regionState.getW() + 20, regionState.getH() + 20)
                    actionPath["noposition"]["region"] = new_region 
                    actionPath["shortposition"]["region"] = new_region
                    actionPath["longposition"]["region"] = new_region
            elif region == "long" or region == "short" or region == "nomove" or region == "close_pos":
                    new_region = Region(regionState.getX() - 10, regionState.getY() - 10, regionState.getW() + 20, regionState.getH() + 20)
                    actionPath["long"]["region"] = new_region 
                    actionPath["short"]["region"] = new_region
                    actionPath["nomove"]["region"] = new_region 
                    actionPath["close_pos"]["region"] = new_region
            else:
                new_region = Region(regionState.getX() - 10, regionState.getY() - 10, regionState.getW() + 20, regionState.getH() + 20)
                actionPath[region]["region"] = new_region       
                result = True
        if config_vars["DEBUG"] == "True":
            # End timing
            end_time = time.time()       
            # Calculate and output execution time
            execution_time = end_time - start_time
            print("setRegion(" + region + ") - Execution time: {:.3f} seconds".format(execution_time))        
        return result

def scanForTriggers():
    global actionPath

    for name,region in actionPath.items():
        if region["region"] == None:
            #setRegion(name)    
            thread = threading.Thread(
                target=setRegion,
                args=(name,)
            )
            thread.start()  
            thread.join()    
############################################################################################################
## TRADING DAYS
############################################################################################################
tradingDay = {
    1: "Images/TradingDay/day1.png",
    2: "Images/TradingDay/day2.png",
    3: "Images/TradingDay/day3.png",
    4: "Images/TradingDay/day4.png",
    5: "Images/TradingDay/day5.png",
    6: "Images/TradingDay/day6.png",
    7: "Images/TradingDay/day7.png",
    8: "Images/TradingDay/day8.png",
    9: "Images/TradingDay/day9.png",
    10: "Images/TradingDay/day10.png",
    11: "Images/TradingDay/day11.png",
    12: "Images/TradingDay/day12.png",
    13: "Images/TradingDay/day13.png",
    14: "Images/TradingDay/day14.png",
    15: "Images/TradingDay/day15.png",
    16: "Images/TradingDay/day16.png",
    17: "Images/TradingDay/day17.png",
    18: "Images/TradingDay/day18.png",
    19: "Images/TradingDay/day19.png",
    20: "Images/TradingDay/day20.png",            
    21: "Images/TradingDay/day21.png",
    22: "Images/TradingDay/day22.png",
    23: "Images/TradingDay/day23.png",
    24: "Images/TradingDay/day24.png",
    25: "Images/TradingDay/day25.png",
    26: "Images/TradingDay/day26.png",
    27: "Images/TradingDay/day27.png",
    28: "Images/TradingDay/day28.png",
    29: "Images/TradingDay/day29.png",       
    30: "Images/TradingDay/day30.png",
    31: "Images/TradingDay/day31.png",            
    "timesection": "Images/TradingDay/timesection.png"
}

def get_month_number(month_name):
    month_name = month_name.lower()  # Convert input to lowercase for case-insensitive matching
    switch = {
        "jan": 1,
        "january": 1,
        "feb": 2,
        "february": 2,
        "mar": 3,
        "march": 3,
        "apr": 4,
        "april": 4,
        "may": 5,
        "jun": 6,
        "june": 6,
        "jul": 7,
        "july": 7,
        "aug": 8,
        "august": 8,
        "sep": 9,
        "september": 9,
        "oct": 10,
        "october": 10,
        "nov": 11,
        "november": 11,
        "dec": 12,
        "december": 12
    }
    return switch.get(month_name, "Invalid month name")

def getTradingDay():
    global lock
    global TRADE_DAY_START
    global TRADE_MONTH
    global TIN_TIM
    with lock:
        tex = actionPath["time_refresh"]["region"]
        # Define the region to capture
        region = Region(tex.x + 30, tex.y + 8, tex.w - 3, tex.h - 15)
        
        # Create a temporary file for the screenshot
        temp_file = tempfile.NamedTemporaryFile(suffix=".png", delete=False).name
        
        img = capture(region)
        os.rename(img, temp_file)  # Move captured image to temp file
    
        result = subprocess.check_output(["tesseract", temp_file, "stdout"], universal_newlines=True)
        TIN_TIM = result
        result = (result.strip()).split(", ")
        TRADE_MONTH = get_month_number(result[0])
        TRADE_DAY_START = result[1]

        return

def checkTradingDay():    
    global TRADE_DAY_START
    global TRADE_MONTH
    global TIN_TIM
    
    thread = threading.Thread(
        target=getTradingDay,
        args=(None)
    )
    thread.start()  
    thread.join() 
    print("-" + TIN_TIM + "-")

    if TRADE_DAY_START > 0 and TRADE_MONTH > 0:
        return True
    else:
        return False 

def profitTaking():
    global profit_taking_status
    global config_vars
    # Validate coordinates and dimensions
    global lock

    with lock:
        tex = actionPath["profitx"]["region"]
        # Define the region to capture
        region = Region(tex.x + 47, tex.y + 10, tex.w + 50, tex.h - 10)
        
        # Create a temporary file for the screenshot
        temp_file = tempfile.NamedTemporaryFile(suffix=".png", delete=False).name
        
        img = capture(region)
        os.rename(img, temp_file)  # Move captured image to temp file
    
        result = subprocess.check_output(["tesseract", temp_file, "stdout"], universal_newlines=True)
        result = result.replace("$", "")
        result = result.replace("(", "-")
        result = result.replace(",", "")
        result = result.replace("`", "")    
        result = result.replace("'", "")     
        result = re.sub(r'\..+', '', result)
        result = result.strip()
    
        if result != "" and result != 0 and result != " ":
            try:
                # Attempt to convert to float (handles decimals)
                number = float(result)
                # If the number is a whole number, convert to int
                if number.is_integer():
                    number = int(number)
                #print("Successfully converted string to number: " + str(number))
                #type(str(number))
            
            except ValueError:
                print("Error: The text '" + result + "' cannot be converted to a number.")
        
            if os.path.exists(temp_file):
                    os.remove(temp_file)   
            
            if  number >= int(config_vars["PROFIT_AMOUNT"]):
                profit_taking_status =  True
            else:
                profit_taking_status = False
        profit_taking_status =  False
        return

def checkProfitTaking():
    global profit_taking_status
    global threads
    thread = threading.Thread(
        target=profitTaking,
        args=(None)
    )
    threads.append(thread)
    thread.start()  
    #thread.join() 
    return profit_taking_status

def nextTradingDay():
    #global day
    # Get current date to set year and month
    current_date = datetime.now()
    year = current_date.year
    month = int(config_vars["TRADE_MONTH"])

    checkpoint = int(config_vars["TRADE_DAY_START"]) + 1
   
    for i in range(checkpoint,32):
        # Create date object for the given day in current month/year
        input_date = datetime(year, month, i)
        
        # Get weekday (0 = Monday, 6 = Sunday)
        weekday = input_date.weekday()
    
        # Check if it's a weekday (Monday to Friday)
        if 0 <= weekday <= 4:
            day = i       
            actionPath["time_refresh"]["region"].click(actionPath["time_refresh"]["path"],0)
            time.sleep(1)
            click(config_vars["TRADE_DAY_START"])
            click(tradingDay["timesection"])
            type("a", KeyModifier.CTRL)
            paste("05:30:00")
            mouseMove(+125,0)
            click(Mouse.at())
           # actionPath["go_refresh"]["region"].click(actionPath["go_refresh"]["path"],0)            
            time.sleep(2)
            return day
    return

def resetAccount():
    #global profit_amount
    global config_vars
    global profit_amount_current

    tex = actionPath["reset_start"]["region"].click(actionPath["reset_start"]["path"],0)
    mouseMove(0,+60)
    click(Mouse.at())
    mouseMove(-100,+45)
    wait(1)
    click(Mouse.at())  
    profit_amount_current = profit_amount_current + config_vars["PROFIT_AMOUNT"]  
    return    
############################################################################################################
## CHECK ACCOUNT TYPE
############################################################################################################
def accountType():
    global accountType

    if exists(actionPath["virtual_account"]["path"]):
        accountType = "virtual"
    elif exists(actionPath["live_account"]["path"]):
        accountType = "live"
    elif exists(actionPath["paper_account"]["path"]):
        accountType = "paper"
    print("-------------------------------")        
    log_message("Account Type: " + str(accountType), "blue")
    print("-------------------------------")
    return
############################################################################################################
## MOUSE MOVEMENT MONITORING
############################################################################################################
def is_mouse_moving(last_position):
    current_position = MouseInfo.getPointerInfo().getLocation()
    return current_position != last_position 

def checkUserIneraction():
    while True:
            last_mouse_position = MouseInfo.getPointerInfo().getLocation()
            time.sleep(0.5)  # Check every 2 seconds
            if not is_mouse_moving(last_mouse_position):
                #print("No Mouse Movement. Resumed...")
                break
            else:
                print("User Interaction Detected. Waiting...")       
############################################################################################################
## REFRESH SECURITY CHOICES
############################################################################################################
def refreshSymbol():
    global config_vars 
    global SMYBOL_FAKE_REFRESH

    if config_vars["DEBUG"] == "True":
        start_time = time.time()
    t = actionPath["change_symbol"]["region"].exists(actionPath["change_symbol"]["path"], 0)
    if t != None:
        # Capture initial mouse position
        original_pos = Location(Env.getMouseLocation())
        checkUserIneraction()
        click(Location(t.x, (t.y + 30)))
        # Move mouse back to original position
        mouseMove(original_pos)          
        paste(SMYBOL_FAKE_REFRESH)
        type(Key.ENTER)
        paste(config_vars["SYMBOL_LIVE"])
        type(Key.ENTER + Key.TAB)
    actionPath["placemouse"]["region"].click(actionPath["placemouse"]["path"],0)
    #time.sleep(2)
    if config_vars["DEBUG"] == "True":
        # End timing
        end_time = time.time()       
        # Calculate and output execution time
        execution_time = end_time - start_time
        print("refreshSymbol() - Execution time: {:.3f} seconds".format(execution_time))        
    return  
############################################################################################################
## CHECK IF MARKET IS OPEN
############################################################################################################
def isMarketOpen():
    # Get current date
    current_date = datetime.now()    
    # Convert to weekday (0 = Monday, 1 = Tuesday, ..., 6 = Sunday)
    weekday_num = current_date.weekday()
    #weekdays = ["Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"]
    #weekday_name = weekdays[weekday_num]
    
    if weekday_num < 5:    
        # Stock market holidays for 2025 (month, day)
        stock_market_holidays = [
            (1, 1),   # New Year's Day: Jan 1, 2025
            (1, 20),  # Martin Luther King Jr. Day: Jan 20, 2025
            (2, 17),  # Washington's Birthday: Feb 17, 2025
            (4, 18),  # Good Friday: Apr 18, 2025
            (5, 26),  # Memorial Day: May 26, 2025
            (6, 19),  # Juneteenth: Jun 19, 2025
            (7, 4),   # Independence Day: Jul 4, 2025
            (9, 1),   # Labor Day: Sep 1, 2025
            (11, 27), # Thanksgiving: Nov 27, 2025
            (12, 25)  # Christmas Day: Dec 25, 2025
        ]        
        # Check if today is a stock market holiday
        is_stock_market_holiday = (current_date.month, current_date.day) in stock_market_holidays
        holiday_message = "The U.S. stock market is open today."        
        if is_stock_market_holiday:
            return "closed"
            #holiday_message = "The U.S. stock market is closed today." 
        else:
            return "open"
        # Display the result
        #popup("Date: " + current_date.strftime("%Y-%m-%d") + "\nWeekday: " + weekday_name + "\n" + holiday_message)    
    else:
        return "closed"                
############################################################################################################
## SCAN FOR POSITION
############################################################################################################
def scanPosition(name,imagepath,region):
    global current_position
    global lock
    with lock:
        if region.exists(imagepath,0):
            current_position = name
            return True
        else:
            return False

def scanForPosition():
    global current_position
    global actionPath

    list = ["noposition","longposition","shortposition"]

    for position in list:
        region = actionPath[position]["region"]
        if region != None:
            #setRegion(name)    
            thread = threading.Thread(
                target=scanPosition,
                args=(position,actionPath[position]["path"],region,)
            )
            thread.start()  
            thread.join() 
    return current_position       
############################################################################################################
## SCAN FOR TRADE
############################################################################################################
def scanTrade(name,imagepath,region):
    global current_trade
    global lock
    with lock:
        if region.exists(imagepath,0):
            current_trade = name
            return True
        else:
            return False

def scanForTrade():
    global current_trade
    global actionPath

    list = ["long","short","close_pos","nomove"]

    for choice in list:
        region = actionPath[choice]["region"]
        if region != None:
            #setRegion(name)    
            thread = threading.Thread(
                target=scanTrade,
                args=(choice,actionPath[choice]["path"],region,)
            )
            thread.start()  
            thread.join() 
    return current_trade     
############################################################################################################
## TRADE
############################################################################################################
def trade(action,position):
    if action == "nomove":
        return
    elif action == "long" and position == "noposition":
        actionPath["buy"]["region"].click(actionPath["buy"]["path"],0)
    elif action == "short" and position == "noposition":
        actionPath["sell"]["region"].click(actionPath["sell"]["path"],0)   
    elif action == "close_pos" and position == "noposition":
        nextTradingDay()        
    elif action == "long" and position == "shortposition":
        actionPath["buy"]["region"].click(actionPath["buy"]["path"],0)
        actionPath["buy"]["region"].click(actionPath["buy"]["path"],0)
    elif action == "short" and position == "longposition":
        actionPath["sell"]["region"].click(actionPath["sell"]["path"],0)
        actionPath["sell"]["region"].click(actionPath["sell"]["path"],0)        
    elif action == "close_pos" and position == "longposition":
        actionPath["sell"]["region"].click(actionPath["sell"]["path"],0)
        log_message("CLOSING OUT POSITION.", "yellow")
        nextTradingDay() 
    elif action == "close_pos" and position == "shortposition":
        actionPath["buy"]["region"].click(actionPath["buy"]["path"],0)
        log_message("CLOSING OUT POSITION.", "yellow")
        nextTradingDay()
############################################################################################################
## MAIN SCRIPT
############################################################################################################
def main():
    global profit_amount_current  

    accountType() 
    searchVariableData()
    currentSettings()

    while True:
        print(1)
        scanForTriggers()
        print(2)
        if accountType == "virtual":
            if not checkTradingDay():
                print("Unable to verify current trading day and month on virtual account. Please review.")
                exit()
        print(3)
        if checkProfitTaking():
            trade("close_pos",scanForPosition())
            resetAccount()
            log_message("CURRENT MONTH PROFITS: " + str(profit_amount_current), "green")  
        print(4)
        Trade_Result = scanForTrade()
        print(45)
        Position_Result = scanForPosition()
        print(46)
        trade(Trade_Result,Position_Result) 
        print(5)
        refreshSymbol()  
        print(6)
        for t in threads:
            t.join()  
        print(7)

if __name__ == "__main__":  
    main()    
