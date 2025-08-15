from sikuli import *
import os
import time
import subprocess

# Global variable for file path
file_path = "/home/tboone/test.txt"  # Update this path

def check_file_write_time():
    try:
        # Get the last modification time (in seconds since epoch)
        mod_time = os.path.getmtime(file_path)
        # Convert to human-readable format
        mod_time_str = time.ctime(mod_time)
        popup("Last write time for %s:\n%s" % (file_path, mod_time_str), "File Write Time")
        return mod_time
    except OSError as e:
        popup("Error: Could not access file %s\n%s" % (file_path, str(e)), "Error")
        return None

def find_process_pid(process_name):
    try:
        # Run tasklist command to find processes (Windows-specific)
        output = subprocess.check_output(["tasklist", "/FI", "IMAGENAME eq %s" % process_name], shell=True).decode()
        # Parse output to find PID
        for line in output.splitlines():
            if process_name.lower() in line.lower():
                parts = line.split()
                if len(parts) > 1 and parts[1].isdigit():
                    pid = int(parts[1])
                    popup("Found process %s with PID: %s" % (process_name, pid), "Process Search")
                    return pid
        popup("Process %s not found." % process_name, "Process Search")
        return None
    except subprocess.CalledProcessError as e:
        popup("Error searching for process %s:\n%s" % (process_name, str(e)), "Error")
        return None

def kill_process_if_file_old():
    # Get file's last modification time
    mod_time = check_file_write_time()
    if mod_time is None:
        exit(1)  # Exit if file access failed
    
    # Calculate time difference (in seconds)
    current_time = time.time()
    time_diff = current_time - mod_time
    
    # Check if file is older than 5 minutes (300 seconds)
    if time_diff > 300:
        popup("File %s is older than 5 minutes. Checking for TrendTrader process..." % file_path, "File Age Check")
        pid = find_process_pid("TrendTrader.exe")
        if pid:
            try:
                # Kill the process using taskkill (Windows-specific)
                subprocess.check_call(["taskkill", "/PID", str(pid), "/F"])
                popup("Process TrendTrader (PID: %s) terminated." % pid, "Process Termination")
            except subprocess.CalledProcessError as e:
                popup("Error terminating process with PID %s:\n%s" % (pid, str(e)), "Error")
        else:
            popup("No TrendTrader process found to terminate.", "Process Termination")
    else:
        remaining = int(300 - time_diff)
        mins = remaining // 60
        secs = remaining % 60
        time_display = "%02d:%02d" % (mins, secs)
        popup("File %s is not yet 5 minutes old. Time remaining: %s" % (file_path, time_display), "File Age Check")
    
    exit(0)

# Run the main function
kill_process_if_file_old()