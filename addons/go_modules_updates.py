#!/usr/bin/env python3
"""
Check for Go module updates using `go list -m -u -json all`.

Go has no release-age window like npm's min-release-age, so each update is
listed with its release date, and one younger than MIN_RELEASE_AGE_DAYS is
marked to wait: a hijacked release is usually caught within days.
"""

import subprocess
import os
import sys
import json
from datetime import datetime, timezone
from typing import Any

MIN_RELEASE_AGE_DAYS = 14


def release_age_days(stamp: str) -> float | None:
    """Days since an RFC 3339 module time, or None when it cannot be read."""
    try:
        released = datetime.fromisoformat(stamp.replace("Z", "+00:00"))
    except (AttributeError, ValueError):
        return None
    if released.tzinfo is None:
        return None
    return (datetime.now(timezone.utc) - released).total_seconds() / 86400


def module_objects(stream: str) -> list[dict[str, Any]]:
    """The JSON objects `go list -json` prints one after another."""
    decoder = json.JSONDecoder()
    objects: list[dict[str, Any]] = []
    index = 0
    while True:
        while index < len(stream) and stream[index].isspace():
            index += 1
        if index >= len(stream):
            return objects
        value, index = decoder.raw_decode(stream, index)
        objects.append(value)


def check_go_updates() -> None:
    """
    Runs the Go module checker to find outdated dependencies.
    Parses the output of 'go list' and prints available updates.
    """
    try:
        # Run the go list command to check for updates
        # Ensure we run this from the project root (where go.mod is located)
        script_dir = os.path.dirname(os.path.realpath(__file__))
        project_root = os.path.join(script_dir, "..")

        # Check if go is installed
        try:
            subprocess.run(
                ["go", "version"],
                check=True,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
        except (subprocess.CalledProcessError, FileNotFoundError):
            print("Error: 'go' command is not found or failed to run.", file=sys.stderr)
            sys.exit(1)

        print("Checking for Go module updates...")

        # Parse go.mod to find which modules are actually used/tracked by this project
        mod_json_run = subprocess.run(
            ["go", "mod", "edit", "-json"],
            cwd=project_root,
            capture_output=True,
            text=True,
            check=True,
        )
        mod_data = json.loads(mod_json_run.stdout)
        tracked_modules = {req["Path"] for req in mod_data.get("Require", [])}

        result = subprocess.run(
            ["go", "list", "-m", "-u", "-json", "all"],
            cwd=project_root,
            capture_output=True,
            text=True,
            check=True,
        )

        ready = []
        waiting = 0

        for module in module_objects(result.stdout):
            update = module.get("Update")
            module_name = module.get("Path")
            if not update or module_name not in tracked_modules:
                continue

            current_version = module.get("Version", "?")
            latest_version = update.get("Version", "?")
            age = release_age_days(update.get("Time", ""))
            if age is None:
                released = "release date unknown"
            else:
                released = f"released {update['Time'][:10]}, {int(age)} days ago"
            line = f"Update available: {module_name} ({current_version} -> {latest_version}, {released})"
            if age is None or age < MIN_RELEASE_AGE_DAYS:
                line += f" - wait until it is {MIN_RELEASE_AGE_DAYS} days old"
                waiting += 1
            else:
                ready.append(f"{module_name}@{latest_version}")
            print(line)

        if not ready and not waiting:
            print("All Go modules are up-to-date!")
        elif not ready:
            print(f"\nNo update is {MIN_RELEASE_AGE_DAYS} days old yet.")
        else:
            print(
                f"\nTo update the modules released at least {MIN_RELEASE_AGE_DAYS} days ago, run:"
            )
            print(f"  go get {' '.join(ready)}")
            print("  go mod tidy")
            print(
                "Check the versions they pull in (git diff go.mod); avoid go get -u, which"
            )
            print("takes the newest release of every dependency, however recent.")
    except subprocess.CalledProcessError as e:
        print(f"Error checking for updates: {e}", file=sys.stderr)
        if e.stderr:
            print(e.stderr, file=sys.stderr)
        sys.exit(1)
    except OSError as e:
        print(f"OS error occurred: {e}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    check_go_updates()
