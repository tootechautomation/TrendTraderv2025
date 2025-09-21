#!/bin/bash

cd /tmp
wget https://tosmediaserver.schwab.com/installer/InstFiles/thinkorswim_installer.sh


sudo apt update -y
sudo apt install g++ -y 
sudo apt-get install libleptonica-dev   -y
sudo apt install -y build-essential cmake git pkg-config \
    libgtk-3-dev libavcodec-dev libavformat-dev libswscale-dev \
    libtbb2 libtbb-dev libjpeg-dev libpng-dev libtiff-dev \
    libdc1394-dev libopenexr-dev libatlas-base-dev gfortran \
    python3-dev
git clone https://github.com/opencv/opencv.git
git clone https://github.com/opencv/opencv_contrib.git
cd opencv
git checkout 4.10.0
cd ../opencv_contrib
git checkout 4.10.0
cd ../opencv
mkdir build && cd build
cmake -D CMAKE_BUILD_TYPE=Release \
      -D CMAKE_INSTALL_PREFIX=/usr/local \
      -D OPENCV_EXTRA_MODULES_PATH=../../opencv_contrib/modules ..
make -j$(nproc)
sudo make install
sudo ldconfig

sudo apt install -y build-essential automake libtool pkg-config \
    libjpeg-dev libpng-dev libtiff-dev zlib1g-dev

wget https://github.com/DanBloomberg/leptonica/releases/download/1.83.1/leptonica-1.83.1.tar.gz
tar -xvzf leptonica-1.83.1.tar.gz
cd leptonica-1.83.1

./configure
make -j$(nproc)
sudo make install
sudo ldconfig