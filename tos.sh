#!/bin/bash 

rm -rf /tmp/ad_*
rm -rf /tmp/mdrcache*
rm -rf /tmp/+~JF*
rm -rf /tmp/Screenshot*
rm -rf /tmp/.org.chrom*

TT_PATH="/home/$(whoami)/Documents/TrendTraderv2025"
API_VERSION="2.0.5"
if [ ! -f  "$TT_PATH/sikulixapi-$API_VERSION-lux.jar" ]; then
    LOCAL_PATH="$TT_PATH/sikulixapi-$API_VERSION-lux.jar"
    REMOTE_PATH="https://launchpad.net/sikuli/sikulix/$API_VERSION/+download/sikulixapi-$API_VERSION-lux.jar" 
    echo "File not found!"
    wget -O $LOCAL_PATH $REMOTE_PATH   
fi

# Check if tesseract-ocr is installed
if ! command -v tesseract &> /dev/null; then
    echo "tesseract-ocr is not installed. Installing now..."
    # Update package list and install tesseract-ocr
    sudo apt update
    sudo apt install tesseract-ocr -y
    # Verify installation
    if command -v tesseract &> /dev/null; then
        echo "tesseract-ocr installed successfully."
    else
        echo "Failed to install tesseract-ocr."
        exit 1
    fi
fi

bash ./cleanup.sh &

while $true; 
do 
    export CLASSPATH=$CLASSPATH:$TT_PATH/sikulixapi-$API_VERSION-lux.jar
    java -jar $TT_PATH/sikulixapi-$API_VERSION-lux.jar -r $TT_PATH/TrendTraderv2025.jar #" org.sikuli.script.Runner
    #java -jar /home/$(whoami)/Documents/TrendTraderv2025/TrendTraderv2025.jar 
done

# VNC XFCE
#XAUTHORITY=$HOME/.Xauthority
#export XAUTHORITY   

