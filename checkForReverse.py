import numpy as np
from typing import List
import sys

def analyze_trade_reversal(pnl_values: List[float], window_size: int = 3, threshold: float = -0.1) -> bool:

    if len(pnl_values) < window_size:
        return False    
    
    # Get the most recent window of P&L values
    recent_values = np.array(pnl_values[-window_size:])
    # Calculate percentage changes between consecutive values
    pct_changes = np.diff(recent_values) / recent_values[:-1]
    
    # Calculate the average rate of change over the window
    avg_change = np.mean(pct_changes)

    #if len(pnl_list) >= 30:
    #    pnl_list.pop(0)
    
    # Check if the average change is below the threshold (significant decline)
    if avg_change < threshold:
        # Confirm if the trend is consistently downward
        negative_changes = np.sum(pct_changes < 0)
        if negative_changes >= window_size // 2:  # Majority of changes are negative
            return True
    
    return False   


def main():

    args = sys.argv[1:]  # Skip sys.argv[0] which is the script name
    if len(args) >= 1:
        received_list = [int(x) for x in args[0].split()]
        #print("P&L Values:", sample_pnl)
        is_reversing = analyze_trade_reversal(received_list, window_size=10, threshold=-0.1)
        if is_reversing:
            #print("Trade is reversing! Consider closing the position to lock in profits.")
            print("True")
            #return True
        else:
            #print("No significant reversal detected. Trade may continue.")
            print("False")
            #return False

    else:
        print("Error: Expected a list as an argument")
        #return False

    # Example usage with sample P&L data
    # Replace this with actual P&L data from your trading system
    
   

if __name__ == "__main__":
    main()    