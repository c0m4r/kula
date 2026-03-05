#!/usr/bin/env python3
"""
Check for Go module updates using `go list -m -u all`.
"""

import subprocess
import os
import sys
import json


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
            ["go", "list", "-m", "-u", "all"],
            cwd=project_root,
            capture_output=True,
            text=True,
            check=True,
        )

        updates_found = False

        # Parse the output
        for line in result.stdout.splitlines():
            line = line.strip()
            if not line:
                continue

            parts = line.split()
            # Format usually is: module current_version [latest_version]
            # e.g., github.com/gin-gonic/gin v1.8.1 [v1.9.1]
            if (
                len(parts) >= 3
                and parts[-1].startswith("[")
                and parts[-1].endswith("]")
            ):
                module_name = parts[0]
                if module_name not in tracked_modules:
                    continue

                current_version = parts[1]
                latest_version = parts[-1][1:-1]  # strip brackets

                print(
                    f"Update available: {module_name} ({current_version} -> {latest_version})"
                )
                updates_found = True

        if not updates_found:
            print("All Go modules are up-to-date!")
        else:
            print("\nTo update modules, run:")
            print("  go get -u all")
            print("  go mod tidy")
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
