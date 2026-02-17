#!/bin/bash
set -e

REPO="tripleclabs/xavi"
INSTALL_DIR="/usr/local/bin"
BINARY_NAME="xavi"
CONFIG_DIR="/etc/tripleclabs"
AUTO_YES=false

# Parse flags
while getopts "y" opt; do
    case $opt in
        y) AUTO_YES=true ;;
        *) echo "Usage: $0 [-y]"; exit 1 ;;
    esac
done

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

SERVICE_REQUIRES=""
SERVICE_AFTER=""
ENV_VARS=""

if command -v podman &> /dev/null; then
    echo "Found Podman. Using Podman as container engine."

    echo "Enabling podman.socket..."
    sudo systemctl enable --now podman.socket

    SERVICE_REQUIRES="podman.socket"
    SERVICE_AFTER="network-online.target podman.socket"
    ENV_VARS="Environment=DOCKER_HOST=unix:///run/podman/podman.sock"

elif command -v docker &> /dev/null; then
    echo "Found Docker. Using Docker as container engine."

    SERVICE_REQUIRES="docker.service"
    SERVICE_AFTER="network-online.target docker.service"

else
    echo "No container engine found."

    # Ask to install podman (or auto-accept with -y)
    if [ "$AUTO_YES" = true ]; then
        INSTALL_PODMAN=true
    else
        read -p "Would you like to install Podman? [y/N] " answer
        case "$answer" in
            [yY]|[yY][eE][sS]) INSTALL_PODMAN=true ;;
            *) INSTALL_PODMAN=false ;;
        esac
    fi

    if [ "$INSTALL_PODMAN" = true ]; then
        # Detect package manager
        if command -v apt-get &> /dev/null; then
            sudo apt-get update && sudo apt-get install -y podman
        elif command -v dnf &> /dev/null; then
            sudo dnf install -y podman
        elif command -v yum &> /dev/null; then
            sudo yum install -y podman
        elif command -v pacman &> /dev/null; then
            sudo pacman -Sy --noconfirm podman
        elif command -v zypper &> /dev/null; then
            sudo zypper install -y podman
        else
            echo "Error: Could not detect a supported package manager to install Podman."
            echo "Please install Podman or Docker manually and rerun this script."
            exit 1
        fi

        if ! command -v podman &> /dev/null; then
            echo "Error: Podman installation failed."
            exit 1
        fi

        echo "Enabling podman.socket..."
        sudo systemctl enable --now podman.socket

        SERVICE_REQUIRES="podman.socket"
        SERVICE_AFTER="network-online.target podman.socket"
        ENV_VARS="Environment=DOCKER_HOST=unix:///run/podman/podman.sock"
    else
        echo "Error: A container engine is required. Please install Podman or Docker and rerun this script."
        exit 1
    fi
fi

# --- Download & Install Xavi ---

# Determine highest semver release tag via GitHub API
echo "Fetching latest release version..."
VERSION=$(curl -s "https://api.github.com/repos/$REPO/releases" \
    | grep -oP '"tag_name":\s*"v\K[0-9]+\.[0-9]+\.[0-9]+' \
    | sort -t. -k1,1n -k2,2n -k3,3n \
    | tail -1)

if [ -z "$VERSION" ]; then
    echo "Error: Could not determine latest release version."
    exit 1
fi

echo "Latest version: v$VERSION"

DOWNLOAD_URL="https://github.com/$REPO/releases/download/v$VERSION/xavi-linux-$ARCH"
CHECKSUM_URL="https://github.com/$REPO/releases/download/v$VERSION/checksums.txt"

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
