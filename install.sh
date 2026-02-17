#!/bin/bash
set -e

REPO="tripleclabs/xavi"
INSTALL_DIR="/usr/local/bin"
BINARY_NAME="xavi"
CONFIG_DIR="/etc/tripleclabs"

# Detect Architecture
ARCH=$(uname -m)
case $ARCH in
    x86_64)
        ARCH="amd64"
        ;;
    aarch64)
        ARCH="arm64"
        ;;
    *)
        echo "Unsupported architecture: $ARCH"
        exit 1
        ;;
esac

echo "Detected architecture: $ARCH"

# --- Container Engine Detection & Installation ---

# Detect Package Manager
PKG_MGR=""
INSTALL_CMD=""

if command -v apt-get &> /dev/null; then
    PKG_MGR="apt-get"
    INSTALL_CMD="apt-get update && apt-get install -y"
elif command -v dnf &> /dev/null; then
    PKG_MGR="dnf"
    INSTALL_CMD="dnf install -y"
elif command -v yum &> /dev/null; then
    PKG_MGR="yum"
    INSTALL_CMD="yum install -y"
elif command -v pacman &> /dev/null; then
    PKG_MGR="pacman"
    INSTALL_CMD="pacman -Sy --noconfirm"
elif command -v zypper &> /dev/null; then
    PKG_MGR="zypper"
    INSTALL_CMD="zypper install -y"
fi

# Try to ensure Podman is installed
if ! command -v podman &> /dev/null; then
    echo "Podman not found."
    if [ -n "$INSTALL_CMD" ]; then
        echo "Attempting to install Podman using $PKG_MGR..."
        EXT_CMD="sudo $INSTALL_CMD podman"
        echo "Running: $EXT_CMD"
        eval $EXT_CMD
    else
        echo "Warning: Could not detect package manager to install Podman automatically."
    fi
fi

# Determine Engine Configuration
SERVICE_REQUIRES=""
SERVICE_AFTER=""
ENV_VARS=""

if command -v podman &> /dev/null; then
    echo "Using Podman as container engine."
    
    # Enable Podman Socket
    echo "Enabling podman.socket..."
    sudo systemctl enable --now podman.socket

    SERVICE_REQUIRES="podman.socket"
    SERVICE_AFTER="network-online.target podman.socket"
    # Set DOCKER_HOST so the generic Docker client libraries work with Podman
    ENV_VARS="Environment=DOCKER_HOST=unix:///run/podman/podman.sock"

elif command -v docker &> /dev/null; then
    echo "Podman not available. Falling back to Docker."
    SERVICE_REQUIRES="docker.service"
    SERVICE_AFTER="network-online.target docker.service"
    ENV_VARS="" # Standard Docker socket is default

else
    echo "Error: Neither Podman nor Docker is available, and Podman installation failed."
    echo "Please install Podman or Docker manually and rerun this script."
    exit 1
fi

# --- Download & Install Xavi ---

# Determine latest release URL
# Simplified: Assume "latest" tag or use GitHub API to find it. 
# For stability, using "latest" release asset pattern.
DOWNLOAD_URL="https://github.com/$REPO/releases/latest/download/xavi-linux-$ARCH"

CHECKSUM_URL="https://github.com/$REPO/releases/latest/download/checksums.txt"

echo "Downloading Xavi from $DOWNLOAD_URL..."
curl -L -o /tmp/$BINARY_NAME $DOWNLOAD_URL

echo "Verifying checksum..."
curl -L -o /tmp/xavi-checksums.txt $CHECKSUM_URL
EXPECTED=$(grep "xavi-linux-$ARCH" /tmp/xavi-checksums.txt | awk '{print $1}')
ACTUAL=$(sha256sum /tmp/$BINARY_NAME | awk '{print $1}')
rm -f /tmp/xavi-checksums.txt

if [ -z "$EXPECTED" ]; then
    echo "Error: Could not find checksum for xavi-linux-$ARCH in checksums.txt"
    exit 1
fi

if [ "$EXPECTED" != "$ACTUAL" ]; then
    echo "Error: Checksum mismatch!"
    echo "  Expected: $EXPECTED"
    echo "  Actual:   $ACTUAL"
    rm -f /tmp/$BINARY_NAME
    exit 1
fi
echo "Checksum verified."

echo "Installing to $INSTALL_DIR..."
sudo mv /tmp/$BINARY_NAME $INSTALL_DIR/$BINARY_NAME
sudo chmod +x $INSTALL_DIR/$BINARY_NAME

echo "Creating configuration directory..."
sudo mkdir -p $CONFIG_DIR

# --- Systemd Service Creation ---

SERVICE_FILE="/etc/systemd/system/xavi.service"
echo "Creating systemd service at $SERVICE_FILE..."

cat <<EOF | sudo tee $SERVICE_FILE
[Unit]
Description=Xavi Edge Agent
Documentation=https://github.com/$REPO
After=$SERVICE_AFTER
Requires=$SERVICE_REQUIRES

[Service]
Type=simple
User=root
ExecStart=$INSTALL_DIR/$BINARY_NAME
Restart=always
RestartSec=5s
$ENV_VARS
# Environment variables can be added here
# Environment=XAVI_LOG_LEVEL=info

[Install]
WantedBy=multi-user.target
EOF

echo "Reloading systemd..."
sudo systemctl daemon-reload

echo "Enabling and starting Xavi..."
sudo systemctl enable xavi
sudo systemctl start xavi

echo "Xavi installed successfully!"
echo "Configuration: $CONFIG_DIR/xavi.json"
echo "Secrets:       $CONFIG_DIR/xavi.secrets"
echo "Logs:          journalctl -u xavi -f"
