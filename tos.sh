#!/bin/bash 

rm -rf /tmp/ad_*
rm -rf /tmp/mdrcache*
rm -rf /tmp/+~JF*
rm -rf /tmp/Screenshot*
rm -rf /tmp/.org.chrom*

TT_PATH="/home/$(whoami)/Documents/TrendTraderv2025/"
API_VERSION="2.0.5"
if [ ! -f  "./sikulixapi-$(API_VERSION)-lux.jar" ]; then
    LOCAL_PATH="$TT_PATH/sikulixapi-$API_VERSION-lux.jar"
    REMOTE_PATH="https://launchpad.net/sikuli/sikulix/$API_VERSION/+download/sikulixapi-$API_VERSION-lux.jar" 
    echo "File not found!"
    curl -o $LOCAL_PATH $REMOTE_PATH   
fi

while $true; 
do 
    java -jar /home/$(whoami)/Documents/TrendTraderv2025/TrendTraderv2025.jar 
done



