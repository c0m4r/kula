#!/usr/bin/env bash

set -e

# Everything runs from main, called on the last line: a download cut short
# (bash -c "$(curl ...)" runs whatever arrived) defines nothing and runs
# nothing, instead of running the commands that made it through.
main() {

GITHUB_REPO="c0m4r/kula"

# Define colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

# ---------------------------------------------------------------------------
# Options
# ---------------------------------------------------------------------------
SKIP_VERIFY=0
ASSUME_YES=0

usage() {
    cat <<USAGE
kula installer

Usage: install_v3.sh [options]

Options:
  -y, --yes          Non-interactive: assume "yes" to all prompts.
                     Downloads are still verified against the release checksums.
      --skip-verify  Do not verify SHA-256 checksums of downloaded files.
  -h, --help         Show this help and exit.

By default every downloaded file is verified against CHECKSUMS.sha256.txt from
the matching GitHub release. Combine --yes --skip-verify for a fully unattended
install (not recommended).
USAGE
}

while [ $# -gt 0 ]; do
    case "$1" in
        -y|--yes) ASSUME_YES=1 ;;
        --skip-verify) SKIP_VERIFY=1 ;;
        -h|--help) usage; exit 0 ;;
        *)
            echo -e "${RED}Unknown option: $1${NC}" >&2
            usage >&2
            exit 1
            ;;
    esac
    shift
done

# Prompt for confirmation; auto-confirms under --yes. Reads from the terminal so
# it works even when the script is piped (e.g. curl ... | bash).
confirm() {
    local prompt="$1" reply
    if [ "$ASSUME_YES" -eq 1 ]; then
        return 0
    fi
    read -rp "$prompt [y/N] " -n 1 reply < /dev/tty
    echo
    [[ "$reply" =~ ^[Yy]$ ]]
}

# Download a URL to a file with curl or wget; non-zero on any failure.
fetch() {
    local url="$1" out="$2"
    if command -v curl >/dev/null; then
        curl -fsSL "$url" -o "$out"
    elif command -v wget >/dev/null; then
        wget -qO "$out" "$url"
    else
        echo -e "${RED}Error: Neither curl nor wget is installed.${NC}" >&2
        return 1
    fi
}

# ---------------------------------------------------------------------------
# Resolve latest release version
# ---------------------------------------------------------------------------
if command -v curl >/dev/null; then
    KULA_VERSION=$(curl -sI "https://github.com/${GITHUB_REPO}/releases/latest" | grep -i 'location:' | sed -E 's|.*/tag/([^[:space:]]+).*|\1|' | tail -n1 | tr -d '\r')
elif command -v wget >/dev/null; then
    KULA_VERSION=$(wget --server-response --max-redirect=0 "https://github.com/${GITHUB_REPO}/releases/latest" 2>&1 | grep -i 'location:' | sed -E 's|.*/tag/([^[:space:]]+).*|\1|' | tail -n1 | tr -d '\r')
else
    echo -e "${RED}Error: Neither curl nor wget is installed.${NC}"
    exit 1
fi

if [ -z "$KULA_VERSION" ]; then
    echo -e "${RED}Error: Failed to fetch the latest version.${NC}"
    exit 1
fi

if [[ ! "$KULA_VERSION" =~ ^[a-zA-Z0-9.-]+$ ]]; then
    echo -e "${RED}Error: Invalid version format received: $KULA_VERSION${NC}"
    exit 1
fi

# Secure Temp Directory allocation
SECURE_TMP=$(mktemp -d /tmp/kula-install-XXXXXX)
trap 'rm -rf "$SECURE_TMP"' EXIT

RELEASE_URL="https://github.com/${GITHUB_REPO}/releases/download/${KULA_VERSION}"
CHECKSUMS_FILE="$SECURE_TMP/CHECKSUMS.sha256.txt"

echo -e "${CYAN}===========================================${NC}"
echo -e "${CYAN}      kula - system monitoring daemon      ${NC}"
echo -e "${CYAN}===========================================${NC}"
echo -e "Version: ${KULA_VERSION}"
if [ "$SKIP_VERIFY" -eq 1 ]; then
    echo -e "Verification: ${YELLOW}disabled (--skip-verify)${NC}"
else
    echo -e "Verification: ${GREEN}enabled${NC}"
fi
echo ""

# Detect Architecture
ARCH=$(uname -m)
case "$ARCH" in
    x86_64) HOST_ARCH="amd64" ;;
    aarch64) HOST_ARCH="arm64" ;;
    riscv64) HOST_ARCH="riscv64" ;;
    *)
        echo -e "${RED}Error: Unsupported architecture $ARCH${NC}"
        exit 1
        ;;
esac
echo -e "Detected Architecture: ${GREEN}${HOST_ARCH}${NC}"

# Detect OS
OS_FAMILY="unknown"
if [ -f /etc/os-release ]; then
    . /etc/os-release
    OS_ID=${ID}
    OS_LIKE=${ID_LIKE:-""}

    if [[ "$OS_ID" == "debian" || "$OS_ID" == "ubuntu" || "$OS_LIKE" == *"debian"* || "$OS_LIKE" == *"ubuntu"* ]]; then
        OS_FAMILY="debian"
    elif [[ "$OS_ID" == "arch" || "$OS_ID" == "manjaro" || "$OS_LIKE" == *"arch"* ]]; then
        OS_FAMILY="arch"
    elif [[ "$OS_ID" == "fedora" || "$OS_ID" == "rhel" || "$OS_ID" == "rocky" || "$OS_ID" == "alma" || "$OS_LIKE" == *"fedora"* || "$OS_LIKE" == *"rhel"* ]]; then
        OS_FAMILY="rpm"
    elif [[ "$OS_ID" == "alpine" ]]; then
        OS_FAMILY="alpine"
    elif [[ "$OS_ID" == "void" ]]; then
        OS_FAMILY="void"
    fi
fi
echo -e "Detected OS Family: ${GREEN}${OS_FAMILY}${NC}"

# Detect Init System
INIT_SYSTEM="unknown"
if command -v systemctl >/dev/null 2>&1 && systemctl --no-pager >/dev/null 2>&1 || [ -d /run/systemd/system ]; then
    INIT_SYSTEM="systemd"
fi
echo -e "Detected Init System: ${GREEN}${INIT_SYSTEM}${NC}"

# ---------------------------------------------------------------------------
# Fetch checksums (required unless --skip-verify)
# ---------------------------------------------------------------------------
if [ "$SKIP_VERIFY" -eq 0 ]; then
    if ! command -v sha256sum >/dev/null; then
        echo -e "${RED}Error: sha256sum is required for verification but was not found.${NC}"
        echo -e "Install coreutils, or re-run with ${YELLOW}--skip-verify${NC} to bypass (not recommended)."
        exit 1
    fi
    echo -e "${BLUE}Fetching checksums...${NC}"
    if ! fetch "${RELEASE_URL}/CHECKSUMS.sha256.txt" "$CHECKSUMS_FILE" || [ ! -s "$CHECKSUMS_FILE" ]; then
        echo -e "${RED}Error: could not fetch CHECKSUMS.sha256.txt for ${KULA_VERSION}.${NC}"
        echo -e "The release may predate published checksums, or the network failed."
        echo -e "Re-run with ${YELLOW}--skip-verify${NC} to install without verification (not recommended)."
        exit 1
    fi
    echo -e "${GREEN}Checksums loaded.${NC}"
else
    echo -e "${YELLOW}Warning: checksum verification is disabled (--skip-verify).${NC}"
fi

# Download a release asset and verify it against the release checksums.
# Prints the path to the verified file on stdout; all messages go to stderr.
download_and_verify() {
    local filename=$1
    local target="$SECURE_TMP/$filename"
    local url="${RELEASE_URL}/${filename}"

    echo -e "${BLUE}Downloading ${filename}...${NC}" >&2
    if ! fetch "$url" "$target" || [ ! -s "$target" ]; then
        echo -e "${RED}Error: download failed or file is empty (${url})${NC}" >&2
        rm -f "$target"
        exit 1
    fi

    if [ "$SKIP_VERIFY" -eq 1 ]; then
        echo -e "${YELLOW}Skipping checksum verification for ${filename}.${NC}" >&2
        if command -v sha256sum >/dev/null; then
            echo -e "${CYAN}  sha256: $(sha256sum "$target" | awk '{print $1}')${NC}" >&2
        fi
        echo "$target"
        return 0
    fi

    local expected actual
    expected=$(awk -v f="$filename" '$2 == f {print $1}' "$CHECKSUMS_FILE")
    if [ -z "$expected" ]; then
        echo -e "${RED}Error: ${filename} is not listed in CHECKSUMS.sha256.txt; refusing to install.${NC}" >&2
        rm -f "$target"
        exit 1
    fi
    actual=$(sha256sum "$target" | awk '{print $1}')
    if [ "$expected" != "$actual" ]; then
        echo -e "${RED}Error: checksum mismatch for ${filename}!${NC}" >&2
        echo -e "${RED}  expected: ${expected}${NC}" >&2
        echo -e "${RED}  actual:   ${actual}${NC}" >&2
        rm -f "$target"
        exit 1
    fi
    echo -e "${GREEN}Verified ${filename} (sha256 OK).${NC}" >&2
    echo -e "${CYAN}  sha256: ${actual}${NC}" >&2

    echo "$target"
}

# Determine action
INSTALL_METHOD=""

if [ "$OS_FAMILY" == "debian" ]; then
    INSTALL_METHOD="deb"
elif [ "$OS_FAMILY" == "rpm" ]; then
    INSTALL_METHOD="rpm"
elif [ "$OS_FAMILY" == "arch" ] && command -v pacman >/dev/null; then
    INSTALL_METHOD="aur"
elif [ "$OS_FAMILY" == "alpine" ]; then
    INSTALL_METHOD="alpine"
elif [ "$OS_FAMILY" == "void" ]; then
    INSTALL_METHOD="void"
else
    # Fallback options
    if [ "$INIT_SYSTEM" == "systemd" ]; then
        INSTALL_METHOD="tarball_systemd"
    elif command -v docker >/dev/null; then
        INSTALL_METHOD="docker"
    else
        INSTALL_METHOD="tarball_opt"
    fi
fi

echo -e "\nProposed installation method: ${YELLOW}${INSTALL_METHOD}${NC}"
if ! confirm "Do you want to continue with this installation method?"; then
    echo -e "${RED}Installation aborted.${NC}"
    exit 1
fi

exec_as_root() {
    if [ "$(id -u)" -eq 0 ]; then
        "$@"
    elif command -v sudo >/dev/null; then
        sudo "$@"
    elif command -v doas >/dev/null; then
        doas "$@"
    elif command -v su >/dev/null; then
        su -c "$*"
    else
        echo -e "${YELLOW}Warning: You are not root and sudo is not available. Installation may fail.${NC}"
        "$@"
    fi
}

# Install /etc/kula from the extracted tarball (run from its kula/ directory,
# after the kula group exists). The config holds the password hash, tokens and
# database passwords: root owns it and the daemon only reads it, through its
# group. An existing config keeps its content, not an older installer's modes.
# Older installers gave /etc/kula to the daemon, so once the directory is
# root's (nothing can swap its entries after that), replace the example rather
# than write through a link left there, and tighten an existing config only if
# it is a plain file with one link.
install_config() {
    local cfg=/etc/kula/config.yaml
    exec_as_root mkdir -p /etc/kula
    exec_as_root chown root:kula /etc/kula
    exec_as_root chmod 0750 /etc/kula
    exec_as_root rm -f /etc/kula/config.example.yaml
    exec_as_root install -m 0640 -o root -g kula config.example.yaml /etc/kula/config.example.yaml
    if ! exec_as_root test -e "$cfg" && ! exec_as_root test -L "$cfg"; then
        exec_as_root install -m 0640 -o root -g kula config.example.yaml "$cfg"
    elif exec_as_root test -f "$cfg" && ! exec_as_root test -L "$cfg" \
        && [ "$(exec_as_root stat -c %h "$cfg")" = 1 ]; then
        exec_as_root chown root:kula "$cfg"
        exec_as_root chmod 0640 "$cfg"
    else
        echo -e "${YELLOW}Warning: ${cfg} is a link or not a plain file; its owner and mode are left as they are.${NC}"
    fi
}

echo ""

# Execute installation
if [ "$INSTALL_METHOD" == "deb" ]; then
    filename="kula-${KULA_VERSION}-${HOST_ARCH}.deb"
    target=$(download_and_verify "$filename")
    echo -e "${BLUE}Installing Debian package...${NC}"
    exec_as_root dpkg -i "$target" || exec_as_root apt-get install -f -y "$target"
    rm -f "$target"
    echo -e "${GREEN}Installation successful!${NC}"

elif [ "$INSTALL_METHOD" == "rpm" ]; then
    RPM_ARCH=$HOST_ARCH
    if [ "$HOST_ARCH" == "amd64" ]; then RPM_ARCH="x86_64"; fi
    if [ "$HOST_ARCH" == "arm64" ]; then RPM_ARCH="aarch64"; fi

    filename="kula-${KULA_VERSION}-${RPM_ARCH}.rpm"
    target=$(download_and_verify "$filename")
    echo -e "${BLUE}Installing RPM package...${NC}"
    if command -v dnf >/dev/null; then
        exec_as_root dnf install -y "$target"
    elif command -v yum >/dev/null; then
        exec_as_root yum install -y "$target"
    else
        exec_as_root rpm -ivh "$target"
    fi
    rm -f "$target"
    echo -e "${GREEN}Installation successful!${NC}"

elif [ "$INSTALL_METHOD" == "aur" ]; then
    filename="kula-${KULA_VERSION}-aur.tar.gz"
    # For makepkg, we should NOT be root.
    if [ "$(id -u)" -eq 0 ]; then
        echo -e "${RED}Error: AUR installation should not be run as root.${NC}"
        echo -e "Please run this script as a normal user with sudo privileges."
        exit 1
    fi
    target=$(download_and_verify "$filename")

    echo -e "${BLUE}Extracting and building AUR package...${NC}"
    build_dir="$SECURE_TMP/kula-aur-build"
    mkdir -p "$build_dir"
    tar -xzf "$target" -C "$build_dir"

    cd "$build_dir/kula-${KULA_VERSION}-aur"
    if [ "$ASSUME_YES" -eq 1 ]; then
        makepkg -si --noconfirm
    else
        makepkg -si
    fi
    cd - >/dev/null
    rm -f "$target"
    echo -e "${GREEN}Installation successful!${NC}"

elif [ "$INSTALL_METHOD" == "tarball_systemd" ]; then
    filename="kula-${KULA_VERSION}-${HOST_ARCH}.tar.gz"
    target=$(download_and_verify "$filename")

    echo -e "${BLUE}Installing from tarball to system directories...${NC}"
    extract_dir="$SECURE_TMP/kula_extract"
    mkdir -p "$extract_dir"
    tar -xzf "$target" -C "$extract_dir"

    cd "$extract_dir/kula"
    if ! getent group kula >/dev/null; then
        exec_as_root groupadd --system kula
    fi
    if ! getent passwd kula >/dev/null; then
        exec_as_root useradd --system -g kula -d /var/lib/kula -s /bin/false -c "Kula System Monitoring Daemon" kula
    fi

    exec_as_root install -Dm755 kula /usr/bin/kula
    exec_as_root install -Dm644 init/systemd/kula.service /etc/systemd/system/kula.service
    install_config
    exec_as_root install -dm750 /var/lib/kula
    exec_as_root chown -R kula:kula /var/lib/kula
    exec_as_root install -dm750 /usr/share/kula
    exec_as_root cp -r CHANGELOG.md README.md LICENSE VERSION config.example.yaml /usr/share/kula/

    echo -e "${BLUE}Reloading systemd and enabling service...${NC}"
    exec_as_root systemctl daemon-reload
    exec_as_root systemctl enable kula.service
    exec_as_root systemctl start kula.service

    cd - >/dev/null
    rm -f "$target"
    echo -e "${GREEN}Installation successful!${NC}"

elif [ "$INSTALL_METHOD" == "alpine" ]; then
    filename="kula-${KULA_VERSION}-${HOST_ARCH}.tar.gz"
    target=$(download_and_verify "$filename")

    echo -e "${BLUE}Installing on Alpine Linux...${NC}"
    extract_dir="$SECURE_TMP/kula_extract"
    mkdir -p "$extract_dir"
    tar -xzf "$target" -C "$extract_dir"

    cd "$extract_dir/kula"

    if ! getent group kula >/dev/null 2>&1; then
        exec_as_root addgroup kula
    fi
    if ! getent passwd kula >/dev/null 2>&1; then
        exec_as_root adduser -S -D -H -h /var/lib/kula -s /sbin/nologin -G kula -g "Kula Monitoring Daemon" kula
    fi

    exec_as_root install -Dm755 kula /usr/bin/kula
    exec_as_root install -Dm755 addons/init/openrc/kula /etc/init.d/kula
    install_config
    exec_as_root install -dm750 /var/lib/kula
    exec_as_root chown -R kula:kula /var/lib/kula
    exec_as_root install -dm750 /usr/share/kula
    exec_as_root cp -r CHANGELOG.md README.md LICENSE VERSION config.example.yaml /usr/share/kula/

    echo -e "${BLUE}Enabling and starting service...${NC}"
    exec_as_root rc-update add kula default
    exec_as_root rc-service kula start

    cd - >/dev/null
    rm -f "$target"
    echo -e "${GREEN}Installation successful!${NC}"

elif [ "$INSTALL_METHOD" == "void" ]; then
    filename="kula-${KULA_VERSION}-${HOST_ARCH}.tar.gz"
    target=$(download_and_verify "$filename")

    echo -e "${BLUE}Installing on Void Linux...${NC}"
    extract_dir="$SECURE_TMP/kula_extract"
    mkdir -p "$extract_dir"
    tar -xzf "$target" -C "$extract_dir"

    cd "$extract_dir/kula"

    if ! getent group kula >/dev/null 2>&1; then
        exec_as_root groupadd --system kula
    fi
    if ! getent passwd kula >/dev/null 2>&1; then
        exec_as_root useradd --system -g kula -d /var/lib/kula -s /bin/false -c "Kula Monitoring Daemon" kula
    fi

    exec_as_root install -Dm755 kula /usr/bin/kula
    install_config
    exec_as_root install -dm750 /var/lib/kula
    exec_as_root chown -R kula:kula /var/lib/kula
    exec_as_root install -dm750 /usr/share/kula
    exec_as_root cp -r CHANGELOG.md README.md LICENSE VERSION config.example.yaml /usr/share/kula/

    exec_as_root cp -r addons/init/runit/kula /etc/sv/
    exec_as_root chmod +x /etc/sv/kula/run
    if [ -f /etc/sv/kula/log/run ]; then
        exec_as_root chmod +x /etc/sv/kula/log/run
    fi

    echo -e "${BLUE}Enabling and starting service...${NC}"
    exec_as_root ln -sf /etc/sv/kula /var/service/
    exec_as_root sv up kula || echo -e "${YELLOW}Notice: Could not start service 'kula'. You might need to start it manually.${NC}"

    cd - >/dev/null
    rm -f "$target"
    echo -e "${GREEN}Installation successful!${NC}"

elif [ "$INSTALL_METHOD" == "docker" ]; then
    echo -e "${BLUE}Docker is installed. You can run Kula via Docker container.${NC}"
    echo -e "Run the following command to start Kula:"
    echo -e "${CYAN}docker run -d --name kula --pid host --network host -v /proc:/proc:ro -v kula_data:/app/data c0m4r/kula:latest${NC}"
    echo ""
    echo -e "To persist configuration, use volume mounts and provide your config.yaml."
    echo -e "You can find more at https://hub.docker.com/r/c0m4r/kula"
    echo ""
    exit 0
elif [ "$INSTALL_METHOD" == "tarball_opt" ]; then
    filename="kula-${KULA_VERSION}-${HOST_ARCH}.tar.gz"
    target=$(download_and_verify "$filename")

    echo -e "${BLUE}Installing to /opt/kula...${NC}"
    if [ ! -d "/opt/kula" ]; then
        exec_as_root mkdir -p /opt/kula
    fi
    # Root extracts: the files belong to root whatever the archive records.
    exec_as_root tar --no-same-owner --no-same-permissions -xzf "$target" -C /opt

    rm -f "$target"
    echo -e "${GREEN}Extracted to /opt/kula successfully.${NC}"
    echo -e "To run Kula manually:"
    echo -e "${CYAN}  cd /opt/kula${NC}"
    echo -e "${CYAN}  cp config.example.yaml config.yaml${NC}"
    echo -e "${CYAN}  ./kula serve${NC}"
fi

echo -e "\n${GREEN}Thank you for installing Kula!${NC}"
}

main "$@"
