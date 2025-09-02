#!/bin/bash

while($True);
do
    sleep 5

    res=$(ps -aux | grep TrendTrader | grep 2025 | awk '{print $2}' | head -n 1)
    if [ -z $res ]; then 
        echo EXIT
        exit
    fi

    rm -rf /tmp/mdrcache*
    find /tmp -type f -name "*.png" -mmin +1 ! -name "grayscale_processed_trendtrader.png" -delete

done
