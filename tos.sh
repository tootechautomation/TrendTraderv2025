#!/bin/bash 

rm -rf /tmp/ad_*
rm -rf /tmp/mdrcache*
rm -rf /tmp/+~JF*
rm -rf /tmp/Screenshot*
rm -rf /tmp/.org.chrom*

while $true; 
do 
    java -jar /home/$(whoami)/Documents/TrendTraderv2025/PLRunner/plrunner.jar &
    java -jar /home/$(whoami)/Documents/TrendTraderv2025/TrendTraderv2025.jar 
    for i in $(ps -aux | grep plrunner | awk '{print $2}'); do kill $i;done
done



