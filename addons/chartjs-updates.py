#!/usr/bin/env python3
# pylint: disable=invalid-name
"""Module for checking available updates for Chart.js and its plugins."""

import json
import urllib.request
import urllib.error
import re
import os
from typing import Optional, Dict, Any

# Configuration
# Both libraries ship in one bundle built by addons/build-chartjs.sh; its banner
# lists their versions.
BUNDLE = "internal/web/static/js/chartjs/chartjs-bundle.min.js"
LIBS: Dict[str, Dict[str, Any]] = {
    "chart.js": {
        "npm_name": "chart.js",
        "local_file": BUNDLE,
        "version_regex": r"Chart\.js v([\d\.]+)",
    },
    "chartjs-plugin-zoom": {
        "npm_name": "chartjs-plugin-zoom",
        "local_file": BUNDLE,
        "version_regex": r"chartjs-plugin-zoom v([\d\.]+)",
    },
}


def get_latest_version(npm_name: str) -> Optional[str]:
    """
    Fetch the latest version of an NPM package from the registry.

    Args:
        npm_name: The name of the package on NPM.

    Returns:
        The latest version string, or None if an error occurred.
    """
    url = f"https://registry.npmjs.org/{npm_name}/latest"
    try:
        # nosec B310: hardcoded https:// scheme, npm_name from LIBS constant (not user input)
        # nosemgrep: dynamic-urllib-use-detected -- url is a hardcoded https:// npm registry path; npm_name is from the LIBS constant
        with urllib.request.urlopen(url) as response:  # nosec B310
            data = json.loads(response.read().decode())
            version = data.get("version")
            return version if isinstance(version, str) else None
    except (urllib.error.URLError, json.JSONDecodeError, AttributeError) as e:
        print(f"Error fetching latest version for {npm_name}: {e}")
        return None


def get_local_version(file_path: str, regex: Optional[str]) -> str:
    """
    Read the local version from a file using a regex pattern.

    Args:
        file_path: Path to the local file.
        regex: Regex pattern with one group to extract the version.

    Returns:
        The version string, 'Not found' if the file is missing,
        or 'Unknown' if the version could not be parsed.
    """
    if not os.path.exists(file_path):
        return "Not found"

    if not regex:
        return "Unknown"

    try:
        with open(file_path, "r", encoding="utf-8") as f:
            # Read first few KB to find version
            content = f.read(4096)
            match = re.search(regex, content)
            if match:
                return match.group(1)
    except OSError as e:
        print(f"Error reading local file {file_path}: {e}")

    return "Unknown"


def main() -> None:
    """Check for updates for all configured libraries and print a report."""
    print(f"{'Library':<30} {'Local':<10} {'Latest':<10} {'Status':<18} {'URL'}")
    print("-" * 115)

    all_up_to_date = True

    for info in LIBS.values():
        npm_name = info["npm_name"]
        local_file = info["local_file"]
        version_regex = info.get("version_regex")

        latest = get_latest_version(npm_name)
        local = get_local_version(local_file, version_regex)

        status = "OK"
        if latest and local != "Unknown" and local != "Not found":
            if local != latest:
                status = "UPDATE AVAILABLE"
                all_up_to_date = False
        elif local == "Not found":
            status = "MISSING"
            all_up_to_date = False

        url = f"https://www.npmjs.com/package/{npm_name}"
        print(f"{npm_name:<30} {local:<10} {latest:<10} {status:<18} {url}")

    if all_up_to_date:
        print("\nAll Chart.js libraries are up to date.")
    else:
        print("\nAction required: Some libraries have updates available.")
        print("Run ./addons/build-chartjs.sh --update to rebuild the bundle.")


if __name__ == "__main__":
    main()
