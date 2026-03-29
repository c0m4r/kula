#!/usr/bin/env python3

"""
Example script that sends custom metrics to Kula using unix socket

applications:
  custom:
    mains:
      - name: "voltage"
        unit: "V"
        max: 300
"""

import socket
import json
import sys


def send_kula_metrics() -> None:
    """send kula metrics"""
    # The target Unix socket path
    socket_path = "/var/lib/kula/kula.sock"

    # Your data payload
    data = {"custom": {"mains": [{"voltage": 236}]}}

    try:
        # Create a Unix domain stream socket (AF_UNIX)
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
            # Connect to the Kula agent
            client.connect(socket_path)

            # Serialize to JSON and encode to bytes
            # We add a newline to mimic the behavior of 'echo'
            payload = json.dumps(data) + "\n"
            client.sendall(payload.encode("utf-8"))

            print("Successfully sent metrics to Kula.")

    except FileNotFoundError:
        print(
            f"Error: Socket not found at {socket_path}. Is Kula running?",
            file=sys.stderr,
        )
    except PermissionError:
        print(
            "Error: Permission daenied. "
            "You might need to run this as kula user.",
            file=sys.stderr,
        )


if __name__ == "__main__":
    send_kula_metrics()
